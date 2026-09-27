package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/dispatch"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Tools backs the agent-facing Feishu commands (`multica lark ...`). Every
// call is scoped to one running agent task: the task's own Feishu chat (when
// it came from one) is the default target, and the agent's Feishu bot
// installation supplies the credentials. An agent can therefore only act
// through the bot it owns, with whatever reach that bot has in Feishu.
type Tools struct {
	store       *ChannelStore
	queries     *db.Queries
	credentials CredentialsResolver
	client      ToolAPIClient
	autopilots  AutopilotDispatcher
	appURL      string
	logger      *slog.Logger
	now         func() time.Time
}

// AutopilotDispatcher starts one run of an autopilot on behalf of a member.
// service.AutopilotService implements it.
type AutopilotDispatcher interface {
	DispatchAutopilotManual(ctx context.Context, autopilot db.Autopilot, triggerID pgtype.UUID, payload []byte, actorUserID pgtype.UUID) (*db.AutopilotRun, dispatch.ReasonCode, error)
}

// SetAutopilotDispatcher enables one-off agent wake-ups. Call at boot.
func (t *Tools) SetAutopilotDispatcher(d AutopilotDispatcher) { t.autopilots = d }

// NewTools wires the tools service. client is normally the production
// httpAPIClient (which implements ToolAPIClient).
func NewTools(queries *db.Queries, credentials CredentialsResolver, client ToolAPIClient, logger *slog.Logger) *Tools {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tools{
		store:       NewChannelStore(queries),
		queries:     queries,
		credentials: credentials,
		client:      client,
		logger:      logger,
		now:         time.Now,
	}
}

// Tool errors the handler maps to 4xx responses.
var (
	// ErrToolNoInstallation: the agent has no active Feishu bot.
	ErrToolNoInstallation = errors.New("this agent has no active Feishu bot installation")
	// ErrToolNoTarget: no --chat/--user was given and the task is not a
	// Feishu conversation to default to.
	ErrToolNoTarget = errors.New("no target: pass a chat_id or open_id (this task is not running in a Feishu chat)")
	// ErrToolInvalidInput covers malformed requests.
	ErrToolInvalidInput = errors.New("invalid input")
	// ErrToolNotFound: the scheduled message does not exist or is not pending.
	ErrToolNotFound = errors.New("not found")
)

func invalidInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrToolInvalidInput, fmt.Sprintf(format, args...))
}

// ToolScope identifies the calling agent task.
type ToolScope struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	TaskID      pgtype.UUID
}

// CurrentChat is the Feishu conversation the task was triggered from.
type CurrentChat struct {
	ChatID   string `json:"chat_id"`
	ChatType string `json:"chat_type"`
	// RequesterOpenID is the open_id of the person whose message started
	// this task.
	RequesterOpenID string `json:"requester_open_id,omitempty"`
	// reply keeps messages inside a Lark topic when the session is one.
	reply ReplyTarget
}

type toolContext struct {
	inst    Installation
	creds   InstallationCredentials
	current *CurrentChat
}

func (t *Tools) resolve(ctx context.Context, scope ToolScope) (toolContext, error) {
	var tc toolContext
	delivery, err := t.store.GetChannelTaskDelivery(ctx, scope.TaskID)
	switch {
	case err == nil && delivery.ChannelType == channelTypeFeishu:
		inst, err := t.store.GetLarkInstallation(ctx, delivery.InstallationID)
		if err != nil {
			return tc, fmt.Errorf("load installation: %w", err)
		}
		tc.inst = inst
		binding := ChatSessionBinding{
			ID: delivery.BindingID, InstallationID: delivery.InstallationID,
			ChannelChatID: delivery.ChannelChatID, ChatType: delivery.ChatType, Config: delivery.Config,
			LastMessageID: delivery.ChannelMessageID, LastThreadID: delivery.ChannelThreadID,
			LastSenderID: delivery.ChannelSenderID,
		}
		cur := &CurrentChat{ChatID: string(outboundChatID(binding)), ChatType: delivery.ChatType}
		if delivery.ChannelSenderID.Valid {
			cur.RequesterOpenID = delivery.ChannelSenderID.String
		}
		if isTopicIsolated(binding) {
			cur.reply = threadReplyTarget(binding)
		}
		tc.current = cur
	case err == nil || errors.Is(err, pgx.ErrNoRows):
		inst, err := t.agentInstallation(ctx, scope)
		if err != nil {
			return tc, err
		}
		tc.inst = inst
	default:
		return tc, fmt.Errorf("lookup task delivery: %w", err)
	}
	if tc.inst.WorkspaceID != scope.WorkspaceID || tc.inst.AgentID != scope.AgentID ||
		InstallationStatus(tc.inst.Status) != InstallationActive {
		return tc, ErrToolNoInstallation
	}
	tc.creds, err = t.installationCredentials(tc.inst)
	if err != nil {
		return tc, err
	}
	return tc, nil
}

func (t *Tools) agentInstallation(ctx context.Context, scope ToolScope) (Installation, error) {
	insts, err := t.store.ListLarkInstallationsByWorkspace(ctx, scope.WorkspaceID)
	if err != nil {
		return Installation{}, fmt.Errorf("list installations: %w", err)
	}
	for _, inst := range insts {
		if inst.AgentID == scope.AgentID && InstallationStatus(inst.Status) == InstallationActive {
			return inst, nil
		}
	}
	return Installation{}, ErrToolNoInstallation
}

func (t *Tools) installationCredentials(inst Installation) (InstallationCredentials, error) {
	secret, err := t.credentials.DecryptAppSecret(inst)
	if err != nil {
		return InstallationCredentials{}, fmt.Errorf("decrypt app_secret: %w", err)
	}
	creds := InstallationCredentials{AppID: inst.AppID, AppSecret: secret, Region: RegionOrDefault(inst.Region)}
	if inst.TenantKey.Valid {
		creds.TenantKey = inst.TenantKey.String
	}
	return creds, nil
}

// ToolContextInfo is what `multica lark context` reports.
type ToolContextInfo struct {
	BotOpenID   string       `json:"bot_open_id"`
	CurrentChat *CurrentChat `json:"current_chat,omitempty"`
}

// Context reports the bot and the current Feishu conversation, if any.
func (t *Tools) Context(ctx context.Context, scope ToolScope) (ToolContextInfo, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return ToolContextInfo{}, err
	}
	return ToolContextInfo{BotOpenID: tc.inst.BotOpenID, CurrentChat: tc.current}, nil
}

// ToolTarget selects where a message goes. Empty means the current chat.
type ToolTarget struct {
	ChatID string `json:"chat_id,omitempty"`
	OpenID string `json:"open_id,omitempty"`
}

func (tc toolContext) messageTarget(target ToolTarget) (MessageTarget, error) {
	switch {
	case target.ChatID != "" && target.OpenID != "":
		return MessageTarget{}, invalidInput("pass either a chat_id or an open_id, not both")
	case target.ChatID != "":
		if tc.current != nil && target.ChatID == tc.current.ChatID {
			return MessageTarget{ChatID: target.ChatID, Reply: tc.current.reply}, nil
		}
		return MessageTarget{ChatID: target.ChatID}, nil
	case target.OpenID != "":
		return MessageTarget{OpenID: target.OpenID}, nil
	case tc.current != nil:
		return MessageTarget{ChatID: tc.current.ChatID, Reply: tc.current.reply}, nil
	default:
		return MessageTarget{}, ErrToolNoTarget
	}
}

// mentions returns the open_ids to @-mention, adding the requester when asked.
func (tc toolContext) mentions(ids []string, mentionRequester bool) ([]string, error) {
	out := make([]string, 0, len(ids)+1)
	seen := map[string]bool{}
	add := func(id string) error {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return nil
		}
		if safeMentionOpenID(id) == "" {
			return invalidInput("invalid open_id %q", id)
		}
		seen[id] = true
		out = append(out, id)
		return nil
	}
	if mentionRequester {
		if tc.current == nil || tc.current.RequesterOpenID == "" {
			return nil, invalidInput("there is no requester to mention: this task was not started from a Feishu message")
		}
		if err := add(tc.current.RequesterOpenID); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// textMessage renders text with leading mentions as either a plain text
// message or, when it contains markdown, a markdown card — the same choice
// the reply Patcher makes.
func textMessage(text string, mentionOpenIDs []string) (msgType, content string, err error) {
	if containsMarkdown(text) {
		body := text
		for i := len(mentionOpenIDs) - 1; i >= 0; i-- {
			body = prependMarkdownMention(mentionOpenIDs[i], body)
		}
		card, err := markdownCardJSON(body, "")
		return "interactive", card, err
	}
	body := text
	for i := len(mentionOpenIDs) - 1; i >= 0; i-- {
		body = prependTextMention(mentionOpenIDs[i], body)
	}
	raw, err := json.Marshal(map[string]string{"text": body})
	return "text", string(raw), err
}

// ToolFile is one file to send.
type ToolFile struct {
	Name        string
	ContentType string
	Data        []byte
}

// SendInput is one `multica lark send`.
type SendInput struct {
	Target           ToolTarget
	Text             string
	MentionOpenIDs   []string
	MentionRequester bool
	Files            []ToolFile
}

// SendResult lists the message ids Lark assigned, in send order.
type SendResult struct {
	MessageIDs []string `json:"message_ids"`
}

// Send posts text and/or files to the target now. Text goes first; each file
// is its own message, since Lark messages carry one resource each.
func (t *Tools) Send(ctx context.Context, scope ToolScope, in SendInput) (SendResult, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" && len(in.Files) == 0 {
		return SendResult{}, invalidInput("nothing to send: provide text or at least one file")
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return SendResult{}, err
	}
	target, err := tc.messageTarget(in.Target)
	if err != nil {
		return SendResult{}, err
	}
	mentions, err := tc.mentions(in.MentionOpenIDs, in.MentionRequester)
	if err != nil {
		return SendResult{}, err
	}
	res := SendResult{MessageIDs: []string{}}
	if text != "" || len(mentions) > 0 {
		msgType, content, err := textMessage(text, mentions)
		if err != nil {
			return res, fmt.Errorf("encode message: %w", err)
		}
		id, err := t.client.SendMessage(ctx, tc.creds, target, msgType, content)
		if err != nil {
			return res, err
		}
		res.MessageIDs = append(res.MessageIDs, id)
	}
	for _, f := range in.Files {
		id, err := sendFileMessage(ctx, t.client, tc.creds, target, f)
		if err != nil {
			return res, fmt.Errorf("send %s: %w", f.Name, err)
		}
		res.MessageIDs = append(res.MessageIDs, id)
	}
	return res, nil
}

// sendFileMessage uploads one file and posts it as an image or file message.
func sendFileMessage(ctx context.Context, client ToolAPIClient, creds InstallationCredentials, target MessageTarget, f ToolFile) (string, error) {
	if IsMessageImage(f.ContentType) && len(f.Data) <= maxMessageImageBytes {
		key, err := client.UploadImage(ctx, creds, f.Name, f.Data)
		if err != nil {
			return "", err
		}
		content, _ := json.Marshal(map[string]string{"image_key": key})
		return client.SendMessage(ctx, creds, target, "image", string(content))
	}
	key, err := client.UploadFile(ctx, creds, f.Name, f.Data)
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(map[string]string{"file_key": key})
	return client.SendMessage(ctx, creds, target, "file", string(content))
}

// ---- scheduled messages ----

// maxScheduleAhead bounds how far out a message may be scheduled.
const maxScheduleAhead = 366 * 24 * time.Hour

// ScheduleInput is one `multica lark send --in/--at`.
type ScheduleInput struct {
	Target           ToolTarget
	Text             string
	MentionOpenIDs   []string
	MentionRequester bool
	FireAt           time.Time
}

// ScheduledMessage is the agent-visible view of a scheduled message.
type ScheduledMessage struct {
	ID string `json:"id"`
	// Kind is "message" (post Text) or "agent_run" (start AutopilotID).
	Kind           string    `json:"kind"`
	AutopilotID    string    `json:"autopilot_id,omitempty"`
	AgentID        string    `json:"agent_id"`
	ReceiveIDType  string    `json:"receive_id_type"`
	ReceiveID      string    `json:"receive_id"`
	Text           string    `json:"text"`
	MentionOpenIDs []string  `json:"mention_open_ids"`
	FireAt         time.Time `json:"fire_at"`
	Status         string    `json:"status"`
}

// ScheduledMessageFromRow is the API view of a channel_scheduled_message row.
func ScheduledMessageFromRow(row db.ChannelScheduledMessage) ScheduledMessage {
	return ScheduledMessage{
		ID:             uuidString(row.ID),
		Kind:           row.Kind,
		AutopilotID:    uuidString(row.AutopilotID),
		AgentID:        uuidString(row.AgentID),
		ReceiveIDType:  row.ReceiveIDType,
		ReceiveID:      row.ReceiveID,
		Text:           row.Text,
		MentionOpenIDs: row.MentionOpenIds,
		FireAt:         row.FireAt.Time.UTC(),
		Status:         row.Status,
	}
}

// Schedule stores a text message for the scheduler to send at FireAt.
func (t *Tools) Schedule(ctx context.Context, scope ToolScope, in ScheduleInput) (ScheduledMessage, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return ScheduledMessage{}, invalidInput("a scheduled message needs text (files can only be sent immediately)")
	}
	now := t.now()
	if !in.FireAt.After(now) {
		return ScheduledMessage{}, invalidInput("the send time must be in the future")
	}
	if in.FireAt.Sub(now) > maxScheduleAhead {
		return ScheduledMessage{}, invalidInput("the send time must be within a year")
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return ScheduledMessage{}, err
	}
	target, err := tc.messageTarget(in.Target)
	if err != nil {
		return ScheduledMessage{}, err
	}
	mentions, err := tc.mentions(in.MentionOpenIDs, in.MentionRequester)
	if err != nil {
		return ScheduledMessage{}, err
	}
	params := db.CreateChannelScheduledMessageParams{
		WorkspaceID:    scope.WorkspaceID,
		InstallationID: tc.inst.ID,
		AgentID:        scope.AgentID,
		ChannelType:    channelTypeFeishu,
		TaskID:         scope.TaskID,
		ReceiveIDType:  "chat_id",
		ReceiveID:      target.ChatID,
		ReplyMessageID: target.Reply.MessageID,
		ReplyInThread:  target.Reply.InThread,
		Text:           text,
		MentionOpenIds: mentions,
		FireAt:         pgtype.Timestamptz{Time: in.FireAt, Valid: true},
		Kind:           scheduledKindMessage,
	}
	if target.OpenID != "" {
		params.ReceiveIDType, params.ReceiveID = "open_id", target.OpenID
	}
	row, err := t.queries.CreateChannelScheduledMessage(ctx, params)
	if err != nil {
		return ScheduledMessage{}, fmt.Errorf("store scheduled message: %w", err)
	}
	return ScheduledMessageFromRow(row), nil
}

// Scheduled row kinds.
const (
	scheduledKindMessage  = "message"
	scheduledKindAgentRun = "agent_run"
)

// ScheduleAgentRunInput is a one-off wake-up: start AutopilotID at FireAt.
// The autopilot must be a run_only autopilot assigned to the calling agent.
type ScheduleAgentRunInput struct {
	AutopilotID pgtype.UUID
	FireAt      time.Time
}

// ScheduleAgentRun stores a one-off wake-up for the scheduler.
func (t *Tools) ScheduleAgentRun(ctx context.Context, scope ToolScope, in ScheduleAgentRunInput) (ScheduledMessage, error) {
	if t.autopilots == nil {
		return ScheduledMessage{}, invalidInput("agent wake-ups are not enabled on this server")
	}
	now := t.now()
	if !in.FireAt.After(now) {
		return ScheduledMessage{}, invalidInput("the wake-up time must be in the future")
	}
	if in.FireAt.Sub(now) > maxScheduleAhead {
		return ScheduledMessage{}, invalidInput("the wake-up time must be within a year")
	}
	// The run acts for the member the calling task acts for — the same
	// originator the autopilot endpoints used to create this autopilot — not
	// the task token's user, which is the runtime owner.
	task, err := t.queries.GetAgentTask(ctx, scope.TaskID)
	if err != nil {
		return ScheduledMessage{}, fmt.Errorf("load task: %w", err)
	}
	if !task.OriginatorUserID.Valid {
		return ScheduledMessage{}, invalidInput("this task does not act for a member, so it cannot schedule a wake-up")
	}
	ap, err := t.queries.GetAutopilot(ctx, in.AutopilotID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ScheduledMessage{}, invalidInput("autopilot not found")
		}
		return ScheduledMessage{}, err
	}
	if ap.WorkspaceID != scope.WorkspaceID || ap.AssigneeID != scope.AgentID || ap.ExecutionMode != "run_only" {
		return ScheduledMessage{}, invalidInput("the autopilot must be a run_only autopilot assigned to this agent")
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return ScheduledMessage{}, err
	}
	params := db.CreateChannelScheduledMessageParams{
		WorkspaceID:    scope.WorkspaceID,
		InstallationID: tc.inst.ID,
		AgentID:        scope.AgentID,
		ChannelType:    channelTypeFeishu,
		TaskID:         scope.TaskID,
		ReceiveIDType:  "chat_id",
		Text:           ap.Title,
		MentionOpenIds: []string{},
		FireAt:         pgtype.Timestamptz{Time: in.FireAt, Valid: true},
		Kind:           scheduledKindAgentRun,
		AutopilotID:    ap.ID,
		ActorUserID:    task.OriginatorUserID,
	}
	if tc.current != nil {
		params.ReceiveID = tc.current.ChatID
	}
	row, err := t.queries.CreateChannelScheduledMessage(ctx, params)
	if err != nil {
		return ScheduledMessage{}, fmt.Errorf("store wake-up: %w", err)
	}
	return ScheduledMessageFromRow(row), nil
}

// ListScheduled lists the agent's pending scheduled messages.
func (t *Tools) ListScheduled(ctx context.Context, scope ToolScope) ([]ScheduledMessage, error) {
	rows, err := t.queries.ListPendingChannelScheduledMessagesByAgent(ctx, db.ListPendingChannelScheduledMessagesByAgentParams{
		WorkspaceID: scope.WorkspaceID, AgentID: scope.AgentID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ScheduledMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, ScheduledMessageFromRow(row))
	}
	return out, nil
}

// CancelScheduled cancels one of the agent's pending scheduled messages.
func (t *Tools) CancelScheduled(ctx context.Context, scope ToolScope, id pgtype.UUID) (ScheduledMessage, error) {
	row, err := t.queries.CancelChannelScheduledMessage(ctx, db.CancelChannelScheduledMessageParams{
		ID: id, WorkspaceID: scope.WorkspaceID, AgentID: scope.AgentID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ScheduledMessage{}, ErrToolNotFound
		}
		return ScheduledMessage{}, err
	}
	return ScheduledMessageFromRow(row), nil
}

// Scheduler cadence. Polling every few seconds keeps a "remind me in 2
// minutes" on time without a timer per message.
const (
	scheduledPollInterval = 5 * time.Second
	scheduledBatchSize    = 20
	scheduledStaleAfter   = 10 * time.Minute
	scheduledSendTimeout  = 15 * time.Second
)

// RunScheduler sends due scheduled messages until ctx ends. Safe to run on
// every replica: rows are claimed with SKIP LOCKED.
func (t *Tools) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(scheduledPollInterval)
	defer ticker.Stop()
	for {
		t.sendDueScheduled(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Tools) sendDueScheduled(ctx context.Context) {
	if n, err := t.queries.FailStaleSendingChannelScheduledMessages(ctx, pgtype.Interval{Microseconds: scheduledStaleAfter.Microseconds(), Valid: true}); err != nil {
		t.logger.Warn("lark scheduler: fail stale sends", "error", err)
	} else if n > 0 {
		t.logger.Warn("lark scheduler: failed interrupted scheduled messages", "count", n)
	}
	rows, err := t.queries.ClaimDueChannelScheduledMessages(ctx, scheduledBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			t.logger.Warn("lark scheduler: claim due messages", "error", err)
		}
		return
	}
	for _, row := range rows {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledSendTimeout)
		messageID, sendErr := t.deliverScheduled(sendCtx, row)
		cancel()
		finish := db.FinishChannelScheduledMessageParams{ID: row.ID, Status: "sent", SentMessageID: messageID}
		if sendErr != nil {
			finish.Status = "failed"
			finish.LastError = truncate(sendErr.Error(), 500)
			t.logger.Warn("lark scheduler: send failed",
				"scheduled_message_id", uuidString(row.ID), "error", sendErr)
		}
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := t.queries.FinishChannelScheduledMessage(finishCtx, finish); err != nil {
			t.logger.Warn("lark scheduler: record result", "scheduled_message_id", uuidString(row.ID), "error", err)
		}
		finishCancel()
	}
}

func (t *Tools) deliverScheduled(ctx context.Context, row db.ChannelScheduledMessage) (string, error) {
	if row.Kind == scheduledKindAgentRun {
		return t.startScheduledRun(ctx, row)
	}
	inst, err := t.store.GetLarkInstallation(ctx, row.InstallationID)
	if err != nil {
		return "", fmt.Errorf("load installation: %w", err)
	}
	if inst.WorkspaceID != row.WorkspaceID || inst.AgentID != row.AgentID ||
		InstallationStatus(inst.Status) != InstallationActive {
		return "", ErrToolNoInstallation
	}
	creds, err := t.installationCredentials(inst)
	if err != nil {
		return "", err
	}
	target := MessageTarget{}
	if row.ReceiveIDType == "open_id" {
		target.OpenID = row.ReceiveID
	} else {
		target.ChatID = row.ReceiveID
	}
	if row.ReplyMessageID != "" {
		target.Reply = ReplyTarget{MessageID: row.ReplyMessageID, InThread: row.ReplyInThread}
	}
	msgType, content, err := textMessage(row.Text, row.MentionOpenIds)
	if err != nil {
		return "", err
	}
	return t.client.SendMessage(ctx, creds, target, msgType, content)
}

// startScheduledRun dispatches a one-off wake-up's autopilot and returns the
// run id. The member it runs for must still belong to the workspace.
func (t *Tools) startScheduledRun(ctx context.Context, row db.ChannelScheduledMessage) (string, error) {
	if t.autopilots == nil {
		return "", errors.New("agent wake-ups are not enabled on this server")
	}
	ap, err := t.queries.GetAutopilot(ctx, row.AutopilotID)
	if err != nil {
		return "", fmt.Errorf("load autopilot: %w", err)
	}
	if ap.WorkspaceID != row.WorkspaceID || ap.AssigneeID != row.AgentID {
		return "", errors.New("the autopilot no longer belongs to this agent")
	}
	if ap.Status != "active" {
		return "", fmt.Errorf("the autopilot is %s", ap.Status)
	}
	member, err := t.store.IsWorkspaceMember(ctx, row.WorkspaceID, row.ActorUserID)
	if err != nil {
		return "", err
	}
	if !member {
		return "", errors.New("the member this wake-up runs for left the workspace")
	}
	run, reason, err := t.autopilots.DispatchAutopilotManual(ctx, ap, pgtype.UUID{}, nil, row.ActorUserID)
	if err != nil {
		return "", err
	}
	if run == nil {
		return "", fmt.Errorf("the run was not started (%s)", reason)
	}
	if run.Status == "skipped" || run.Status == "failed" {
		return uuidString(run.ID), fmt.Errorf("the run was %s: %s", run.Status, run.FailureReason.String)
	}
	return uuidString(run.ID), nil
}

// ---- documents ----

// maxDocChars bounds how much document text a single read returns, so one
// huge doc cannot flood the agent's context.
const maxDocChars = 100_000

// DocResult is a document's plain text.
type DocResult struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Title     string `json:"title,omitempty"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ParseDocURL extracts the document kind and token from a Feishu/Lark
// document link (…/docx/<token>, …/wiki/<token>, …/sheets/<token>). A bare
// token is treated as a docx document id.
func ParseDocURL(raw string) (kind, token string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", invalidInput("missing document url")
	}
	if !strings.Contains(raw, "/") {
		return "docx", raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", invalidInput("invalid document url: %v", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "docx", "wiki", "sheets", "docs", "base":
			return parts[i], parts[i+1], nil
		}
	}
	return "", "", invalidInput("unrecognized Feishu document url %q (expected …/docx/…, …/docs/…, …/wiki/…, …/sheets/… or …/base/…)", raw)
}

// ReadDoc returns the text of a docx document or spreadsheet, following wiki
// links to the document they wrap. The bot can only read documents shared
// with it (or with the whole tenant, when the app has that scope).
func (t *Tools) ReadDoc(ctx context.Context, scope ToolScope, rawURL string) (DocResult, error) {
	kind, token, err := ParseDocURL(rawURL)
	if err != nil {
		return DocResult{}, err
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return DocResult{}, err
	}
	res := DocResult{Type: kind, Token: token}
	if kind == "wiki" {
		node, err := t.client.GetWikiNode(ctx, tc.creds, token)
		if err != nil {
			return DocResult{}, err
		}
		kind, token = node.ObjType, node.ObjToken
		res.Type, res.Token, res.Title = kind, token, node.Title
	}
	switch kind {
	case "docx":
		res.Content, err = t.client.GetDocRawContent(ctx, tc.creds, token)
	case "sheet", "sheets":
		res.Type = "sheet"
		res.Content, err = t.readSpreadsheet(ctx, tc.creds, token)
	case "doc", "docs":
		res.Type = "doc"
		res.Content, err = t.client.GetLegacyDocRawContent(ctx, tc.creds, token)
	case "bitable", "base":
		res.Type = "bitable"
		res.Content, err = t.readBitable(ctx, tc.creds, token)
	default:
		return DocResult{}, invalidInput("reading %q documents is not supported (docx, legacy docs, sheets and bases are)", kind)
	}
	if err != nil {
		return DocResult{}, err
	}
	res.Content, res.Truncated = truncateRunes(res.Content, maxDocChars)
	return res, nil
}

// readSpreadsheet renders every sheet as tab-separated rows under a
// "## <title>" heading.
func (t *Tools) readSpreadsheet(ctx context.Context, creds InstallationCredentials, token string) (string, error) {
	sheets, err := t.client.ListSheets(ctx, creds, token)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, sh := range sheets {
		rows, err := t.client.GetSheetValues(ctx, creds, token, sh.SheetID)
		if err != nil {
			return "", fmt.Errorf("sheet %q: %w", sh.Title, err)
		}
		fmt.Fprintf(&b, "## %s\n", sh.Title)
		for _, row := range rows {
			b.WriteString(strings.Join(row, "\t"))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		if b.Len() > maxDocChars*4 {
			break
		}
	}
	return b.String(), nil
}

// maxBitableRecords bounds how many records of one table a read renders.
const maxBitableRecords = 2000

// readBitable renders every table of a base as a header row of field names
// followed by one tab-separated row per record, under a "## <table>" heading.
func (t *Tools) readBitable(ctx context.Context, creds InstallationCredentials, appToken string) (string, error) {
	tables, err := t.client.ListBitableTables(ctx, creds, appToken)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, table := range tables {
		fields, err := t.client.ListBitableFields(ctx, creds, appToken, table.TableID)
		if err != nil {
			return "", fmt.Errorf("table %q: %w", table.Name, err)
		}
		fmt.Fprintf(&b, "## %s\n%s\n", table.Name, strings.Join(fields, "\t"))
		page, count := "", 0
		for {
			records, next, err := t.client.ListBitableRecords(ctx, creds, appToken, table.TableID, page)
			if err != nil {
				return "", fmt.Errorf("table %q: %w", table.Name, err)
			}
			for _, rec := range records {
				cells := make([]string, len(fields))
				for i, f := range fields {
					cells[i] = strings.NewReplacer("\t", " ", "\n", " ").Replace(sheetCellString(rec[f]))
				}
				b.WriteString(strings.Join(cells, "\t"))
				b.WriteByte('\n')
			}
			count += len(records)
			if next == "" || count >= maxBitableRecords || b.Len() > maxDocChars*4 {
				break
			}
			page = next
		}
		b.WriteByte('\n')
		if b.Len() > maxDocChars*4 {
			break
		}
	}
	return b.String(), nil
}

func truncateRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	runes := []rune(s)
	return string(runes[:max]), true
}

// ---- chats ----

// maxListedChats bounds a chat listing across pages.
const maxListedChats = 500

// ListChats lists the group chats the bot is in.
func (t *Tools) ListChats(ctx context.Context, scope ToolScope) ([]ChatInfo, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := []ChatInfo{}
	page := ""
	for {
		items, next, err := t.client.ListChats(ctx, tc.creds, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || len(out) >= maxListedChats {
			return out, nil
		}
		page = next
	}
}

// ListMembers lists a chat's members; an empty chatID means the current chat.
func (t *Tools) ListMembers(ctx context.Context, scope ToolScope, chatID string) ([]ChatMember, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	if chatID == "" {
		if tc.current == nil {
			return nil, ErrToolNoTarget
		}
		chatID = tc.current.ChatID
	}
	out := []ChatMember{}
	page := ""
	for {
		items, next, err := t.client.ListChatMembers(ctx, tc.creds, chatID, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || len(out) >= maxListedChats {
			return out, nil
		}
		page = next
	}
}

// CreateGroupInput is one `multica lark group create`.
type CreateGroupInput struct {
	Name             string
	Description      string
	Members          []string
	IncludeRequester bool
}

// CreateGroup creates a group chat owned by the bot.
func (t *Tools) CreateGroup(ctx context.Context, scope ToolScope, in CreateGroupInput) (ChatInfo, error) {
	if strings.TrimSpace(in.Name) == "" {
		return ChatInfo{}, invalidInput("a group needs a name")
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return ChatInfo{}, err
	}
	members, err := tc.mentions(in.Members, in.IncludeRequester)
	if err != nil {
		return ChatInfo{}, err
	}
	return t.client.CreateChat(ctx, tc.creds, CreateChatParams{Name: strings.TrimSpace(in.Name), Description: in.Description, Members: members})
}

// AddMembers invites users into a chat; an empty chatID means the current chat.
// It returns the open_ids Lark rejected.
func (t *Tools) AddMembers(ctx context.Context, scope ToolScope, chatID string, members []string) ([]string, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	if chatID == "" {
		if tc.current == nil {
			return nil, ErrToolNoTarget
		}
		chatID = tc.current.ChatID
	}
	ids, err := tc.mentions(members, false)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, invalidInput("no members to add")
	}
	return t.client.AddChatMembers(ctx, tc.creds, chatID, ids)
}

// ---- document creation ----

// maxDocMarkdownBytes bounds the markdown one create call converts.
const maxDocMarkdownBytes = 1 << 20

// docLinkShares are the link-sharing modes a created doc may use.
var docLinkShares = map[string]bool{
	"tenant_readable": true, "tenant_editable": true,
	"anyone_readable": true, "anyone_editable": true, "closed": true,
}

// CreateDocInput is one `multica lark doc create`.
type CreateDocInput struct {
	Title    string
	Markdown string
	// ShareWith are open_ids granted edit access; the requester is added
	// with full access unless NoRequester is set.
	ShareWith   []string
	NoRequester bool
	// LinkShare is the link-sharing mode; empty means "tenant_readable" so
	// everyone in the organization with the link can read it.
	LinkShare string
}

// CreatedDoc is a new Feishu document.
type CreatedDoc struct {
	DocumentID string `json:"document_id"`
	URL        string `json:"url"`
	// ShareWarning explains a sharing step that failed; the document itself
	// exists and is owned by the bot.
	ShareWarning string `json:"share_warning,omitempty"`
}

// CreateDoc creates a Feishu document from markdown, owned by the bot, and
// shares it so people can open it.
func (t *Tools) CreateDoc(ctx context.Context, scope ToolScope, in CreateDocInput) (CreatedDoc, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return CreatedDoc{}, invalidInput("a document needs a title")
	}
	if len(in.Markdown) > maxDocMarkdownBytes {
		return CreatedDoc{}, invalidInput("the document content is over 1 MiB")
	}
	linkShare := in.LinkShare
	if linkShare == "" {
		linkShare = "tenant_readable"
	}
	if !docLinkShares[linkShare] {
		return CreatedDoc{}, invalidInput("unknown link sharing %q", linkShare)
	}
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return CreatedDoc{}, err
	}
	editors, err := tc.mentions(in.ShareWith, false)
	if err != nil {
		return CreatedDoc{}, err
	}
	docID, err := t.client.CreateDocFromMarkdown(ctx, tc.creds, title, in.Markdown)
	if err != nil {
		if docID != "" {
			return CreatedDoc{}, fmt.Errorf("document %s was created but filling it failed: %w", docID, err)
		}
		return CreatedDoc{}, err
	}
	out := CreatedDoc{DocumentID: docID}
	var warnings []string
	if !in.NoRequester && tc.current != nil && tc.current.RequesterOpenID != "" {
		if err := t.client.ShareDoc(ctx, tc.creds, docID, []string{tc.current.RequesterOpenID}, "full_access", ""); err != nil {
			warnings = append(warnings, "sharing with the requester failed: "+err.Error())
		}
	}
	if err := t.client.ShareDoc(ctx, tc.creds, docID, editors, "edit", linkShare); err != nil {
		warnings = append(warnings, "sharing failed: "+err.Error())
	}
	out.ShareWarning = strings.Join(warnings, "; ")
	if u, err := t.client.DocURL(ctx, tc.creds, docID); err == nil {
		out.URL = u
	} else {
		out.URL = fallbackDocURL(tc.inst.Region, docID)
	}
	return out, nil
}

// fallbackDocURL builds a document link when Lark does not return one. The
// generic host redirects to the tenant's own domain.
func fallbackDocURL(region, documentID string) string {
	if RegionOrDefault(region) == RegionLark {
		return "https://www.larksuite.com/docx/" + documentID
	}
	return "https://www.feishu.cn/docx/" + documentID
}
