package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
)

// Agent-facing Feishu tools (`multica lark ...`). Every endpoint accepts only
// a task token: the token pins the workspace, agent and task, and the tools
// act through that agent's own Feishu bot. The task's Feishu conversation,
// when there is one, is the default target.

// maxLarkSendBytes bounds one send request: Lark caps a message file at
// 30 MiB, and a request may carry a few files plus form overhead.
const maxLarkSendBytes = 100 << 20

func (h *Handler) larkToolScope(w http.ResponseWriter, r *http.Request) (lark.ToolScope, bool) {
	if h.LarkTools == nil {
		writeError(w, http.StatusServiceUnavailable, "Feishu integration is not configured on this server")
		return lark.ToolScope{}, false
	}
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "Feishu tools are only available from within an agent task")
		return lark.ToolScope{}, false
	}
	ws, errWS := util.ParseUUID(r.Header.Get("X-Workspace-ID"))
	agent, errAgent := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	task, errTask := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if errWS != nil || errAgent != nil || errTask != nil {
		writeError(w, http.StatusBadRequest, "missing task context")
		return lark.ToolScope{}, false
	}
	scope := lark.ToolScope{WorkspaceID: ws, AgentID: agent, TaskID: task}
	return scope, true
}

// writeLarkToolError maps tool errors onto HTTP statuses. Lark business
// errors (permissions, unknown chat, …) are the caller's to fix, so they
// surface as 400 with Lark's own message.
func writeLarkToolError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *lark.APIError
	switch {
	case errors.Is(err, lark.ErrToolNoInstallation):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, lark.ErrToolNotFound):
		writeError(w, http.StatusNotFound, "scheduled message not found or no longer pending")
	case errors.Is(err, lark.ErrToolNoTarget), errors.Is(err, lark.ErrToolInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &apiErr):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Feishu rejected the request (code %d): %s%s",
			apiErr.Code, apiErr.Msg, larkPermissionHint(apiErr.Code)))
	default:
		slog.Error("lark tool failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadGateway, "Feishu request failed: "+err.Error())
	}
}

// larkPermissionHint adds the fix for Lark's "app lacks a scope" and "no
// access to this resource" codes, which are the common failures of a new tool.
func larkPermissionHint(code int) string {
	switch code {
	case 99991672, 99991679:
		return " (the Feishu app is missing an API permission: add it in the Feishu Open Platform console and publish a new app version)"
	case 1770032, 91204, 91403, 131006:
		return " (the bot cannot access this document: share it with the bot, or grant the app tenant-wide read access)"
	}
	return ""
}

// GetLarkContext serves `multica lark context`.
func (h *Handler) GetLarkContext(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	info, err := h.LarkTools.Context(r.Context(), scope)
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// LarkSendResponse is the send response: message ids for an immediate send,
// the stored row for a scheduled one.
type LarkSendResponse struct {
	MessageIDs []string               `json:"message_ids,omitempty"`
	Scheduled  *lark.ScheduledMessage `json:"scheduled,omitempty"`
}

// SendLarkMessage serves `multica lark send`. The body is multipart: text,
// chat_id / open_id, repeated mention, mention_requester, repeated file, and
// either delay (a Go duration such as "2m") or send_at (RFC 3339) to schedule
// the message instead of sending it now.
func (h *Handler) SendLarkMessage(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLarkSendBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "request too large or not a multipart form")
		return
	}
	defer r.MultipartForm.RemoveAll()
	form := r.MultipartForm.Value
	first := func(k string) string {
		if v := form[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	target := lark.ToolTarget{ChatID: first("chat_id"), OpenID: first("open_id")}
	mentionRequester := first("mention_requester") == "true"

	fireAt, scheduled, err := larkSendTime(first("delay"), first("send_at"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if scheduled {
		if len(r.MultipartForm.File["file"]) > 0 {
			writeError(w, http.StatusBadRequest, "files cannot be scheduled; send them now or schedule a text message")
			return
		}
		row, err := h.LarkTools.Schedule(r.Context(), scope, lark.ScheduleInput{
			Target: target, Text: first("text"), MentionOpenIDs: form["mention"],
			MentionRequester: mentionRequester, FireAt: fireAt,
		})
		if err != nil {
			writeLarkToolError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, LarkSendResponse{Scheduled: &row})
		return
	}

	var files []lark.ToolFile
	for _, fh := range r.MultipartForm.File["file"] {
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read file "+fh.Filename)
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read file "+fh.Filename)
			return
		}
		files = append(files, lark.ToolFile{
			Name:        path.Base(fh.Filename),
			ContentType: larkFileContentType(fh.Filename, data),
			Data:        data,
		})
	}
	res, err := h.LarkTools.Send(r.Context(), scope, lark.SendInput{
		Target: target, Text: first("text"), MentionOpenIDs: form["mention"],
		MentionRequester: mentionRequester, Files: files,
	})
	if err != nil {
		if len(res.MessageIDs) > 0 {
			// Partial delivery: say what already went out so the agent does
			// not resend it.
			writeError(w, http.StatusBadGateway, fmt.Sprintf("sent %d message(s) (%s) before failing: %v",
				len(res.MessageIDs), strings.Join(res.MessageIDs, ", "), err))
			return
		}
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, LarkSendResponse{MessageIDs: res.MessageIDs})
}

// larkSendTime resolves the optional delay / send_at pair into a fire time.
// Neither means "send now".
func larkSendTime(delay, sendAt string, now time.Time) (time.Time, bool, error) {
	switch {
	case delay != "" && sendAt != "":
		return time.Time{}, false, errors.New("pass either delay or send_at, not both")
	case delay != "":
		d, err := time.ParseDuration(delay)
		if err != nil || d <= 0 {
			return time.Time{}, false, fmt.Errorf("invalid delay %q: use a duration such as 90s, 2m or 1h30m", delay)
		}
		return now.Add(d), true, nil
	case sendAt != "":
		t, err := time.Parse(time.RFC3339, sendAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("invalid send_at %q: use RFC 3339 with a timezone, e.g. 2026-09-26T09:00:00+08:00", sendAt)
		}
		return t, true, nil
	}
	return time.Time{}, false, nil
}

// larkFileContentType sniffs a file's type, preferring the extension table
// the attachment upload uses when it knows the extension.
func larkFileContentType(filename string, data []byte) string {
	if ct, ok := extContentTypes[strings.ToLower(path.Ext(filename))]; ok {
		return ct
	}
	return http.DetectContentType(data)
}

// ListLarkScheduled serves `multica lark scheduled list`.
func (h *Handler) ListLarkScheduled(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	rows, err := h.LarkTools.ListScheduled(r.Context(), scope)
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scheduled": rows})
}

// CancelLarkScheduled serves `multica lark scheduled cancel <id>`.
func (h *Handler) CancelLarkScheduled(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !ok {
		return
	}
	row, err := h.LarkTools.CancelScheduled(r.Context(), scope, id)
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// ReadLarkDoc serves `multica lark doc <url>`.
func (h *Handler) ReadLarkDoc(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	doc, err := h.LarkTools.ReadDoc(r.Context(), scope, r.URL.Query().Get("url"))
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// ListLarkChats serves `multica lark chats`.
func (h *Handler) ListLarkChats(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	chats, err := h.LarkTools.ListChats(r.Context(), scope)
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

// ListLarkChatMembers serves `multica lark members`.
func (h *Handler) ListLarkChatMembers(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	members, err := h.LarkTools.ListMembers(r.Context(), scope, strings.TrimSpace(r.URL.Query().Get("chat_id")))
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// CreateLarkGroupRequest is the `multica lark group create` body.
type CreateLarkGroupRequest struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Members          []string `json:"members"`
	IncludeRequester bool     `json:"include_requester"`
}

// CreateLarkGroup serves `multica lark group create`.
func (h *Handler) CreateLarkGroup(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	var req CreateLarkGroupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	chat, err := h.LarkTools.CreateGroup(r.Context(), scope, lark.CreateGroupInput{
		Name: req.Name, Description: req.Description, Members: req.Members, IncludeRequester: req.IncludeRequester,
	})
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, chat)
}

// AddLarkGroupMembersRequest is the `multica lark group add` body.
type AddLarkGroupMembersRequest struct {
	ChatID  string   `json:"chat_id"`
	Members []string `json:"members"`
}

// AddLarkGroupMembers serves `multica lark group add`.
func (h *Handler) AddLarkGroupMembers(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	var req AddLarkGroupMembersRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	invalid, err := h.LarkTools.AddMembers(r.Context(), scope, strings.TrimSpace(req.ChatID), req.Members)
	if err != nil {
		writeLarkToolError(w, r, err)
		return
	}
	if invalid == nil {
		invalid = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invalid_members": invalid})
}
