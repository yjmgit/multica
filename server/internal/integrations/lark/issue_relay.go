package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Delegation relay (`multica lark delegate`). Feishu does not deliver one
// bot's messages to another bot, so agents hand work to each other through a
// Multica issue instead. The delegating agent registers the Feishu chat it was
// asked from; whenever a run on that issue finishes, the comment the run
// posted is relayed back to that chat through the delegating agent's bot —
// the assignee needs no Feishu bot of its own.

// relayTimeout bounds one relay: loading the run's context and posting it.
const relayTimeout = 30 * time.Second

// maxRelayChars bounds how much of a comment is posted; the rest stays on the
// issue, which the message links to.
const maxRelayChars = 3000

// RelayedIssue is the result of registering a delegation relay.
type RelayedIssue struct {
	IssueID string `json:"issue_id"`
	ChatID  string `json:"chat_id"`
}

// RegisterRelay records that issueID's results go back to the current
// Feishu conversation. The calling task must be running in one.
func (t *Tools) RegisterRelay(ctx context.Context, scope ToolScope, issueID pgtype.UUID) (RelayedIssue, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return RelayedIssue{}, err
	}
	if tc.current == nil {
		return RelayedIssue{}, invalidInput("delegation results can only be relayed from a task running in a Feishu conversation")
	}
	issue, err := t.queries.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelayedIssue{}, invalidInput("issue not found")
		}
		return RelayedIssue{}, err
	}
	if issue.WorkspaceID != scope.WorkspaceID {
		return RelayedIssue{}, invalidInput("issue not found")
	}
	return t.registerRelay(ctx, scope, tc, issue)
}

// RegisterCrossWorkspaceRelay is RegisterRelay for an issue in another
// workspace. The caller has already checked that the person behind the task
// may create work there.
func (t *Tools) RegisterCrossWorkspaceRelay(ctx context.Context, scope ToolScope, issue db.Issue) (RelayedIssue, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return RelayedIssue{}, err
	}
	if tc.current == nil {
		return RelayedIssue{}, invalidInput("delegation results can only be relayed from a task running in a Feishu conversation")
	}
	return t.registerRelay(ctx, scope, tc, issue)
}

func (t *Tools) registerRelay(ctx context.Context, scope ToolScope, tc toolContext, issue db.Issue) (RelayedIssue, error) {
	row, err := t.queries.UpsertChannelIssueRelay(ctx, db.UpsertChannelIssueRelayParams{
		WorkspaceID:     scope.WorkspaceID,
		InstallationID:  tc.inst.ID,
		IssueID:         issue.ID,
		ChatID:          tc.current.ChatID,
		ReplyMessageID:  tc.current.reply.MessageID,
		ReplyInThread:   tc.current.reply.InThread,
		RequesterOpenID: tc.current.RequesterOpenID,
	})
	if err != nil {
		return RelayedIssue{}, fmt.Errorf("store relay: %w", err)
	}
	return RelayedIssue{IssueID: uuidString(row.IssueID), ChatID: row.ChatID}, nil
}

// RelayedAutopilot is the result of registering an autopilot relay.
type RelayedAutopilot struct {
	AutopilotID string `json:"autopilot_id"`
	Title       string `json:"title"`
	ChatID      string `json:"chat_id"`
}

// RegisterAutopilotRelay records that each run of autopilotID reports back to
// the current Feishu conversation. The calling task must be running in one.
func (t *Tools) RegisterAutopilotRelay(ctx context.Context, scope ToolScope, autopilotID pgtype.UUID) (RelayedAutopilot, error) {
	tc, err := t.resolve(ctx, scope)
	if err != nil {
		return RelayedAutopilot{}, err
	}
	if tc.current == nil {
		return RelayedAutopilot{}, invalidInput("autopilot results can only be relayed from a task running in a Feishu conversation")
	}
	ap, err := t.queries.GetAutopilot(ctx, autopilotID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelayedAutopilot{}, invalidInput("autopilot not found")
		}
		return RelayedAutopilot{}, err
	}
	if ap.WorkspaceID != scope.WorkspaceID {
		return RelayedAutopilot{}, invalidInput("autopilot not found")
	}
	row, err := t.queries.UpsertChannelAutopilotRelay(ctx, db.UpsertChannelAutopilotRelayParams{
		WorkspaceID:     scope.WorkspaceID,
		InstallationID:  tc.inst.ID,
		AutopilotID:     ap.ID,
		ChatID:          tc.current.ChatID,
		RequesterOpenID: tc.current.RequesterOpenID,
	})
	if err != nil {
		return RelayedAutopilot{}, fmt.Errorf("store autopilot relay: %w", err)
	}
	return RelayedAutopilot{AutopilotID: uuidString(row.AutopilotID), Title: ap.Title, ChatID: row.ChatID}, nil
}

// SetAppURL sets the Multica web URL used to link relayed issues.
func (t *Tools) SetAppURL(appURL string) { t.appURL = appURL }

// RegisterIssueRelay subscribes the relay to run completion. Call once at
// boot. The bus is synchronous, so each relay runs in its own goroutine.
func (t *Tools) RegisterIssueRelay(bus *events.Bus) {
	handle := func(e events.Event) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
			defer cancel()
			if err := t.relayRun(ctx, e); err != nil {
				t.logger.Warn("lark relay: relaying run result failed", "task_id", e.TaskID, "error", err)
			}
		}()
	}
	bus.Subscribe(protocol.EventTaskCompleted, handle)
	bus.Subscribe(protocol.EventTaskFailed, handle)
}

func (t *Tools) relayRun(ctx context.Context, e events.Event) error {
	var taskID pgtype.UUID
	if err := taskID.Scan(e.TaskID); err != nil || !taskID.Valid {
		return nil
	}
	failed := e.Type == protocol.EventTaskFailed
	if failed {
		// An automatic retry follows; relay only the terminal outcome.
		if p, ok := e.Payload.(map[string]any); ok && p["retry_pending"] == true {
			return nil
		}
	}
	task, err := t.queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return nil
	}
	if task.AutopilotRunID.Valid {
		if err := t.relayAutopilotRun(ctx, task, failed); err != nil {
			return err
		}
	}
	if !task.IssueID.Valid {
		return nil
	}
	relays, err := t.queries.ListActiveChannelIssueRelaysByIssue(ctx, task.IssueID)
	if err != nil || len(relays) == 0 {
		return err
	}

	body := ""
	if !failed {
		comment, err := t.queries.GetLatestIssueCommentByTask(ctx, db.GetLatestIssueCommentByTaskParams{
			IssueID: task.IssueID, SourceTaskID: task.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The run left no comment; there is nothing to report.
			return nil
		}
		if err != nil {
			return fmt.Errorf("load run comment: %w", err)
		}
		body = strings.TrimSpace(comment.Content)
		if body == "" {
			return nil
		}
	}
	text, err := t.relayText(ctx, task, failed, body)
	if err != nil {
		return err
	}

	var firstErr error
	for _, relay := range relays {
		claimed, err := t.queries.ClaimChannelIssueRelayTask(ctx, db.ClaimChannelIssueRelayTaskParams{ID: relay.ID, TaskID: task.ID})
		if err != nil || claimed == 0 {
			if err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := t.postRelay(ctx, relay, text); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// relayText renders the message: who did the work, on which issue, and the
// run's comment (or that the run failed), plus a link back to the issue.
func (t *Tools) relayText(ctx context.Context, task db.AgentTaskQueue, failed bool, body string) (string, error) {
	issue, err := t.queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		return "", fmt.Errorf("load issue: %w", err)
	}
	agentName := "智能体"
	if agent, err := t.queries.GetAgent(ctx, task.AgentID); err == nil && agent.Name != "" {
		agentName = agent.Name
	}
	identifier, link := "", ""
	if ws, err := t.queries.GetWorkspace(ctx, issue.WorkspaceID); err == nil {
		identifier = service.IssueIdentifier(ws.IssuePrefix, issue.Number)
		link = channel.IssueWebLink(t.appURL, ws.Slug, identifier)
	}
	ref := "「" + issue.Title + "」"
	if identifier != "" {
		ref = identifier + " " + ref
	}
	var b strings.Builder
	if failed {
		fmt.Fprintf(&b, "**%s** 处理 %s 时失败了。", agentName, ref)
	} else {
		fmt.Fprintf(&b, "**%s** 更新了 %s：\n\n", agentName, ref)
		if trimmed, cut := truncateRunes(body, maxRelayChars); cut {
			b.WriteString(trimmed + "…\n\n（内容较长，完整内容见任务）")
		} else {
			b.WriteString(body)
		}
	}
	if link != "" {
		fmt.Fprintf(&b, "\n\n[在 Multica 查看](%s)", link)
	}
	return b.String(), nil
}

func (t *Tools) postRelay(ctx context.Context, relay db.ChannelIssueRelay, text string) error {
	inst, err := t.store.GetLarkInstallation(ctx, relay.InstallationID)
	if err != nil {
		return fmt.Errorf("load installation: %w", err)
	}
	if inst.WorkspaceID != relay.WorkspaceID || InstallationStatus(inst.Status) != InstallationActive {
		return nil
	}
	creds, err := t.installationCredentials(inst)
	if err != nil {
		return err
	}
	var mentions []string
	if relay.RequesterOpenID != "" {
		mentions = []string{relay.RequesterOpenID}
	}
	msgType, content, err := textMessage(text, mentions)
	if err != nil {
		return err
	}
	target := MessageTarget{ChatID: relay.ChatID}
	if relay.ReplyMessageID != "" {
		target.Reply = ReplyTarget{MessageID: relay.ReplyMessageID, InThread: relay.ReplyInThread}
	}
	_, err = t.client.SendMessage(ctx, creds, target, msgType, content)
	return err
}

// relayAutopilotRun posts one autopilot run's result to every chat following
// the autopilot: the comment the run left on its issue, or the run's output
// when it has no issue (run_only).
func (t *Tools) relayAutopilotRun(ctx context.Context, task db.AgentTaskQueue, failed bool) error {
	run, err := t.queries.GetAutopilotRun(ctx, task.AutopilotRunID)
	if err != nil {
		return nil
	}
	relays, err := t.queries.ListChannelAutopilotRelaysByAutopilot(ctx, run.AutopilotID)
	if err != nil || len(relays) == 0 {
		return err
	}
	ap, err := t.queries.GetAutopilot(ctx, run.AutopilotID)
	if err != nil {
		return fmt.Errorf("load autopilot: %w", err)
	}

	body := ""
	if !failed {
		if task.IssueID.Valid {
			comment, err := t.queries.GetLatestIssueCommentByTask(ctx, db.GetLatestIssueCommentByTaskParams{
				IssueID: task.IssueID, SourceTaskID: task.ID,
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("load run comment: %w", err)
			}
			body = strings.TrimSpace(comment.Content)
		}
		if body == "" {
			var result struct {
				Output string `json:"output"`
			}
			_ = json.Unmarshal(task.Result, &result)
			body = strings.TrimSpace(result.Output)
		}
		if body == "" {
			return nil
		}
	}
	text := t.autopilotRelayText(ctx, task, ap, failed, body)

	var firstErr error
	for _, relay := range relays {
		claimed, err := t.queries.ClaimChannelAutopilotRelayTask(ctx, db.ClaimChannelAutopilotRelayTaskParams{ID: relay.ID, TaskID: task.ID})
		if err != nil || claimed == 0 {
			if err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		target := db.ChannelIssueRelay{
			WorkspaceID: relay.WorkspaceID, InstallationID: relay.InstallationID,
			ChatID: relay.ChatID, RequesterOpenID: relay.RequesterOpenID,
		}
		if err := t.postRelay(ctx, target, text); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *Tools) autopilotRelayText(ctx context.Context, task db.AgentTaskQueue, ap db.Autopilot, failed bool, body string) string {
	agentName := "智能体"
	if agent, err := t.queries.GetAgent(ctx, task.AgentID); err == nil && agent.Name != "" {
		agentName = agent.Name
	}
	link := ""
	if task.IssueID.Valid {
		if issue, err := t.queries.GetIssue(ctx, task.IssueID); err == nil {
			if ws, err := t.queries.GetWorkspace(ctx, issue.WorkspaceID); err == nil {
				link = channel.IssueWebLink(t.appURL, ws.Slug, service.IssueIdentifier(ws.IssuePrefix, issue.Number))
			}
		}
	}
	var b strings.Builder
	if failed {
		fmt.Fprintf(&b, "**%s** 的自动化「%s」这次运行失败了。", agentName, ap.Title)
	} else {
		fmt.Fprintf(&b, "**%s** 的自动化「%s」运行完成：\n\n", agentName, ap.Title)
		if trimmed, cut := truncateRunes(body, maxRelayChars); cut {
			b.WriteString(trimmed + "…\n\n（内容较长，完整内容见任务）")
		} else {
			b.WriteString(body)
		}
	}
	if link != "" {
		fmt.Fprintf(&b, "\n\n[在 Multica 查看](%s)", link)
	}
	return b.String()
}

// DelegateDescription tells the assignee of a delegated issue where its
// result goes.
func DelegateDescription(instructions string) string {
	return strings.TrimSpace(instructions) + "\n\n---\n" +
		"Delegated from a Feishu conversation. Post your result as a comment on this issue — the comment each run posts is relayed to that conversation automatically, so write it for the person who asked.\n"
}
