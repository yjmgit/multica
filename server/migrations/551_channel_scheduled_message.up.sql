-- Messages an agent scheduled to post into a channel later (for example a
-- "remind me in 2 minutes" in Feishu). The server-side scheduler claims due
-- rows and sends them, so a reminder does not depend on the agent still
-- running. No foreign keys: workspace deletion and the scheduler clean up and
-- validate installation/agent references in application code.
CREATE TABLE IF NOT EXISTS channel_scheduled_message (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id     UUID NOT NULL,
    installation_id  UUID NOT NULL,
    agent_id         UUID NOT NULL,
    channel_type     TEXT NOT NULL,
    task_id          UUID,
    -- Where to send: receive_id_type is 'chat_id' or 'open_id'. A non-empty
    -- reply_message_id routes the message as a reply (keeps a topic intact).
    receive_id_type  TEXT NOT NULL CHECK (receive_id_type IN ('chat_id', 'open_id')),
    receive_id       TEXT NOT NULL,
    reply_message_id TEXT NOT NULL DEFAULT '',
    reply_in_thread  BOOLEAN NOT NULL DEFAULT false,
    text             TEXT NOT NULL,
    mention_open_ids TEXT[] NOT NULL DEFAULT '{}',
    fire_at          TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    last_error       TEXT NOT NULL DEFAULT '',
    sent_message_id  TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
