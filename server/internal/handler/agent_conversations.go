package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Agent conversation history: a read-only view of every Chat anyone in the
// workspace has had with an agent, including conversations that arrived
// through an IM channel such as Feishu. Chat endpoints stay creator-only;
// these only read, and only for members who can access the agent.

const agentConversationsLimit = 200

// loadAgentForConversationReader resolves the agent and applies the
// private-agent gate. Returns ok=false after writing the error response.
func (h *Handler) loadAgentForConversationReader(w http.ResponseWriter, r *http.Request) (db.Agent, string, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.Agent{}, "", false
	}
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return db.Agent{}, "", false
	}
	workspaceID := uuidToString(agent.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if !h.canAccessPrivateAgent(r.Context(), agent, actorType, actorID, workspaceID) {
		writeError(w, http.StatusForbidden, "you do not have access to this agent")
		return db.Agent{}, "", false
	}
	return agent, workspaceID, true
}

// ListAgentConversations lists the agent's Chats across all members, most
// recently active first.
func (h *Handler) ListAgentConversations(w http.ResponseWriter, r *http.Request) {
	agent, _, ok := h.loadAgentForConversationReader(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListChatSessionsByAgent(r.Context(), db.ListChatSessionsByAgentParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     agent.ID,
		Limit:       agentConversationsLimit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	resp := make([]ChatSessionResponse, 0, len(rows))
	for _, s := range rows {
		resp = append(resp, ChatSessionResponse{
			ID:          uuidToString(s.ID),
			WorkspaceID: uuidToString(s.WorkspaceID),
			AgentID:     uuidToString(s.AgentID),
			CreatorID:   uuidToString(s.CreatorID),
			ProjectID:   uuidToPtr(s.ProjectID),
			Title:       s.Title,
			Status:      s.Status,
			LastMessage: buildChatLastMessage(s.LastMessageAt, s.LastMessageContent, s.LastMessageRole, s.LastMessageFailureReason, s.LastMessageKind),
			Pinned:      s.PinnedAt.Valid,
			CreatedAt:   timestampToString(s.CreatedAt),
			UpdatedAt:   timestampToString(s.UpdatedAt),
		})
	}
	if err := h.hydrateChatSessionChannelMetadata(r.Context(), resp); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat channel metadata")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ListAgentConversationMessages returns one of the agent's Chats, read-only.
func (h *Handler) ListAgentConversationMessages(w http.ResponseWriter, r *http.Request) {
	agent, workspaceID, ok := h.loadAgentForConversationReader(w, r)
	if !ok {
		return
	}
	sessionUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sessionId"), "chat session id")
	if !ok {
		return
	}
	session, err := h.Queries.GetPublicChatSessionInWorkspace(r.Context(), db.GetPublicChatSessionInWorkspaceParams{
		ID:          sessionUUID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil || session.AgentID != agent.ID {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	messages, err := h.Queries.ListChatMessages(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat messages")
		return
	}
	messages = visibleChatMessages(messages)
	messageIDs := make([]pgtype.UUID, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	groupedAtt := h.groupChatMessageAttachments(r.Context(), workspaceID, messageIDs)
	resp := make([]ChatMessageResponse, len(messages))
	for i, m := range messages {
		resp[i] = chatMessageToResponse(m, groupedAtt[uuidToString(m.ID)])
	}
	writeJSON(w, http.StatusOK, resp)
}
