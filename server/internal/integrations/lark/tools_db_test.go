package lark

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/dispatch"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeToolClient records every ToolAPIClient call.
type fakeToolClient struct {
	mu    sync.Mutex
	sends []fakeToolSend
	files []string
	imgs  []string
	docs  map[string]string
	wiki  map[string]WikiNode
	chats []CreateChatParams
	fail  error

	legacyDocs map[string]string
	tables     []BitableTable
	fields     []string
	records    [][]map[string]any // pages
	createdMD  string
	shares     []string // "perm:open_id" or "link:<mode>"
}

type fakeToolSend struct {
	target  MessageTarget
	msgType string
	content string
}

func (f *fakeToolClient) UploadImage(_ context.Context, _ InstallationCredentials, name string, _ []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imgs = append(f.imgs, name)
	return "img_" + name, nil
}

func (f *fakeToolClient) UploadFile(_ context.Context, _ InstallationCredentials, name string, _ []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, name)
	return "file_" + name, nil
}

func (f *fakeToolClient) SendMessage(_ context.Context, _ InstallationCredentials, target MessageTarget, msgType, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.sends = append(f.sends, fakeToolSend{target: target, msgType: msgType, content: content})
	return "om_sent", nil
}

func (f *fakeToolClient) GetDocRawContent(_ context.Context, _ InstallationCredentials, id string) (string, error) {
	return f.docs[id], nil
}

func (f *fakeToolClient) GetWikiNode(_ context.Context, _ InstallationCredentials, token string) (WikiNode, error) {
	return f.wiki[token], nil
}

func (f *fakeToolClient) CreateChat(_ context.Context, _ InstallationCredentials, p CreateChatParams) (ChatInfo, error) {
	f.chats = append(f.chats, p)
	return ChatInfo{ChatID: "oc_created", Name: p.Name}, nil
}

func (f *fakeToolClient) AddChatMembers(context.Context, InstallationCredentials, string, []string) ([]string, error) {
	return nil, nil
}

func (f *fakeToolClient) ListChats(context.Context, InstallationCredentials, string) ([]ChatInfo, string, error) {
	return nil, "", nil
}

func (f *fakeToolClient) ListSheets(context.Context, InstallationCredentials, string) ([]SheetInfo, error) {
	return nil, nil
}

func (f *fakeToolClient) GetSheetValues(context.Context, InstallationCredentials, string, string) ([][]string, error) {
	return nil, nil
}

func (f *fakeToolClient) ListChatMembers(context.Context, InstallationCredentials, string, string) ([]ChatMember, string, error) {
	return nil, "", nil
}

func (f *fakeToolClient) GetLegacyDocRawContent(_ context.Context, _ InstallationCredentials, token string) (string, error) {
	return f.legacyDocs[token], nil
}

func (f *fakeToolClient) ListBitableTables(context.Context, InstallationCredentials, string) ([]BitableTable, error) {
	return f.tables, nil
}

func (f *fakeToolClient) ListBitableFields(context.Context, InstallationCredentials, string, string) ([]string, error) {
	return f.fields, nil
}

func (f *fakeToolClient) ListBitableRecords(_ context.Context, _ InstallationCredentials, _, _, page string) ([]map[string]any, string, error) {
	i := 0
	if page != "" {
		i = int(page[0] - '0')
	}
	next := ""
	if i+1 < len(f.records) {
		next = string(rune('0' + i + 1))
	}
	return f.records[i], next, nil
}

func (f *fakeToolClient) CreateDocFromMarkdown(_ context.Context, _ InstallationCredentials, _, markdown string) (string, error) {
	f.createdMD = markdown
	return "doxcnNew", nil
}

func (f *fakeToolClient) ShareDoc(_ context.Context, _ InstallationCredentials, _ string, openIDs []string, perm, linkShare string) error {
	for _, id := range openIDs {
		f.shares = append(f.shares, perm+":"+id)
	}
	if linkShare != "" {
		f.shares = append(f.shares, "link:"+linkShare)
	}
	return nil
}

func (f *fakeToolClient) DocURL(context.Context, InstallationCredentials, string) (string, error) {
	return "https://acme.feishu.cn/docx/doxcnNew", nil
}

// fakeDispatcher records autopilot dispatches.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []pgtype.UUID // actor user ids
	run   db.AutopilotRun
}

func (d *fakeDispatcher) DispatchAutopilotManual(_ context.Context, _ db.Autopilot, _ pgtype.UUID, _ []byte, actor pgtype.UUID) (*db.AutopilotRun, dispatch.ReasonCode, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, actor)
	run := d.run
	return &run, "", nil
}

type plainSecret struct{}

func (plainSecret) DecryptAppSecret(Installation) (string, error) { return "secret", nil }

type toolsFixture struct {
	fx        *dbfx.Fixture
	agentID   string
	pool      *pgxpool.Pool
	tools     *Tools
	client    *fakeToolClient
	chatScope ToolScope // task started from a Feishu group message
	bareScope ToolScope // task with no Feishu conversation (e.g. autopilot)
}

func newToolsFixture(t *testing.T) toolsFixture {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is required for the Feishu tools integration test")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	asUUID := func(s string) pgtype.UUID {
		var u pgtype.UUID
		if err := u.Scan(s); err != nil {
			t.Fatal(err)
		}
		return u
	}

	fx := dbfx.New(pool, "", "")
	suffix := uuid.NewString()
	fx.UserID = fx.User(t, "Tools owner", "tools-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "Tools workspace", "tools-"+suffix)
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	agentID := fx.Agent(t, "Tools agent", "")
	installationID := fx.Insert(t, "channel_installation", dbfx.Cols{
		"workspace_id": fx.WorkspaceID, "agent_id": agentID, "channel_type": channelTypeFeishu,
		"config": dbfx.Raw(`'{"app_id":"cli_tools"}'::jsonb`), "status": "active", "installer_user_id": fx.UserID,
	})
	sessionID := fx.ChatSession(t, agentID)
	bindingID := fx.Insert(t, "channel_chat_session_binding", dbfx.Cols{
		"chat_session_id": sessionID, "installation_id": installationID, "channel_type": channelTypeFeishu,
		"channel_chat_id": "oc_group", "chat_type": "group",
	})
	chatTaskID := fx.Task(t, agentID, dbfx.Cols{"status": "completed", "completed_at": dbfx.Raw("now()"), "originator_user_id": fx.UserID, "accountable_user_id": fx.UserID})
	fx.InsertNoID(t, "channel_task_delivery", dbfx.Cols{
		"task_id": chatTaskID, "binding_id": bindingID, "installation_id": installationID,
		"channel_type": channelTypeFeishu, "channel_chat_id": "oc_group", "chat_type": "group",
		"channel_message_id": "om_trigger", "channel_sender_id": "ou_asker", "route_revision": 1,
	}, "task_id = $1", chatTaskID)
	bareTaskID := fx.Task(t, agentID, dbfx.Cols{"status": "completed", "completed_at": dbfx.Raw("now()")})
	fx.Cleanup(t, `DELETE FROM channel_scheduled_message WHERE workspace_id = $1`, fx.WorkspaceID)

	client := &fakeToolClient{docs: map[string]string{}, wiki: map[string]WikiNode{}, legacyDocs: map[string]string{}}
	tools := NewTools(db.New(pool), plainSecret{}, client, newDiscardLogger())
	ws, agent := asUUID(fx.WorkspaceID), asUUID(agentID)
	return toolsFixture{
		fx:        fx,
		agentID:   agentID,
		pool:      pool,
		tools:     tools,
		client:    client,
		chatScope: ToolScope{WorkspaceID: ws, AgentID: agent, TaskID: asUUID(chatTaskID)},
		bareScope: ToolScope{WorkspaceID: ws, AgentID: agent, TaskID: asUUID(bareTaskID)},
	}
}

func TestFeishuToolsDB(t *testing.T) {
	f := newToolsFixture(t)
	ctx := context.Background()

	t.Run("context reports the current chat and requester", func(t *testing.T) {
		info, err := f.tools.Context(ctx, f.chatScope)
		if err != nil {
			t.Fatal(err)
		}
		if info.CurrentChat == nil || info.CurrentChat.ChatID != "oc_group" || info.CurrentChat.RequesterOpenID != "ou_asker" {
			t.Fatalf("context = %+v", info)
		}
		bare, err := f.tools.Context(ctx, f.bareScope)
		if err != nil || bare.CurrentChat != nil {
			t.Fatalf("bare context = %+v, %v", bare, err)
		}
	})

	t.Run("send defaults to the current chat with requester mention and files", func(t *testing.T) {
		f.client.sends = nil
		res, err := f.tools.Send(ctx, f.chatScope, SendInput{
			Text: "done", MentionRequester: true,
			Files: []ToolFile{{Name: "a.png", ContentType: "image/png", Data: []byte("x")}, {Name: "r.pdf", ContentType: "application/pdf", Data: []byte("y")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.MessageIDs) != 3 || len(f.client.sends) != 3 {
			t.Fatalf("sent %d messages, want 3", len(f.client.sends))
		}
		first := f.client.sends[0]
		if first.target.ChatID != "oc_group" || first.msgType != "text" || textBody(t, first.content) != `<at user_id="ou_asker"></at> done` {
			t.Fatalf("text send = %+v", first)
		}
		if f.client.sends[1].msgType != "image" || f.client.sends[2].msgType != "file" {
			t.Fatalf("file sends = %+v", f.client.sends[1:])
		}
	})

	t.Run("a task without a Feishu chat needs an explicit target", func(t *testing.T) {
		if _, err := f.tools.Send(ctx, f.bareScope, SendInput{Text: "hi"}); !errors.Is(err, ErrToolNoTarget) {
			t.Fatalf("err = %v, want ErrToolNoTarget", err)
		}
		f.client.sends = nil
		if _, err := f.tools.Send(ctx, f.bareScope, SendInput{Text: "hi", Target: ToolTarget{OpenID: "ou_x"}}); err != nil {
			t.Fatal(err)
		}
		if f.client.sends[0].target.OpenID != "ou_x" {
			t.Fatalf("target = %+v", f.client.sends[0].target)
		}
		if _, err := f.tools.Send(ctx, f.bareScope, SendInput{Text: "hi", MentionRequester: true, Target: ToolTarget{ChatID: "oc_x"}}); !errors.Is(err, ErrToolInvalidInput) {
			t.Fatalf("mention requester without requester: err = %v", err)
		}
	})

	t.Run("another agent cannot use the installation", func(t *testing.T) {
		other := f.chatScope
		other.AgentID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		if _, err := f.tools.Send(ctx, other, SendInput{Text: "hi"}); !errors.Is(err, ErrToolNoInstallation) {
			t.Fatalf("err = %v, want ErrToolNoInstallation", err)
		}
	})

	t.Run("scheduled message is sent by the scheduler when due", func(t *testing.T) {
		now := time.Now()
		row, err := f.tools.Schedule(ctx, f.chatScope, ScheduleInput{Text: "关煤气", MentionRequester: true, FireAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		list, err := f.tools.ListScheduled(ctx, f.chatScope)
		if err != nil || len(list) != 1 || list[0].ID != row.ID {
			t.Fatalf("list = %+v, %v", list, err)
		}
		// Not due yet: the scheduler leaves it alone.
		f.client.sends = nil
		f.tools.sendDueScheduled(ctx)
		if len(f.client.sends) != 0 {
			t.Fatalf("sent before due: %+v", f.client.sends)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE channel_scheduled_message SET fire_at = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		f.tools.sendDueScheduled(ctx)
		if len(f.client.sends) != 1 {
			t.Fatalf("sends = %d, want 1", len(f.client.sends))
		}
		got := f.client.sends[0]
		if got.target.ChatID != "oc_group" || textBody(t, got.content) != `<at user_id="ou_asker"></at> 关煤气` {
			t.Fatalf("scheduled send = %+v", got)
		}
		var status, sentID string
		if err := f.pool.QueryRow(ctx, `SELECT status, sent_message_id FROM channel_scheduled_message WHERE id = $1`, row.ID).Scan(&status, &sentID); err != nil {
			t.Fatal(err)
		}
		if status != "sent" || sentID != "om_sent" {
			t.Fatalf("status = %s, sent_message_id = %s", status, sentID)
		}
		// Sent once: a second pass does not resend.
		f.tools.sendDueScheduled(ctx)
		if len(f.client.sends) != 1 {
			t.Fatalf("resent: %d sends", len(f.client.sends))
		}
	})

	t.Run("failed scheduled send is recorded", func(t *testing.T) {
		row, err := f.tools.Schedule(ctx, f.chatScope, ScheduleInput{Text: "x", FireAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE channel_scheduled_message SET fire_at = now() WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		f.client.fail = &APIError{Op: "send", Code: 230002, Msg: "bot not in chat"}
		defer func() { f.client.fail = nil }()
		f.tools.sendDueScheduled(ctx)
		var status, lastErr string
		if err := f.pool.QueryRow(ctx, `SELECT status, last_error FROM channel_scheduled_message WHERE id = $1`, row.ID).Scan(&status, &lastErr); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !strings.Contains(lastErr, "bot not in chat") {
			t.Fatalf("status = %s, last_error = %s", status, lastErr)
		}
	})

	t.Run("cancel stops a pending message", func(t *testing.T) {
		row, err := f.tools.Schedule(ctx, f.chatScope, ScheduleInput{Text: "later", FireAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		var id pgtype.UUID
		_ = id.Scan(row.ID)
		if _, err := f.tools.CancelScheduled(ctx, f.chatScope, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.tools.CancelScheduled(ctx, f.chatScope, id); !errors.Is(err, ErrToolNotFound) {
			t.Fatalf("second cancel err = %v, want ErrToolNotFound", err)
		}
	})

	t.Run("schedule validates the time", func(t *testing.T) {
		if _, err := f.tools.Schedule(ctx, f.chatScope, ScheduleInput{Text: "x", FireAt: time.Now().Add(-time.Minute)}); !errors.Is(err, ErrToolInvalidInput) {
			t.Fatalf("past: err = %v", err)
		}
		if _, err := f.tools.Schedule(ctx, f.chatScope, ScheduleInput{Text: "x", FireAt: time.Now().Add(2 * maxScheduleAhead)}); !errors.Is(err, ErrToolInvalidInput) {
			t.Fatalf("too far: err = %v", err)
		}
	})

	t.Run("read doc follows wiki links", func(t *testing.T) {
		f.client.wiki["wikcn1"] = WikiNode{ObjType: "docx", ObjToken: "doxcn1", Title: "Plan"}
		f.client.docs["doxcn1"] = "plan body"
		doc, err := f.tools.ReadDoc(ctx, f.chatScope, "https://acme.feishu.cn/wiki/wikcn1?from=x")
		if err != nil {
			t.Fatal(err)
		}
		if doc.Content != "plan body" || doc.Title != "Plan" || doc.Type != "docx" {
			t.Fatalf("doc = %+v", doc)
		}
	})

	t.Run("read legacy doc and base", func(t *testing.T) {
		f.client.legacyDocs["doccnOld"] = "old body"
		doc, err := f.tools.ReadDoc(ctx, f.chatScope, "https://acme.feishu.cn/docs/doccnOld")
		if err != nil || doc.Content != "old body" || doc.Type != "doc" {
			t.Fatalf("legacy doc = %+v, %v", doc, err)
		}
		f.client.tables = []BitableTable{{TableID: "tbl1", Name: "任务"}}
		f.client.fields = []string{"名称", "负责人", "进度"}
		f.client.records = [][]map[string]any{
			{{"名称": "登录页", "负责人": []any{map[string]any{"name": "王五"}, map[string]any{"name": "李四"}}, "进度": 0.5}},
			{{"名称": "多行\n描述"}},
		}
		base, err := f.tools.ReadDoc(ctx, f.chatScope, "https://acme.feishu.cn/base/bascnX?table=tbl1")
		if err != nil {
			t.Fatal(err)
		}
		want := "## 任务\n名称\t负责人\t进度\n登录页\t王五, 李四\t0.5\n多行 描述\t\t\n\n"
		if base.Type != "bitable" || base.Content != want {
			t.Fatalf("base = %q, want %q", base.Content, want)
		}
	})

	t.Run("create doc shares with requester and the organization", func(t *testing.T) {
		f.client.shares = nil
		doc, err := f.tools.CreateDoc(ctx, f.chatScope, CreateDocInput{Title: "周报", Markdown: "# 本周\n- 完成 A", ShareWith: []string{"ou_b"}})
		if err != nil {
			t.Fatal(err)
		}
		if doc.URL != "https://acme.feishu.cn/docx/doxcnNew" || doc.ShareWarning != "" || f.client.createdMD != "# 本周\n- 完成 A" {
			t.Fatalf("doc = %+v, markdown = %q", doc, f.client.createdMD)
		}
		want := []string{"full_access:ou_asker", "edit:ou_b", "link:tenant_readable"}
		if strings.Join(f.client.shares, ",") != strings.Join(want, ",") {
			t.Fatalf("shares = %v, want %v", f.client.shares, want)
		}
		if _, err := f.tools.CreateDoc(ctx, f.chatScope, CreateDocInput{Title: "x", LinkShare: "public"}); !errors.Is(err, ErrToolInvalidInput) {
			t.Fatalf("bad link share: err = %v", err)
		}
	})

	t.Run("one-off wake-up dispatches the autopilot when due", func(t *testing.T) {
		d := &fakeDispatcher{run: db.AutopilotRun{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Status: "running"}}
		f.tools.SetAutopilotDispatcher(d)
		apID := f.fx.Insert(t, "autopilot", dbfx.Cols{
			"workspace_id": f.fx.WorkspaceID, "title": "检查部署", "assignee_id": f.agentID,
			"execution_mode": "run_only", "created_by_type": "member", "created_by_id": f.fx.UserID,
		})
		var apUUID, actor pgtype.UUID
		_ = apUUID.Scan(apID)
		_ = actor.Scan(f.fx.UserID)
		row, err := f.tools.ScheduleAgentRun(ctx, f.chatScope, ScheduleAgentRunInput{AutopilotID: apUUID, FireAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if row.Kind != "agent_run" || row.AutopilotID != apID || row.ReceiveID != "oc_group" {
			t.Fatalf("row = %+v", row)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE channel_scheduled_message SET fire_at = now() WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		f.client.sends = nil
		f.tools.sendDueScheduled(ctx)
		if len(d.calls) != 1 || d.calls[0] != actor || len(f.client.sends) != 0 {
			t.Fatalf("dispatches = %v, sends = %d", d.calls, len(f.client.sends))
		}
		var status string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM channel_scheduled_message WHERE id = $1`, row.ID).Scan(&status); err != nil || status != "sent" {
			t.Fatalf("status = %q, %v", status, err)
		}

		// Only this agent's run_only autopilots can be woken.
		other := f.fx.Insert(t, "autopilot", dbfx.Cols{
			"workspace_id": f.fx.WorkspaceID, "title": "x", "assignee_id": f.agentID,
			"execution_mode": "create_issue", "created_by_type": "member", "created_by_id": f.fx.UserID,
		})
		var otherUUID pgtype.UUID
		_ = otherUUID.Scan(other)
		if _, err := f.tools.ScheduleAgentRun(ctx, f.chatScope, ScheduleAgentRunInput{AutopilotID: otherUUID, FireAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrToolInvalidInput) {
			t.Fatalf("create_issue autopilot: err = %v", err)
		}
	})

	t.Run("create group includes the requester", func(t *testing.T) {
		chat, err := f.tools.CreateGroup(ctx, f.chatScope, CreateGroupInput{Name: "Team", Members: []string{"ou_b", "ou_asker"}, IncludeRequester: true})
		if err != nil || chat.ChatID != "oc_created" {
			t.Fatalf("CreateGroup = %+v, %v", chat, err)
		}
		got := f.client.chats[len(f.client.chats)-1].Members
		if len(got) != 2 || got[0] != "ou_asker" || got[1] != "ou_b" {
			t.Fatalf("members = %v, want [ou_asker ou_b]", got)
		}
	})
}

func TestParseDocURL(t *testing.T) {
	for _, tc := range []struct{ in, kind, token string }{
		{"https://acme.feishu.cn/docx/doxcnAbc", "docx", "doxcnAbc"},
		{"https://acme.feishu.cn/wiki/wikcnX?from=from_copylink", "wiki", "wikcnX"},
		{"https://acme.larksuite.com/sheets/shtcnY#sheet=1", "sheets", "shtcnY"},
		{"doxcnBare", "docx", "doxcnBare"},
	} {
		kind, token, err := ParseDocURL(tc.in)
		if err != nil || kind != tc.kind || token != tc.token {
			t.Errorf("ParseDocURL(%q) = %q, %q, %v", tc.in, kind, token, err)
		}
	}
	if _, _, err := ParseDocURL("https://acme.feishu.cn/drive/folder/x"); !errors.Is(err, ErrToolInvalidInput) {
		t.Errorf("unknown url: err = %v", err)
	}
}

func TestTextMessageUsesCardForMarkdown(t *testing.T) {
	msgType, content, err := textMessage("**bold**", []string{"ou_a"})
	var card struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(content), &card)
	}
	if err != nil || msgType != "interactive" || len(card.Body.Elements) != 1 || card.Body.Elements[0].Content != "<at id=ou_a></at> **bold**" {
		t.Fatalf("markdown = %q %q %v", msgType, content, err)
	}
	msgType, content, err = textMessage("plain", []string{"ou_a", "ou_b"})
	if err != nil || msgType != "text" || textBody(t, content) != `<at user_id="ou_a"></at> <at user_id="ou_b"></at> plain` {
		t.Fatalf("text = %q %q %v", msgType, content, err)
	}
}

// textBody decodes a msg_type=text content envelope.
func textBody(t *testing.T, content string) string {
	t.Helper()
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		t.Fatalf("decode text content %q: %v", content, err)
	}
	return body.Text
}
