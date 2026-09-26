package lark

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Files an agent attaches to a chat reply (`multica attachment upload`) are
// bound to the assistant chat_message. Web chat renders them as cards; this
// file delivers them to Feishu too, one image or file message each, right
// after the reply text.

// ReplyAttachmentQueries is the attachment lookup the Patcher needs.
type ReplyAttachmentQueries interface {
	ListAttachmentsByChatMessage(ctx context.Context, arg db.ListAttachmentsByChatMessageParams) ([]db.Attachment, error)
}

// ReplyAttachmentStorage reads attachment bytes back from object storage.
type ReplyAttachmentStorage interface {
	KeyFromURL(rawURL string) string
	GetReader(ctx context.Context, key string) (io.ReadCloser, error)
}

// ReplyAttachmentDeps wires reply-attachment delivery into the Patcher.
type ReplyAttachmentDeps struct {
	Queries ReplyAttachmentQueries
	Storage ReplyAttachmentStorage
	Client  ToolAPIClient
}

// replyAttachmentTimeout bounds downloading, uploading and sending all of one
// reply's attachments. It runs detached from the bus delivery.
const replyAttachmentTimeout = 2 * time.Minute

// SetReplyAttachments enables delivering reply attachments. Leaving it unset
// keeps replies text-only.
func (p *Patcher) SetReplyAttachments(d ReplyAttachmentDeps) {
	if d.Queries == nil || d.Storage == nil || d.Client == nil {
		return
	}
	p.attachments = &d
}

// chatDoneMessageID extracts the assistant chat_message id from the payload.
func chatDoneMessageID(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.MessageID
	case map[string]any:
		if s, ok := p["message_id"].(string); ok {
			return s
		}
	}
	return ""
}

// deliverReplyAttachments sends the reply's attachments in the background so
// a large upload never blocks the event bus.
func (p *Patcher) deliverReplyAttachments(creds InstallationCredentials, binding ChatSessionBinding, workspaceID pgtype.UUID, payload any) {
	if p.attachments == nil || topicSendWithoutTrigger(binding) {
		return
	}
	var messageID pgtype.UUID
	if err := messageID.Scan(chatDoneMessageID(payload)); err != nil || !messageID.Valid {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), replyAttachmentTimeout)
		defer cancel()
		if err := p.sendReplyAttachments(ctx, creds, binding, workspaceID, messageID); err != nil {
			p.cfg.Logger.Warn("lark patcher: reply attachment delivery failed",
				"chat_message_id", uuidString(messageID), "error", err)
		}
	}()
}

func (p *Patcher) sendReplyAttachments(ctx context.Context, creds InstallationCredentials, binding ChatSessionBinding, workspaceID, messageID pgtype.UUID) error {
	atts, err := p.attachments.Queries.ListAttachmentsByChatMessage(ctx, db.ListAttachmentsByChatMessageParams{
		ChatMessageID: messageID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("list attachments: %w", err)
	}
	chatID := string(outboundChatID(binding))
	for _, att := range atts {
		data, err := p.readAttachment(ctx, att)
		if err != nil {
			p.cfg.Logger.Warn("lark patcher: read reply attachment", "attachment_id", uuidString(att.ID), "error", err)
			continue
		}
		f := ToolFile{Name: att.Filename, ContentType: att.ContentType, Data: data}
		err = sendWithReplyFallback(p.cfg.Logger, "send reply attachment", threadReplyTarget(binding), func(t ReplyTarget) error {
			_, err := sendFileMessage(ctx, p.attachments.Client, creds, MessageTarget{ChatID: chatID, Reply: t}, f)
			return err
		})
		if err != nil {
			p.cfg.Logger.Warn("lark patcher: send reply attachment", "attachment_id", uuidString(att.ID), "error", err)
		}
	}
	return nil
}

func (p *Patcher) readAttachment(ctx context.Context, att db.Attachment) ([]byte, error) {
	if att.SizeBytes > maxMessageFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, over Lark's 30 MiB file limit", att.Filename, att.SizeBytes)
	}
	key := p.attachments.Storage.KeyFromURL(att.Url)
	if key == "" {
		return nil, fmt.Errorf("no storage key for %s", att.Url)
	}
	rc, err := p.attachments.Storage.GetReader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMessageFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMessageFileBytes {
		return nil, fmt.Errorf("%s is over Lark's 30 MiB file limit", att.Filename)
	}
	return data, nil
}
