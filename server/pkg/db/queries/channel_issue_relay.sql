-- name: UpsertChannelIssueRelay :one
INSERT INTO channel_issue_relay (
    workspace_id, installation_id, issue_id, chat_id,
    reply_message_id, reply_in_thread, requester_open_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (issue_id, installation_id) DO UPDATE SET
    chat_id = EXCLUDED.chat_id,
    reply_message_id = EXCLUDED.reply_message_id,
    reply_in_thread = EXCLUDED.reply_in_thread,
    requester_open_id = EXCLUDED.requester_open_id,
    created_at = now(),
    updated_at = now()
RETURNING *;

-- name: ListActiveChannelIssueRelaysByIssue :many
-- Relays expire 30 days after they were set up so a long-lived issue stops
-- posting into a chat that has moved on.
SELECT * FROM channel_issue_relay
WHERE issue_id = $1 AND created_at > now() - interval '30 days';

-- name: ClaimChannelIssueRelayTask :execrows
-- Records that task_id is being relayed; zero rows means it already was.
UPDATE channel_issue_relay
SET last_task_id = sqlc.arg(task_id), updated_at = now()
WHERE id = sqlc.arg(id) AND last_task_id IS DISTINCT FROM sqlc.arg(task_id);

-- name: GetLatestIssueCommentByTask :one
SELECT * FROM comment
WHERE issue_id = $1 AND source_task_id = $2 AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;
