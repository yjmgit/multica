package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// An agent answering in Feishu hands work to an agent in another workspace the
// requester belongs to; the issue lands there, created as the requester, and
// its result is relayed back to the chat.
func TestDelegateLarkWorkAcrossWorkspaces(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "Delegating agent", "")
	installationID := dbfx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "channel_type": "feishu",
		"config": testutil.Raw(`'{"app_id":"cli_cross_ws"}'::jsonb`), "status": "active", "installer_user_id": testUserID,
	})
	sessionID := dbfx.ChatSession(t, agentID)
	bindingID := dbfx.Insert(t, "channel_chat_session_binding", testutil.Cols{
		"chat_session_id": sessionID, "installation_id": installationID, "channel_type": "feishu",
		"channel_chat_id": "oc_cross", "chat_type": "group",
	})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "completed_at": testutil.Raw("now()"),
		"originator_user_id": testUserID, "accountable_user_id": testUserID})
	dbfx.InsertNoID(t, "channel_task_delivery", testutil.Cols{
		"task_id": taskID, "binding_id": bindingID, "installation_id": installationID,
		"channel_type": "feishu", "channel_chat_id": "oc_cross", "chat_type": "group",
		"channel_message_id": "om_ask", "channel_sender_id": "ou_asker", "route_revision": 1,
	}, "task_id = $1", taskID)

	otherWS := dbfx.Workspace(t, "Other team", "other-team-"+taskID[:8], testutil.Cols{"issue_prefix": "OTH"})
	dbfx.Member(t, otherWS, testUserID, "member")
	target := dbfx.Agent(t, "Platform helper", handlerTestRuntimeID(t), testutil.Cols{"workspace_id": otherWS})
	dbfx.Cleanup(t, `DELETE FROM channel_issue_relay WHERE installation_id = $1`, installationID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id = $1)`, otherWS)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE workspace_id = $1`, otherWS)

	prev := testHandler.LarkTools
	testHandler.LarkTools = lark.NewTools(db.New(testPool), handlerPlainSecret{}, &handlerFakeToolClient{}, nil)
	t.Cleanup(func() { testHandler.LarkTools = prev })

	delegate := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/lark/delegations", bytes.NewReader(raw))
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
		w := httptest.NewRecorder()
		testHandler.DelegateLarkWork(w, req.WithContext(ctx))
		return w
	}

	w := delegate(map[string]any{"to": "platform helper", "instructions": "查一下 prod 昨天新增的接口"})
	if w.Code != http.StatusCreated {
		t.Fatalf("delegate: %d %s", w.Code, w.Body.String())
	}
	var resp LarkDelegateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.AssigneeID != target || resp.RelayedToChat != "oc_cross" || !strings.HasPrefix(resp.Identifier, "OTH-") {
		t.Fatalf("response = %+v", resp)
	}
	var wsID, creatorID, assigneeID string
	if err := testPool.QueryRow(ctx, `SELECT workspace_id::text, creator_id::text, assignee_id::text FROM issue WHERE id = $1`, resp.IssueID).
		Scan(&wsID, &creatorID, &assigneeID); err != nil {
		t.Fatal(err)
	}
	if wsID != otherWS || creatorID != testUserID || assigneeID != target {
		t.Fatalf("issue in %s by %s for %s", wsID, creatorID, assigneeID)
	}

	// A workspace the requester is not in is never searched.
	w = delegate(map[string]any{"to": "Platform helper", "workspace": "no-such-team", "instructions": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown workspace: %d %s", w.Code, w.Body.String())
	}
	// An unknown agent lists what is available.
	w = delegate(map[string]any{"to": "Nobody", "instructions": "x"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Platform helper") {
		t.Fatalf("unknown agent: %d %s", w.Code, w.Body.String())
	}
}
