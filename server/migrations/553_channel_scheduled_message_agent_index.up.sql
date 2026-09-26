-- Agents list and cancel their own scheduled messages.
CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_scheduled_message_agent_idx
    ON channel_scheduled_message (workspace_id, agent_id, fire_at);
