package lark

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGroupMemberPolicy(t *testing.T) {
	for _, tc := range []struct {
		value    string
		duration time.Duration
	}{{"", 2 * time.Hour}, {"2h", 2 * time.Hour}, {"30m", 30 * time.Minute}, {"0", 0}} {
		p, err := ParseGroupMemberPolicy(" cli_test,cli_second ", tc.value)
		if err != nil || p.IdleTTL != tc.duration || !p.Enabled("cli_test") || p.Enabled("other") {
			t.Fatalf("parse %q: %+v %v", tc.value, p, err)
		}
	}
	for _, v := range []string{"-1h", "nope", "2", "99999999999999999999999h"} {
		if _, err := ParseGroupMemberPolicy("cli_test", v); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
}

func groupMessage(chat, sender string) channel.InboundMessage {
	return channelMessageFromLark(InboundMessage{AppID: "cli_test", ChatID: ChatID(chat), ChatType: ChatTypeGroup,
		MessageID: "om_test", SenderType: "user", SenderOpenID: OpenID(sender), Body: "hello", AddressedToBot: true})
}

func TestGroupMemberRoutingAndTTL(t *testing.T) {
	p, _ := ParseGroupMemberPolicy("cli_test", "2h")
	f := &fakeChatSession{}
	b := &feishuSessionBinder{session: f, groupMembers: p}
	seen := map[string]bool{}
	for _, pair := range [][2]string{{"oc_a", "ou_a"}, {"oc_a", "ou_b"}, {"oc_b", "ou_a"}} {
		msg := groupMessage(pair[0], pair[1])
		key, config := b.sessionRouting(msg)
		if seen[key] {
			t.Fatal("members or groups share a key")
		}
		seen[key] = true
		var cfg larkBindingConfig
		if json.Unmarshal(config, &cfg) != nil || cfg.ChatID != pair[0] || cfg.MemberOpenID != pair[1] {
			t.Fatal("lost outbound identity")
		}
		msg.Source.ThreadID = "omt_thread"
		threadKey, _ := b.sessionRouting(msg)
		if threadKey != key {
			t.Fatal("thread split member context")
		}
		_, err := b.EnsureSession(context.Background(), engine.EnsureSessionParams{Message: msg})
		if err != nil || f.ensureIn.IdleTTL != 2*time.Hour {
			t.Fatal("TTL not passed to engine", err)
		}
		if isTopicIsolated(ChatSessionBinding{ChannelChatID: key, Config: config}) {
			t.Fatal("member route misclassified as topic")
		}
	}
	msg := groupMessage("oc_a", "ou_a")
	msg.Source.ChatType = channel.ChatTypeP2P
	key, _ := b.sessionRouting(msg)
	if key != "oc_a" {
		t.Fatal("private chat changed")
	}
}

func TestGroupMemberDecoderBoundary(t *testing.T) {
	p, _ := ParseGroupMemberPolicy("cli_test", "2h")
	d := &LarkJSONFrameDecoder{GroupMembers: p}
	inst := Installation{AppID: "cli_test", BotOpenID: "ou_bot"}
	for _, tc := range []struct {
		app, senderType, sender, chat, message string
		valid                                  bool
	}{
		{"cli_test", "user", "ou_a", "oc_a", "om_a", true},
		{"other", "user", "ou_a", "oc_a", "om_a", false},
		{"cli_test", "app", "ou_a", "oc_a", "om_a", false},
		{"cli_test", "user", "", "oc_a", "om_a", false},
		{"cli_test", "user", "ou_a", "", "om_a", false},
		{"cli_test", "user", "ou_a", "oc_a", "", false},
	} {
		ev := map[string]any{"schema": "2.0", "header": map[string]any{"event_type": "im.message.receive_v1", "app_id": tc.app}, "event": map[string]any{
			"sender":  map[string]any{"sender_type": tc.senderType, "sender_id": map[string]any{"open_id": tc.sender}},
			"message": map[string]any{"chat_type": "group", "chat_id": tc.chat, "message_id": tc.message, "message_type": "text", "content": `{"text":"hello"}`}}}
		payload, _ := json.Marshal(ev)
		_, ok, err := d.Decode(payload, inst)
		if err != nil || ok != tc.valid {
			t.Fatalf("boundary %+v: ok=%v err=%v", tc, ok, err)
		}
	}
}

func TestGroupMemberEnrichmentOnlyExplicitContext(t *testing.T) {
	p, _ := ParseGroupMemberPolicy("cli_test", "2h")
	fake := newEnricherFake()
	fake.byChat["oc_a"] = []LarkMessage{textMsg("om_other", "ou_b", "OTHER_MEMBER_HISTORY", "1")}
	fake.byID["om_quote"] = []LarkMessage{textMsg("om_quote", "ou_b", "EXPLICIT_QUOTE", "1")}
	msg := InboundMessage{AppID: "cli_test", ChatID: "oc_a", ChatType: ChatTypeGroup, AddressedToBot: true, Body: "question", SenderOpenID: "ou_a"}
	out := enrich(t, fake, msg, InboundEnricherConfig{GroupMembers: p, RecentContextSize: 10})
	if len(fake.listCalls) != 0 || strings.Contains(out.Body, "OTHER_MEMBER_HISTORY") {
		t.Fatal("group history leaked")
	}
	msg.ParentID = "om_quote"
	out = enrich(t, fake, msg, InboundEnricherConfig{GroupMembers: p, RecentContextSize: 10})
	if !strings.Contains(out.Body, "EXPLICIT_QUOTE") || len(fake.listCalls) != 0 {
		t.Fatal("explicit context lost or group history fetched")
	}
}

func TestGroupMemberIdentityWithoutBinding(t *testing.T) {
	pool := channelScopeTestDB(t)
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Group operator", "group-operator-"+time.Now().Format("150405.000000000")+"@test.invalid")
	workspace := fx.Workspace(t, "Group test", "group-test-"+time.Now().Format("150405.000000000"))
	fx.Member(t, workspace, user, "owner")
	policy, _ := ParseGroupMemberPolicy("cli_test", "2h")
	resolver := &feishuIdentityResolver{store: NewChannelStore(db.New(pool)), groupMembers: policy}
	inst := engine.ResolvedInstallation{ID: binderUUID(40), WorkspaceID: util.MustParseUUID(workspace), InstallerUserID: util.MustParseUUID(user), Platform: Installation{AppID: "cli_test"}}
	for _, sender := range []string{"ou_a", "ou_b"} {
		identity, err := resolver.ResolveSender(context.Background(), inst, groupMessage("oc_a", sender))
		if err != nil || identity.UserID != inst.InstallerUserID {
			t.Fatal("group member needs binding", err)
		}
	}
	// Private chats skip the group policy and take the open-access path: an
	// unbound sender talks as the installer.
	msg := groupMessage("oc_a", "ou_a")
	msg.Source.ChatType = channel.ChatTypeP2P
	if identity, err := resolver.ResolveSender(context.Background(), inst, msg); err != nil || identity.UserID != inst.InstallerUserID {
		t.Fatal("unbound private sender not resolved to installer", err)
	}
	msg = groupMessage("oc_a", "ou_a")
	msg.Source.SenderID = "ou_forged"
	if _, err := resolver.ResolveSender(context.Background(), inst, msg); !errors.Is(err, engine.ErrSenderNotMember) {
		t.Fatal("inconsistent identity accepted", err)
	}
}

func TestGroupMemberFailureQuotesActualSender(t *testing.T) {
	p, q, api := groupPatcherWithSender(t, "ou_a")
	q.binding.ChannelChatID = "member:oc_group:ou_a"
	q.binding.Config = []byte(`{"chat_id":"oc_group","member_open_id":"ou_a"}`)
	taskID := uuidFromString(t, "eecceeee-eecc-eecc-eecc-eeeeeeeeeeee")
	q.deliveriesByTask = map[string]db.ChannelTaskDelivery{uuidString(taskID): deliveryWithTrigger(q.binding, "om_a", "ou_a")}
	p.handleEvent(events.Event{Type: protocol.EventTaskFailed, TaskID: uuidString(taskID), ChatSessionID: uuidString(q.binding.ChatSessionID), Payload: map[string]any{"error": "internal secret path"}})
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.textSent) != 1 {
		t.Fatalf("expected one failure reply, got %d", len(api.textSent))
	}
	got := api.textSent[0]
	if got.ChatID != "oc_group" || got.ReplyTarget.MessageID != "om_a" || !strings.Contains(got.Text, `<at user_id="ou_a"></at>`) || strings.Contains(got.Text, "internal secret") {
		t.Fatalf("wrong failure reply: %+v", got)
	}
}

func TestGroupMemberNoticesMentionAndQuote(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeChatStarted, OutcomeIngested, OutcomeAgentOffline} {
		stub := &stubAPIClientWithRecorder{configured: true}
		rep := NewLarkOutcomeReplier(OutcomeReplierConfig{APIClient: stub, BindingSvc: &BindingTokenService{}, Credentials: stubCredentialsResolver{secret: "s"}, Queries: stubReplierQueries{pending: true}})
		rep.Reply(context.Background(), Installation{}, InboundMessage{ChatID: "oc_group", ChatType: ChatTypeGroup, MessageID: "om_a", ThreadID: "omt_a", SenderOpenID: "ou_a"}, DispatchResult{Outcome: outcome, ChatSessionID: binderUUID(42)})
		if len(stub.interactiveOut) != 1 {
			t.Fatalf("%s: missing notice", outcome)
		}
		got := stub.interactiveOut[0]
		var card map[string]any
		if err := json.Unmarshal([]byte(got.CardJSON), &card); err != nil {
			t.Fatal(err)
		}
		body := card["elements"].([]any)[0].(map[string]any)["text"].(map[string]any)
		if body["tag"] != "lark_md" || !strings.Contains(body["content"].(string), `<at id=ou_a></at>`) || got.ReplyTarget.MessageID != "om_a" || !got.ReplyTarget.InThread {
			t.Fatalf("%s: wrong notice %+v", outcome, got)
		}
	}
}
