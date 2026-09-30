package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Cross-workspace delegation (`multica lark delegate`). A task token is bound
// to its own workspace, so an agent cannot reach agents in another one. The
// person behind the task often belongs to several workspaces, though; this
// endpoint lets the agent hand work to an agent in any of them, acting as
// that person — exactly what they could do by hand in the web app.

// LarkDelegateRequest is the body of POST /api/lark/delegations.
type LarkDelegateRequest struct {
	// To is the target agent's name or ID.
	To string `json:"to"`
	// Workspace is the target workspace's slug, name or ID. Empty searches
	// every workspace the person belongs to.
	Workspace    string `json:"workspace"`
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
}

type LarkDelegateResponse struct {
	IssueID       string `json:"issue_id"`
	Identifier    string `json:"identifier"`
	Workspace     string `json:"workspace"`
	AssigneeID    string `json:"assignee_id"`
	AssigneeName  string `json:"assignee_name"`
	RelayedToChat string `json:"relayed_to_chat,omitempty"`
	RelayError    string `json:"relay_error,omitempty"`
}

type delegateCandidate struct {
	ws    db.Workspace
	agent db.Agent
}

// DelegateLarkWork serves POST /api/lark/delegations.
func (h *Handler) DelegateLarkWork(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.larkToolScope(w, r)
	if !ok {
		return
	}
	var req LarkDelegateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	to := strings.TrimSpace(req.To)
	instructions := strings.TrimSpace(req.Instructions)
	if to == "" || instructions == "" {
		writeError(w, http.StatusBadRequest, "to and instructions are required")
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), scope.TaskID)
	if err != nil || !task.OriginatorUserID.Valid {
		writeError(w, http.StatusBadRequest, "this task has no person behind it to delegate as")
		return
	}
	person := task.OriginatorUserID
	personID := uuidToString(person)

	workspaces, err := h.Queries.ListWorkspaces(r.Context(), person)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workspaces")
		return
	}
	want := strings.ToLower(strings.TrimSpace(req.Workspace))
	var candidates []delegateCandidate
	var available []string
	for _, ws := range workspaces {
		if want != "" && want != strings.ToLower(ws.Slug) && want != strings.ToLower(ws.Name) && want != uuidToString(ws.ID) {
			continue
		}
		agents, err := h.Queries.ListAgents(r.Context(), ws.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list agents")
			return
		}
		for _, a := range agents {
			if a.ID == scope.AgentID {
				continue
			}
			if !h.canInvokeAgent(r.Context(), a, "member", personID, personID, uuidToString(ws.ID)) {
				continue
			}
			available = append(available, fmt.Sprintf("%s (workspace %s)", a.Name, ws.Slug))
			if strings.EqualFold(a.Name, to) || uuidToString(a.ID) == to {
				candidates = append(candidates, delegateCandidate{ws: ws, agent: a})
			}
		}
	}
	switch {
	case len(candidates) == 0:
		msg := fmt.Sprintf("no agent named %q that you can assign work to", to)
		if want != "" {
			msg += fmt.Sprintf(" in workspace %q", req.Workspace)
		}
		if len(available) > 0 {
			msg += "; available: " + strings.Join(available, ", ")
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	case len(candidates) > 1:
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			names = append(names, c.ws.Slug)
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q exists in several workspaces (%s); pass --workspace", to, strings.Join(names, ", ")))
		return
	}
	target := candidates[0]

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.SplitN(instructions, "\n", 2)[0]
	}
	if runes := []rune(title); len(runes) > 60 {
		title = string(runes[:60]) + "…"
	}
	res, err := h.IssueService.Create(r.Context(), service.IssueCreateParams{
		WorkspaceID:  target.ws.ID,
		Title:        title,
		Description:  pgtype.Text{String: lark.DelegateDescription(instructions), Valid: true},
		Status:       "todo",
		Priority:     "none",
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
		AssigneeID:   target.agent.ID,
		CreatorType:  "member",
		CreatorID:    person,
	}, service.IssueCreateOpts{
		ActorID:          personID,
		AnalyticsAgentID: uuidToString(target.agent.ID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create issue: "+err.Error())
		return
	}
	resp := LarkDelegateResponse{
		IssueID:      uuidToString(res.Issue.ID),
		Identifier:   service.IssueIdentifier(target.ws.IssuePrefix, res.Issue.Number),
		Workspace:    target.ws.Slug,
		AssigneeID:   uuidToString(target.agent.ID),
		AssigneeName: target.agent.Name,
	}
	relay, err := h.LarkTools.RegisterCrossWorkspaceRelay(r.Context(), scope, res.Issue)
	switch {
	case err == nil:
		resp.RelayedToChat = relay.ChatID
	case errors.Is(err, lark.ErrToolInvalidInput):
		resp.RelayError = err.Error()
	default:
		resp.RelayError = "the result will not be posted back automatically: " + err.Error()
	}
	writeJSON(w, http.StatusCreated, resp)
}
