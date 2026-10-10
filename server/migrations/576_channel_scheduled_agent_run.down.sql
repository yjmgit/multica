ALTER TABLE channel_scheduled_message
    DROP COLUMN IF EXISTS actor_user_id,
    DROP COLUMN IF EXISTS autopilot_id,
    DROP COLUMN IF EXISTS kind;
