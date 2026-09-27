-- An issue an agent delegated from a Feishu conversation (`multica lark
-- delegate`). When a run on the issue finishes, the comment that run posted is
-- relayed back to chat_id through the delegating bot. No foreign keys:
-- workspace deletion cleans rows up and the relay validates the installation
-- and issue when it fires.
CREATE TABLE IF NOT EXISTS channel_issue_relay (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL,
    installation_id   UUID NOT NULL,
    issue_id          UUID NOT NULL,
    chat_id           TEXT NOT NULL,
    reply_message_id  TEXT NOT NULL DEFAULT '',
    reply_in_thread   BOOLEAN NOT NULL DEFAULT false,
    requester_open_id TEXT NOT NULL DEFAULT '',
    -- The last run relayed, so one run is never posted twice.
    last_task_id      UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
