-- A scheduled row can also start a one-off agent run: at fire_at the scheduler
-- dispatches autopilot_id on behalf of actor_user_id instead of posting text.
-- The table is small and new, so the CHECK validation scan is trivial; bound
-- lock acquisition anyway so a busy scheduler cannot stall the migration.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE channel_scheduled_message
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'message'
        CHECK (kind IN ('message', 'agent_run')),
    ADD COLUMN IF NOT EXISTS autopilot_id UUID,
    ADD COLUMN IF NOT EXISTS actor_user_id UUID;
