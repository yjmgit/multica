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

  # Post to a specific group at 9:00 Beijing time
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
	Short: "Read a Feishu doc, wiki page or sheet as text",
	Long: `Read a Feishu document link (…/docx/…, …/wiki/…, …/sheets/…) as plain text.
Sheets are returned as tab-separated rows. The bot can only read documents that
are shared with it (or with the whole organization, if the app has that scope).`,
	Args: exactArgs(1),
	RunE: runLarkDoc,
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

	larkMembersCmd.Flags().String("chat", "", "chat_id (default: the current conversation)")

	larkGroupCreateCmd.Flags().String("description", "", "Group description")
	larkGroupCreateCmd.Flags().StringArray("member", nil, "open_id to add (repeatable)")
	larkGroupCreateCmd.Flags().Bool("include-requester", false, "Add the person whose message started this task")

	larkGroupAddCmd.Flags().String("chat", "", "chat_id (default: the current conversation)")
	larkGroupAddCmd.Flags().StringArray("member", nil, "open_id to add (repeatable)")

	larkScheduledCmd.AddCommand(larkScheduledListCmd, larkScheduledCancelCmd)
	larkGroupCmd.AddCommand(larkGroupCreateCmd, larkGroupAddCmd)
	larkCmd.AddCommand(larkContextCmd, larkSendCmd, larkScheduledCmd, larkDocCmd, larkChatsCmd, larkMembersCmd, larkGroupCmd)
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
