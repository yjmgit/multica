package lark

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeAttachmentQueries struct {
	atts []db.Attachment
	got  db.ListAttachmentsByChatMessageParams
}

func (f *fakeAttachmentQueries) ListAttachmentsByChatMessage(_ context.Context, arg db.ListAttachmentsByChatMessageParams) ([]db.Attachment, error) {
	f.got = arg
	return f.atts, nil
}

type fakeAttachmentStorage struct{ objects map[string]string }

func (f fakeAttachmentStorage) KeyFromURL(rawURL string) string {
	return strings.TrimPrefix(rawURL, "https://cdn.test/")
}

func (f fakeAttachmentStorage) GetReader(_ context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.objects[key])), nil
}

// A file the agent attached with `multica attachment upload` reaches the
// Feishu chat as its own message after the text reply.
func TestPatcherDeliversReplyAttachments(t *testing.T) {
	p, q, api := newTestPatcher(t)
	messageID := uuidFromString(t, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	aq := &fakeAttachmentQueries{atts: []db.Attachment{
		{ID: uuidFromString(t, "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1"), Filename: "chart.png", ContentType: "image/png", Url: "https://cdn.test/k1", SizeBytes: 3},
		{ID: uuidFromString(t, "a2a2a2a2-a2a2-a2a2-a2a2-a2a2a2a2a2a2"), Filename: "report.pdf", ContentType: "application/pdf", Url: "https://cdn.test/k2", SizeBytes: 3},
	}}
	tc := &fakeToolClient{}
	p.SetReplyAttachments(ReplyAttachmentDeps{
		Queries: aq,
		Storage: fakeAttachmentStorage{objects: map[string]string{"k1": "png", "k2": "pdf"}},
		Client:  tc,
	})
	taskID := uuidFromString(t, "ee777777-ee77-ee77-ee77-eeeeeeeeeeee")

	p.handleEvent(events.Event{
		Type:          protocol.EventChatDone,
		TaskID:        uuidString(taskID),
		ChatSessionID: uuidString(q.binding.ChatSessionID),
		Payload: protocol.ChatDonePayload{
			TaskID:        uuidString(taskID),
			ChatSessionID: uuidString(q.binding.ChatSessionID),
			MessageID:     uuidString(messageID),
			Content:       "here is the chart",
		},
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		tc.mu.Lock()
		n := len(tc.sends)
		tc.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if len(tc.sends) != 2 {
		t.Fatalf("attachment sends = %d, want 2", len(tc.sends))
	}
	if tc.sends[0].msgType != "image" || tc.sends[1].msgType != "file" {
		t.Fatalf("msg types = %s, %s", tc.sends[0].msgType, tc.sends[1].msgType)
	}
	if tc.sends[0].target.ChatID != q.binding.ChannelChatID {
		t.Fatalf("target = %+v", tc.sends[0].target)
	}
	if aq.got.ChatMessageID != messageID {
		t.Fatalf("listed attachments of %v, want the reply message", aq.got.ChatMessageID)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.textSent) != 1 {
		t.Fatalf("text reply sends = %d, want 1", len(api.textSent))
	}
}
