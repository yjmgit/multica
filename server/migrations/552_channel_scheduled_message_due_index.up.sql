-- The scheduler polls for due pending messages; keep that scan off the
-- sent/cancelled history.
CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_scheduled_message_due_idx
    ON channel_scheduled_message (fire_at) WHERE status = 'pending';
