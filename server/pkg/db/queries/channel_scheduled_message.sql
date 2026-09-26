-- name: CreateChannelScheduledMessage :one
INSERT INTO channel_scheduled_message (
    workspace_id, installation_id, agent_id, channel_type, task_id,
    receive_id_type, receive_id, reply_message_id, reply_in_thread,
    text, mention_open_ids, fire_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING *;

-- name: ListPendingChannelScheduledMessagesByAgent :many
SELECT * FROM channel_scheduled_message
WHERE workspace_id = $1 AND agent_id = $2 AND status = 'pending'
ORDER BY fire_at ASC
LIMIT 100;

-- name: CancelChannelScheduledMessage :one
UPDATE channel_scheduled_message
SET status = 'cancelled', updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND agent_id = $3 AND status = 'pending'
RETURNING *;

-- name: ClaimDueChannelScheduledMessages :many
-- Moves due pending rows to 'sending' so exactly one replica sends each.
-- SKIP LOCKED lets concurrent schedulers split the batch instead of waiting.
UPDATE channel_scheduled_message
SET status = 'sending', updated_at = now()
WHERE id IN (
    SELECT id FROM channel_scheduled_message
    WHERE status = 'pending' AND fire_at <= now()
    ORDER BY fire_at ASC
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishChannelScheduledMessage :exec
UPDATE channel_scheduled_message
SET status = sqlc.arg(status), last_error = sqlc.arg(last_error),
    sent_message_id = sqlc.arg(sent_message_id), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'sending';

-- name: FailStaleSendingChannelScheduledMessages :execrows
-- A replica that died mid-send leaves its row in 'sending'. Fail it rather than
-- resend: the message may already have been delivered, and a duplicate
-- reminder is worse than a missing one the agent can see in the list.
UPDATE channel_scheduled_message
SET status = 'failed', last_error = 'interrupted while sending', updated_at = now()
WHERE status = 'sending' AND updated_at < now() - sqlc.arg(stale_after)::interval;
