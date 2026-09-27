package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestLarkSendTime(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	at, scheduled, err := larkSendTime("", "", now)
	if err != nil || scheduled || !at.IsZero() {
		t.Fatalf("no time: %v %v %v", at, scheduled, err)
	}
	at, scheduled, err = larkSendTime("2m", "", now)
	if err != nil || !scheduled || !at.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("delay: %v %v %v", at, scheduled, err)
	}
	at, scheduled, err = larkSendTime("", "2026-09-27T09:00:00+08:00", now)
	if err != nil || !scheduled || !at.Equal(time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("send_at: %v %v %v", at, scheduled, err)
	}
	for _, bad := range [][2]string{{"2m", "2026-09-27T09:00:00+08:00"}, {"soon", ""}, {"-1m", ""}, {"", "2026-09-27 09:00"}} {
		if _, _, err := larkSendTime(bad[0], bad[1], now); err == nil {
			t.Errorf("larkSendTime(%q, %q): want error", bad[0], bad[1])
		}
	}
}

// Feishu tools act through the agent's bot, so a member session must not
// reach them — only the task token pins which agent and task is calling.
func TestLarkToolsRequireTaskToken(t *testing.T) {
	h := &Handler{LarkTools: lark.NewTools(nil, nil, nil, nil)}
	req := httptest.NewRequest(http.MethodGet, "/api/lark/context", nil)
	req.Header.Set("X-Workspace-ID", "11111111-1111-1111-1111-111111111111")
	w := httptest.NewRecorder()
	h.GetLarkContext(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	unconfigured := &Handler{}
	w = httptest.NewRecorder()
	unconfigured.GetLarkContext(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want 503", w.Code)
	}
}

// handlerFakeToolClient is a ToolAPIClient that records sends.
type handlerFakeToolClient struct {
	uploads []string
	sends   []string // msg types
}

func (f *handlerFakeToolClient) UploadImage(_ context.Context, _ lark.InstallationCredentials, name string, _ []byte) (string, error) {
	f.uploads = append(f.uploads, "image:"+name)
	return "img", nil
}
func (f *handlerFakeToolClient) UploadFile(_ context.Context, _ lark.InstallationCredentials, name string, _ []byte) (string, error) {
	f.uploads = append(f.uploads, "file:"+name)
	return "file", nil
}
func (f *handlerFakeToolClient) SendMessage(_ context.Context, _ lark.InstallationCredentials, _ lark.MessageTarget, msgType, _ string) (string, error) {
	f.sends = append(f.sends, msgType)
	return "om_x", nil
}
func (f *handlerFakeToolClient) GetDocRawContent(context.Context, lark.InstallationCredentials, string) (string, error) {
	return "", nil
}
func (f *handlerFakeToolClient) GetWikiNode(context.Context, lark.InstallationCredentials, string) (lark.WikiNode, error) {
	return lark.WikiNode{}, nil
}
func (f *handlerFakeToolClient) CreateChat(context.Context, lark.InstallationCredentials, lark.CreateChatParams) (lark.ChatInfo, error) {
	return lark.ChatInfo{}, nil
}
func (f *handlerFakeToolClient) AddChatMembers(context.Context, lark.InstallationCredentials, string, []string) ([]string, error) {
	return nil, nil
}
func (f *handlerFakeToolClient) ListChats(context.Context, lark.InstallationCredentials, string) ([]lark.ChatInfo, string, error) {
	return nil, "", nil
}
func (f *handlerFakeToolClient) ListSheets(context.Context, lark.InstallationCredentials, string) ([]lark.SheetInfo, error) {
	return nil, nil
}
func (f *handlerFakeToolClient) GetSheetValues(context.Context, lark.InstallationCredentials, string, string) ([][]string, error) {
	return nil, nil
}
func (f *handlerFakeToolClient) ListChatMembers(context.Context, lark.InstallationCredentials, string, string) ([]lark.ChatMember, string, error) {
	return nil, "", nil
}

func (f *handlerFakeToolClient) GetLegacyDocRawContent(context.Context, lark.InstallationCredentials, string) (string, error) {
	return "", nil
}
func (f *handlerFakeToolClient) ListBitableTables(context.Context, lark.InstallationCredentials, string) ([]lark.BitableTable, error) {
	return nil, nil
}
func (f *handlerFakeToolClient) ListBitableFields(context.Context, lark.InstallationCredentials, string, string) ([]string, error) {
	return nil, nil
}
func (f *handlerFakeToolClient) ListBitableRecords(context.Context, lark.InstallationCredentials, string, string, string) ([]map[string]any, string, error) {
	return nil, "", nil
}
func (f *handlerFakeToolClient) CreateDocFromMarkdown(context.Context, lark.InstallationCredentials, string, string) (string, error) {
	return "doxcn", nil
}
func (f *handlerFakeToolClient) ShareDoc(context.Context, lark.InstallationCredentials, string, []string, string, string) error {
	return nil
}
func (f *handlerFakeToolClient) DocURL(context.Context, lark.InstallationCredentials, string) (string, error) {
	return "https://x/docx/doxcn", nil
}

type handlerPlainSecret struct{}

func (handlerPlainSecret) DecryptAppSecret(lark.Installation) (string, error) { return "secret", nil }

// The CLI's multipart request sends text and files now, or stores a
// scheduled message when it carries a delay.
func TestSendLarkMessageMultipart(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "Lark tools agent", "")
	installationID := dbfx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "feishu",
		"config": testutil.Raw(`'{"app_id":"cli_handler_tools"}'::jsonb`), "status": "active", "installer_user_id": testUserID,
	})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "completed_at": testutil.Raw("now()")})
	dbfx.Cleanup(t, `DELETE FROM channel_scheduled_message WHERE installation_id = $1`, installationID)

	client := &handlerFakeToolClient{}
	h := &Handler{LarkTools: lark.NewTools(db.New(testPool), handlerPlainSecret{}, client, nil)}

	send := func(fields map[string]string, files map[string]string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		for name, data := range files {
			part, _ := mw.CreateFormFile("file", name)
			_, _ = part.Write([]byte(data))
		}
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/lark/send", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
		w := httptest.NewRecorder()
		h.SendLarkMessage(w, req.WithContext(ctx))
		return w
	}

	w := send(map[string]string{"text": "report", "chat_id": "oc_team"}, map[string]string{"r.pdf": "%PDF-1.4 x"})
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	if len(client.sends) != 2 || client.sends[0] != "text" || client.sends[1] != "file" || client.uploads[0] != "file:r.pdf" {
		t.Fatalf("sends = %v uploads = %v", client.sends, client.uploads)
	}

	w = send(map[string]string{"text": "关煤气", "chat_id": "oc_team", "delay": "2m"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
	}
	var resp LarkSendResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Scheduled == nil {
		t.Fatalf("schedule response = %s (%v)", w.Body.String(), err)
	}
	if d := time.Until(resp.Scheduled.FireAt); d < time.Minute || d > 3*time.Minute {
		t.Fatalf("fire_at %v is not ~2 minutes out", resp.Scheduled.FireAt)
	}

	// No Feishu conversation behind this task and no target: a clear 400.
	w = send(map[string]string{"text": "hi"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no target: %d %s", w.Code, w.Body.String())
	}
	// Files cannot be scheduled.
	w = send(map[string]string{"chat_id": "oc_team", "delay": "1m"}, map[string]string{"a.txt": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("scheduled file: %d %s", w.Code, w.Body.String())
	}
}

// Members see the workspace's pending scheduled messages; cancelling needs the
// agent's owner or a workspace admin.
func TestWorkspaceLarkScheduledListAndCancel(t *testing.T) {
	agentID := dbfx.Agent(t, "Scheduled owner agent", "")
	installationID := dbfx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "feishu",
		"config": testutil.Raw(`'{"app_id":"cli_sched_ui"}'::jsonb`), "status": "active", "installer_user_id": testUserID,
	})
	scheduledID := dbfx.Insert(t, "channel_scheduled_message", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": installationID, "agent_id": agentID,
		"channel_type": "feishu", "receive_id_type": "chat_id", "receive_id": "oc_ui", "text": "关煤气",
		"fire_at": testutil.Raw("now() + interval '1 hour'"),
	})

	w := httptest.NewRecorder()
	testHandler.ListWorkspaceLarkScheduled(w, withURLParam(newRequest(http.MethodGet, "/", nil), "id", testWorkspaceID))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Scheduled []lark.ScheduledMessage `json:"scheduled"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	found := false
	for _, s := range list.Scheduled {
		found = found || (s.ID == scheduledID && s.Text == "关煤气" && s.Kind == "message")
	}
	if !found {
		t.Fatalf("scheduled message missing from %s", w.Body.String())
	}

	// A plain member who does not own the agent cannot cancel it.
	outsider := dbfx.User(t, "Scheduled outsider", "sched-outsider-"+scheduledID[:8]+"@multica.test")
	dbfx.Member(t, testWorkspaceID, outsider, "member")
	req := withURLParams(newRequest(http.MethodDelete, "/", nil), "id", testWorkspaceID, "scheduledId", scheduledID)
	req.Header.Set("X-User-ID", outsider)
	w = httptest.NewRecorder()
	testHandler.CancelWorkspaceLarkScheduled(w, req)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("outsider cancel: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	testHandler.CancelWorkspaceLarkScheduled(w, withURLParams(newRequest(http.MethodDelete, "/", nil), "id", testWorkspaceID, "scheduledId", scheduledID))
	if w.Code != http.StatusOK {
		t.Fatalf("owner cancel: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	testHandler.CancelWorkspaceLarkScheduled(w, withURLParams(newRequest(http.MethodDelete, "/", nil), "id", testWorkspaceID, "scheduledId", scheduledID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("second cancel: %d, want 404", w.Code)
	}
}
