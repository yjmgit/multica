package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var larkCmd = &cobra.Command{
	Use:   "lark",
	Short: "Act in Feishu (飞书) through this agent's bot",
	Long: `Act in Feishu through the Feishu bot of the agent running this task: send
messages and files, schedule reminders, read Feishu docs and sheets, and manage
group chats.

Only available inside an agent task (it uses the task token). When the task was
started from a Feishu conversation, that conversation is the default target;
otherwise pass --chat <chat_id> or --user <open_id>. Run "multica lark context"
to see the current conversation and who asked.`,
}

var larkContextCmd = &cobra.Command{
	Use:   "context",
	Short: "Show the current Feishu conversation and requester",
	Args:  cobra.NoArgs,
	RunE:  runLarkContext,
}

var larkSendCmd = &cobra.Command{
	Use:   "send [text]",
	Short: "Send a message and/or files now, or schedule a message",
	Long: `Send a message to Feishu. Text is the argument (or --text); markdown is rendered
as a card. Attach files with --file (repeatable): images are sent as images,
everything else as files (Lark limit: 30 MiB per file).

Schedule a text message instead of sending it now with --in (e.g. 2m, 1h30m) or
--at (RFC 3339 with a timezone). The server sends it at that time, even after
this task has ended. Use --mention-requester to @ the person who asked.`,
	Example: `  # Remind the requester in 2 minutes
  $ multica lark send --in 2m --mention-requester "该关煤气了"

  # Send a report file to the current conversation
  $ multica lark send "本周报告见附件" --file ./report.pdf

  # Post to a specific group at 9:00 Singapore time
  $ multica lark send --chat oc_xxx --at 2026-09-27T09:00:00+08:00 "早会开始"`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLarkSend,
}

var larkScheduledCmd = &cobra.Command{
	Use:   "scheduled",
	Short: "List or cancel this agent's scheduled messages",
}

var larkScheduledListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending scheduled messages",
	Args:  cobra.NoArgs,
	RunE:  runLarkScheduledList,
}

var larkScheduledCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a pending scheduled message",
	Args:  exactArgs(1),
	RunE:  runLarkScheduledCancel,
}

var larkDocCmd = &cobra.Command{
	Use:   "doc <url>",
	Short: "Read a Feishu doc, wiki page, sheet or base as text (or: doc create)",
	Long: `Read a Feishu document link (…/docx/…, …/docs/…, …/wiki/…, …/sheets/…, …/base/…)
as plain text. Sheets and bases (多维表格) are returned as tab-separated rows.
Use "multica lark doc create" to write a new document. The bot can only read documents that
are shared with it (or with the whole organization, if the app has that scope).`,
	Args: exactArgs(1),
	RunE: runLarkDoc,
}

var larkDocCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a Feishu doc from markdown and share it",
	Long: `Create a Feishu document owned by the bot from markdown (headings, lists, tables,
code blocks, links; images are not carried over). The person who asked gets full
access and, by default, everyone in the organization can read it via the link.
Prints the document URL — send it in your reply or with "multica lark send".`,
	Example: `  # Write a report file into a doc
  $ multica lark doc create --title "本周周报" --file ./report.md

  # Let two colleagues edit, keep the link private
  $ multica lark doc create --title "方案" --file ./plan.md --share-with ou_aaa --share-with ou_bbb --link off`,
	Args: cobra.NoArgs,
	RunE: runLarkDocCreate,
}

var larkWakeupCmd = &cobra.Command{
	Use:   "wakeup <instructions>",
	Short: "Run this agent again later (once or on a schedule) and post the result to Feishu",
	Long: `Schedule this agent to run again with the given instructions — once (--in / --at)
or repeatedly (--cron) — and post its result into the Feishu conversation.

Unlike "send --in", which only posts fixed text, a wake-up is a real agent run:
use it for "every morning at 9 summarize ...", "tomorrow at 3pm check ... and
tell me". It is stored as a run_only autopilot, so it shows up on the web
Autopilots page and "multica autopilot list/delete" manage it.`,
	Example: `  # Every weekday at 9:00 Singapore time
  $ multica lark wakeup --cron "0 9 * * 1-5" "汇总昨天群里讨论的要点"

  # Once, in 2 hours, and @ the person who asked
  $ multica lark wakeup --in 2h --mention-requester "检查一下部署是否完成"`,
	Args: exactArgs(1),
	RunE: runLarkWakeup,
}

var larkDelegateCmd = &cobra.Command{
	Use:   "delegate <instructions>",
	Short: "Hand work to another agent; its result is posted back to this Feishu chat",
	Long: `Hand work to another agent from a Feishu conversation. Feishu does not deliver
one bot's messages to another bot, so the work goes through a Multica issue
assigned to that agent. Each time a run on the issue finishes, the comment the
run posted is sent back to this Feishu chat through your bot (@-mentioning the
person who asked) — the other agent does not need a Feishu bot. A failed run is
reported too.

The agent may live in another workspace: any workspace the person who asked
belongs to works. Name it with --workspace, or leave it out and an agent not
found here is looked up in all of them.

Write the instructions so the other agent can work without this conversation:
include the goal, inputs and what to deliver.`,
	Example: `  $ multica lark delegate --to "数据清洗助手" "清洗附件里的销售表：去重、统一日期格式，结果写成新的 CSV"
  $ multica lark delegate --workspace ai-data-triage --to "EDN 数据开发助手" "查一下 prod 昨天新增的接口"`,
	Args: exactArgs(1),
	RunE: runLarkDelegate,
}

var larkFollowAutopilotCmd = &cobra.Command{
	Use:   "follow-autopilot <autopilot-id>",
	Short: "Post each run result of an existing Autopilot to this Feishu chat",
	Long: `Send the result of every future run of an Autopilot to this Feishu chat
(@-mentioning the person who asked): the comment the run posts on its issue, or
the run's output when it creates no issue. A failed run is reported too.

An Autopilot you create with "multica autopilot create" from a Feishu
conversation already reports here; use this for one that exists already.`,
	Example: `  $ multica lark follow-autopilot 0b6f3c7e-1a2b-4c5d-8e9f-0123456789ab`,
	Args:    exactArgs(1),
	RunE:    runLarkFollowAutopilot,
}

var larkChatsCmd = &cobra.Command{
	Use:   "chats",
	Short: "List the group chats the bot is in",
	Args:  cobra.NoArgs,
	RunE:  runLarkChats,
}

var larkMembersCmd = &cobra.Command{
	Use:   "members",
	Short: "List a chat's members with their open_ids",
	Long: `List the members of a chat (the current one by default) with their open_ids.
Use it to turn names into open_ids for --mention, "group create" and "group add".`,
	Args: cobra.NoArgs,
	RunE: runLarkMembers,
}

var larkGroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Create group chats and add members",
}

var larkGroupCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a group chat owned by the bot",
	Example: `  # Create a group with the requester and two colleagues
  $ multica lark group create "项目A沟通群" --include-requester --member ou_aaa --member ou_bbb`,
	Args: exactArgs(1),
	RunE: runLarkGroupCreate,
}

var larkGroupAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add members to a group chat (the current one by default)",
	Args:  cobra.NoArgs,
	RunE:  runLarkGroupAdd,
}

func init() {
	addLarkSendFlags(larkSendCmd)

	larkDocCreateCmd.Flags().String("title", "", "Document title (required)")
	larkDocCreateCmd.Flags().String("file", "", "Markdown file with the document content")
	larkDocCreateCmd.Flags().String("content", "", "Markdown content (alternative to --file)")
	larkDocCreateCmd.Flags().StringArray("share-with", nil, "open_id to grant edit access (repeatable)")
	larkDocCreateCmd.Flags().Bool("no-requester", false, "Do not grant the requester access")
	larkDocCreateCmd.Flags().String("link", "tenant", "Link sharing: tenant, tenant-edit, anyone, anyone-edit or off")
	larkDocCmd.AddCommand(larkDocCreateCmd)

	addLarkWakeupFlags(larkWakeupCmd)

	larkDelegateCmd.Flags().String("to", "", "Agent to hand the work to (name or ID, required)")
	larkDelegateCmd.Flags().String("title", "", "Issue title (default: the start of the instructions)")
	larkDelegateCmd.Flags().String("workspace", "", "Workspace of the agent (slug, name or ID); default: this one, then every workspace the requester belongs to")

	larkMembersCmd.Flags().String("chat", "", "chat_id (default: the current conversation)")

	larkGroupCreateCmd.Flags().String("description", "", "Group description")
	larkGroupCreateCmd.Flags().StringArray("member", nil, "open_id to add (repeatable)")
	larkGroupCreateCmd.Flags().Bool("include-requester", false, "Add the person whose message started this task")

	larkGroupAddCmd.Flags().String("chat", "", "chat_id (default: the current conversation)")
	larkGroupAddCmd.Flags().StringArray("member", nil, "open_id to add (repeatable)")

	larkScheduledCmd.AddCommand(larkScheduledListCmd, larkScheduledCancelCmd)
	larkGroupCmd.AddCommand(larkGroupCreateCmd, larkGroupAddCmd)
	larkCmd.AddCommand(larkContextCmd, larkSendCmd, larkScheduledCmd, larkDocCmd, larkChatsCmd, larkMembersCmd, larkGroupCmd, larkWakeupCmd, larkDelegateCmd, larkFollowAutopilotCmd)
}

// addLarkSendFlags registers the send flags; tests build a fresh command with it.
func addLarkSendFlags(cmd *cobra.Command) {
	cmd.Flags().String("text", "", "Message text (alternative to the positional argument)")
	cmd.Flags().StringArray("file", nil, "File to send (repeatable)")
	cmd.Flags().String("chat", "", "Target chat_id (default: the current conversation)")
	cmd.Flags().String("user", "", "Target user open_id (sends a direct message from the bot)")
	cmd.Flags().StringArray("mention", nil, "open_id to @-mention (repeatable)")
	cmd.Flags().Bool("mention-requester", false, "@-mention the person whose message started this task")
	cmd.Flags().String("in", "", "Schedule the message after this delay, e.g. 90s, 2m, 1h30m")
	cmd.Flags().String("at", "", "Schedule the message at this time (RFC 3339, e.g. 2026-09-27T09:00:00+08:00)")
}

// addLarkWakeupFlags registers the wakeup flags; tests build a fresh command with it.
func addLarkWakeupFlags(cmd *cobra.Command) {
	cmd.Flags().String("in", "", "Run once after this delay, e.g. 30m, 2h")
	cmd.Flags().String("at", "", "Run once at this time (RFC 3339, e.g. 2026-09-28T15:00:00+08:00)")
	cmd.Flags().String("cron", "", "Run on this cron schedule, e.g. \"0 9 * * 1-5\"")
	cmd.Flags().String("timezone", "Asia/Singapore", "IANA timezone for --cron")
	cmd.Flags().String("title", "", "Title shown on the Autopilots page (default: the instructions)")
	cmd.Flags().String("chat", "", "chat_id to post the result to (default: the current conversation)")
	cmd.Flags().String("user", "", "open_id to post the result to as a direct message")
	cmd.Flags().Bool("mention-requester", false, "@-mention the person whose message started this task in the result")
}

func larkGet(cmd *cobra.Command, path string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp map[string]any
	if err := client.GetJSON(ctx, path, &resp); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, resp)
}

func larkPost(cmd *cobra.Command, path string, body any) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp map[string]any
	if err := client.PostJSON(ctx, path, body, &resp); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, resp)
}

func runLarkContext(cmd *cobra.Command, _ []string) error {
	return larkGet(cmd, "/api/lark/context")
}

func runLarkSend(cmd *cobra.Command, args []string) error {
	text, _ := cmd.Flags().GetString("text")
	if len(args) == 1 {
		if text != "" {
			return fmt.Errorf("pass the text either as an argument or with --text, not both")
		}
		text = args[0]
	}
	paths, _ := cmd.Flags().GetStringArray("file")
	chat, _ := cmd.Flags().GetString("chat")
	user, _ := cmd.Flags().GetString("user")
	mentions, _ := cmd.Flags().GetStringArray("mention")
	mentionRequester, _ := cmd.Flags().GetBool("mention-requester")
	delay, _ := cmd.Flags().GetString("in")
	at, _ := cmd.Flags().GetString("at")
	if strings.TrimSpace(text) == "" && len(paths) == 0 {
		return fmt.Errorf("nothing to send: provide text or --file")
	}

	fields := map[string][]string{}
	set := func(k, v string) {
		if v != "" {
			fields[k] = []string{v}
		}
	}
	set("text", text)
	set("chat_id", chat)
	set("open_id", user)
	set("delay", delay)
	set("send_at", at)
	if mentionRequester {
		set("mention_requester", "true")
	}
	if len(mentions) > 0 {
		fields["mention"] = mentions
	}
	var files []cli.MultipartFile
	for _, p := range paths {
		if isHTTPURL(p) {
			return fmt.Errorf("--file takes a local path, not a URL: %s", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read file %s: %w", p, err)
		}
		files = append(files, cli.MultipartFile{Field: "file", Filename: p, Data: data})
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cli.AtLeastAPITimeout(2*time.Minute))
	defer cancel()
	var resp map[string]any
	if err := client.PostMultipart(ctx, "/api/lark/send", fields, files, &resp); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, resp)
}

func runLarkScheduledList(cmd *cobra.Command, _ []string) error {
	return larkGet(cmd, "/api/lark/scheduled")
}

func runLarkScheduledCancel(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp map[string]any
	if err := client.DeleteJSONResponse(ctx, "/api/lark/scheduled/"+url.PathEscape(args[0]), &resp); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, resp)
}

func runLarkDoc(cmd *cobra.Command, args []string) error {
	return larkGet(cmd, "/api/lark/doc?url="+url.QueryEscape(args[0]))
}

// larkLinkShares maps the --link values onto Lark's link_share_entity.
var larkLinkShares = map[string]string{
	"tenant": "tenant_readable", "tenant-edit": "tenant_editable",
	"anyone": "anyone_readable", "anyone-edit": "anyone_editable", "off": "closed",
}

func runLarkDocCreate(cmd *cobra.Command, _ []string) error {
	title, _ := cmd.Flags().GetString("title")
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("--title is required")
	}
	file, _ := cmd.Flags().GetString("file")
	content, _ := cmd.Flags().GetString("content")
	if file != "" && content != "" {
		return fmt.Errorf("pass either --file or --content, not both")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read file %s: %w", file, err)
		}
		content = string(data)
	}
	link, _ := cmd.Flags().GetString("link")
	linkShare, ok := larkLinkShares[link]
	if !ok {
		return fmt.Errorf("--link must be tenant, tenant-edit, anyone, anyone-edit or off")
	}
	shareWith, _ := cmd.Flags().GetStringArray("share-with")
	noRequester, _ := cmd.Flags().GetBool("no-requester")
	return larkPost(cmd, "/api/lark/docs", map[string]any{
		"title":        title,
		"markdown":     content,
		"share_with":   shareWith,
		"no_requester": noRequester,
		"link_share":   linkShare,
	})
}

// wakeupDeliveryFlags renders the `multica lark send` target flags the
// woken-up run must use to deliver its result.
func wakeupDeliveryFlags(chat, user, mention string) string {
	flags := "--chat " + chat
	if user != "" {
		flags = "--user " + user
	}
	if mention != "" {
		flags += " --mention " + mention
	}
	return flags
}

// wakeupPrompt appends delivery instructions to the user's instructions: an
// autopilot run has no Feishu conversation of its own, so its answer reaches
// Feishu only if the run sends it.
func wakeupPrompt(instructions, deliveryFlags string) string {
	return strings.TrimSpace(instructions) + "\n\n---\n" +
		"This run was scheduled from a Feishu conversation. Nothing you write in this run reaches Feishu on its own — when you are done, deliver the result yourself, exactly once:\n" +
		"  multica lark send " + deliveryFlags + " \"<your result>\"\n" +
		"Attach files with --file <path>, or put a long result into a doc with `multica lark doc create` and send its link.\n"
}

func runLarkWakeup(cmd *cobra.Command, args []string) error {
	instructions := strings.TrimSpace(args[0])
	if instructions == "" {
		return fmt.Errorf("the instructions are empty")
	}
	delay, _ := cmd.Flags().GetString("in")
	at, _ := cmd.Flags().GetString("at")
	cron, _ := cmd.Flags().GetString("cron")
	set := 0
	for _, v := range []string{delay, at, cron} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("pass exactly one of --in, --at or --cron")
	}
	chat, _ := cmd.Flags().GetString("chat")
	user, _ := cmd.Flags().GetString("user")
	if chat != "" && user != "" {
		return fmt.Errorf("pass either --chat or --user, not both")
	}
	mentionRequester, _ := cmd.Flags().GetBool("mention-requester")

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if client.AgentID == "" {
		return fmt.Errorf("no agent in context: run this inside an agent task")
	}
	if _, err := requireWorkspaceID(cmd); err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var info struct {
		CurrentChat *struct {
			ChatID          string `json:"chat_id"`
			RequesterOpenID string `json:"requester_open_id"`
		} `json:"current_chat"`
	}
	if err := client.GetJSON(ctx, "/api/lark/context", &info); err != nil {
		return err
	}
	if chat == "" && user == "" {
		if info.CurrentChat == nil {
			return fmt.Errorf("no target: pass --chat <chat_id> or --user <open_id> (this task is not running in a Feishu chat)")
		}
		chat = info.CurrentChat.ChatID
	}
	mention := ""
	if mentionRequester {
		if info.CurrentChat == nil || info.CurrentChat.RequesterOpenID == "" {
			return fmt.Errorf("there is no requester to mention: this task was not started from a Feishu message")
		}
		mention = info.CurrentChat.RequesterOpenID
	}

	title, _ := cmd.Flags().GetString("title")
	if strings.TrimSpace(title) == "" {
		title = instructions
	}
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60]) + "…"
	}
	var ap map[string]any
	if err := client.PostJSON(ctx, "/api/autopilots", map[string]any{
		"title":          title,
		"assignee_id":    client.AgentID,
		"execution_mode": "run_only",
		"description":    wakeupPrompt(instructions, wakeupDeliveryFlags(chat, user, mention)),
	}, &ap); err != nil {
		return fmt.Errorf("create autopilot: %w", err)
	}
	autopilotID := strVal(ap, "id")
	cleanup := func() {
		_ = client.DeleteJSON(context.Background(), "/api/autopilots/"+url.PathEscape(autopilotID))
	}

	result := map[string]any{"autopilot_id": autopilotID, "title": title}
	if cron != "" {
		timezone, _ := cmd.Flags().GetString("timezone")
		var trigger map[string]any
		if err := client.PostJSON(ctx, "/api/autopilots/"+url.PathEscape(autopilotID)+"/triggers", map[string]any{
			"kind": "schedule", "cron_expression": cron, "timezone": timezone, "label": "Feishu wake-up",
		}, &trigger); err != nil {
			cleanup()
			return fmt.Errorf("add schedule: %w", err)
		}
		result["cron"] = cron
		result["timezone"] = timezone
		result["next_run_at"] = trigger["next_run_at"]
	} else {
		var row map[string]any
		if err := client.PostJSON(ctx, "/api/lark/wakeups", map[string]any{
			"autopilot_id": autopilotID, "delay": delay, "send_at": at,
		}, &row); err != nil {
			cleanup()
			return err
		}
		result["run_at"] = row["fire_at"]
		result["scheduled_id"] = row["id"]
	}
	return cli.PrintJSON(os.Stdout, result)
}

// delegateDescription tells the assignee where its result goes.
func delegateDescription(instructions string) string {
	return strings.TrimSpace(instructions) + "\n\n---\n" +
		"Delegated from a Feishu conversation. Post your result as a comment on this issue — the comment each run posts is relayed to that conversation automatically, so write it for the person who asked.\n"
}

func runLarkFollowAutopilot(cmd *cobra.Command, args []string) error {
	autopilotID := strings.TrimSpace(args[0])
	if autopilotID == "" {
		return fmt.Errorf("the autopilot ID is empty")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if _, err := requireWorkspaceID(cmd); err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.PostJSON(ctx, "/api/lark/autopilot-relays", map[string]any{"autopilot_id": autopilotID}, &out); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

func runLarkDelegate(cmd *cobra.Command, args []string) error {
	instructions := strings.TrimSpace(args[0])
	if instructions == "" {
		return fmt.Errorf("the instructions are empty")
	}
	to, _ := cmd.Flags().GetString("to")
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("--to is required (agent name or ID)")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if _, err := requireWorkspaceID(cmd); err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var info struct {
		CurrentChat *struct {
			ChatID string `json:"chat_id"`
		} `json:"current_chat"`
	}
	if err := client.GetJSON(ctx, "/api/lark/context", &info); err != nil {
		return err
	}
	if info.CurrentChat == nil {
		return fmt.Errorf("delegate only works from a task running in a Feishu conversation; use \"multica issue create --assignee\" otherwise")
	}
	title, _ := cmd.Flags().GetString("title")
	workspace, _ := cmd.Flags().GetString("workspace")
	// An agent in another workspace — named with --workspace, or simply not
	// found in this one — is reached through the server, acting as the person
	// who asked in any workspace they belong to.
	crossWorkspace := func() error {
		var out map[string]any
		if err := client.PostJSON(ctx, "/api/lark/delegations", map[string]any{
			"to": to, "workspace": workspace, "title": title, "instructions": instructions,
		}, &out); err != nil {
			return err
		}
		return cli.PrintJSON(os.Stdout, out)
	}
	if strings.TrimSpace(workspace) != "" {
		return crossWorkspace()
	}
	agentID, err := resolveAgent(ctx, client, to)
	if err != nil {
		if crossErr := crossWorkspace(); crossErr != nil {
			return fmt.Errorf("resolve agent: %w", crossErr)
		}
		return nil
	}
	if agentID == client.AgentID {
		return fmt.Errorf("--to names this agent itself; do the work directly instead")
	}

	if strings.TrimSpace(title) == "" {
		title = strings.SplitN(instructions, "\n", 2)[0]
	}
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60]) + "…"
	}
	var issue map[string]any
	if err := client.PostJSON(ctx, "/api/issues", map[string]any{
		"title":         title,
		"description":   delegateDescription(instructions),
		"assignee_type": "agent",
		"assignee_id":   agentID,
	}, &issue); err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	issueID := strVal(issue, "id")
	result := map[string]any{"issue_id": issueID, "identifier": strVal(issue, "identifier"), "assignee_id": agentID}
	var relay map[string]any
	if err := client.PostJSON(ctx, "/api/lark/relays", map[string]any{"issue_id": issueID}, &relay); err != nil {
		// The issue exists and the other agent will work on it; only the
		// automatic post-back is missing.
		result["relay_error"] = err.Error()
		result["note"] = "The issue was created, but its result will not be posted back automatically; check the issue and relay it yourself."
	} else {
		result["relayed_to_chat"] = relay["chat_id"]
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runLarkChats(cmd *cobra.Command, _ []string) error {
	return larkGet(cmd, "/api/lark/chats")
}

func runLarkMembers(cmd *cobra.Command, _ []string) error {
	chat, _ := cmd.Flags().GetString("chat")
	path := "/api/lark/members"
	if chat != "" {
		path += "?chat_id=" + url.QueryEscape(chat)
	}
	return larkGet(cmd, path)
}

func runLarkGroupCreate(cmd *cobra.Command, args []string) error {
	description, _ := cmd.Flags().GetString("description")
	members, _ := cmd.Flags().GetStringArray("member")
	includeRequester, _ := cmd.Flags().GetBool("include-requester")
	return larkPost(cmd, "/api/lark/groups", map[string]any{
		"name":              args[0],
		"description":       description,
		"members":           members,
		"include_requester": includeRequester,
	})
}

func runLarkGroupAdd(cmd *cobra.Command, _ []string) error {
	chat, _ := cmd.Flags().GetString("chat")
	members, _ := cmd.Flags().GetStringArray("member")
	if len(members) == 0 {
		return fmt.Errorf("pass at least one --member <open_id>")
	}
	return larkPost(cmd, "/api/lark/groups/members", map[string]any{"chat_id": chat, "members": members})
}
