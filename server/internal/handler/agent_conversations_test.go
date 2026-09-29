package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestAgentConversations: a member who can access an agent reads every Chat
// with it, including ones someone else started; a member who cannot access
// the agent reads none.
func TestAgentConversations(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, ownerID, memberID := privateAgentTestFixture(t)

	var sessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status)
		VALUES ($1, $2, $3, 'owner feishu chat', 'active')
		RETURNING id
	`, testWorkspaceID, agentID, ownerID).Scan(&sessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content) VALUES ($1, 'user', 'how many tenants?')
	`, sessionID); err != nil {
		t.Fatalf("seed chat message: %v", err)
	}

	// The workspace owner (testUserID) did not start the Chat but can
	// access the private agent.
	w := httptest.NewRecorder()
	req := withURLParams(newRequest("GET", "/api/agents/"+agentID+"/conversations", nil), "id", agentID)
	testHandler.ListAgentConversations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var sessions []ChatSessionResponse
	json.NewDecoder(w.Body).Decode(&sessions)
	if len(sessions) != 1 || sessions[0].ID != sessionID || sessions[0].CreatorID != ownerID {
		t.Fatalf("list = %+v, want the owner's Chat", sessions)
	}
	if sessions[0].LastMessage == nil {
		t.Fatalf("list row missing last message preview: %+v", sessions[0])
	}

	w = httptest.NewRecorder()
	req = withURLParams(newRequest("GET", "/api/agents/"+agentID+"/conversations/"+sessionID+"/messages", nil),
		"id", agentID, "sessionId", sessionID)
	testHandler.ListAgentConversationMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("messages: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var messages []ChatMessageResponse
	json.NewDecoder(w.Body).Decode(&messages)
	if len(messages) != 1 || messages[0].Content != "how many tenants?" {
		t.Fatalf("messages = %+v", messages)
	}

	// A Chat must be read through its own agent.
	w = httptest.NewRecorder()
	var otherAgent string
	if err := testPool.QueryRow(ctx, `SELECT id FROM agent WHERE workspace_id = $1 AND id != $2 LIMIT 1`, testWorkspaceID, agentID).Scan(&otherAgent); err != nil {
		t.Fatalf("load another agent: %v", err)
	}
	req = withURLParams(newRequest("GET", "/api/agents/"+otherAgent+"/conversations/"+sessionID+"/messages", nil),
		"id", otherAgent, "sessionId", sessionID)
	testHandler.ListAgentConversationMessages(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("messages via another agent: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// A plain member cannot access the private agent, so reads nothing.
	memberRow, err := testHandler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      util.MustParseUUID(memberID),
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load plain member row: %v", err)
	}
	for _, path := range []string{"/conversations", "/conversations/" + sessionID + "/messages"} {
		w = httptest.NewRecorder()
		req = newRequestAs(memberID, "GET", "/api/agents/"+agentID+path, nil)
		req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, memberRow))
		req = withURLParams(req, "id", agentID, "sessionId", sessionID)
		if path == "/conversations" {
			testHandler.ListAgentConversations(w, req)
		} else {
			testHandler.ListAgentConversationMessages(w, req)
		}
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s as plain member: expected 403, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}
