-- name: IsChannelSessionIdle :one
-- Called with the session and current binding locked, in append lock order.
-- Queued/retrying work and unsealed input are active work, not idle time.
SELECT COALESCE(now() >= GREATEST(
    session.created_at,
    (SELECT max(message.created_at) FROM chat_message message
     WHERE message.chat_session_id = session.id AND message.role = 'user'),
    (SELECT max(task.completed_at) FROM agent_task_queue task
     WHERE task.chat_session_id = session.id)
) + make_interval(secs => @idle_seconds::double precision)
AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue task
    WHERE task.chat_session_id = session.id
      AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
)
AND NOT EXISTS (
    SELECT 1 FROM chat_message message
    WHERE message.chat_session_id = session.id AND message.role = 'user'
      AND message.channel_ingested AND message.message_kind <> 'channel_command'
      AND message.task_id IS NULL
), false)::boolean AS idle
FROM chat_session session WHERE session.id = @chat_session_id;

-- name: HasPendingChannelPredecessor :one
-- This scope is deliberately identical to the predecessor gate in ClaimAgentTask.
SELECT EXISTS (
    SELECT 1 FROM channel_chat_session_binding current_route
    JOIN channel_chat_session_binding older
      ON older.installation_id = current_route.installation_id
     AND older.channel_chat_id = current_route.channel_chat_id
     AND older.route_revision < current_route.route_revision
    WHERE current_route.chat_session_id = @chat_session_id
      AND current_route.channel_type = 'feishu'
      AND current_route.config->>'member_open_id' IS NOT NULL
      AND (
          EXISTS (SELECT 1 FROM agent_task_queue task
              WHERE task.chat_session_id = older.chat_session_id
                AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
          OR EXISTS (SELECT 1 FROM chat_message message
              WHERE message.chat_session_id = older.chat_session_id
                AND message.role = 'user' AND message.channel_ingested
                AND message.message_kind <> 'channel_command' AND message.task_id IS NULL)
      )
) AS pending;
