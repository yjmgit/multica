-- An autopilot whose run results are posted to a Feishu chat: set up
-- automatically when an agent creates a create_issue autopilot from a Feishu
-- conversation, or explicitly with `multica lark follow-autopilot`. When a run
-- finishes, its comment (or, without an issue, its output) is posted to
-- chat_id through the bot of installation_id. No foreign keys: workspace
-- deletion cleans rows up and the relay validates the installation and
-- autopilot when it fires.
CREATE TABLE IF NOT EXISTS channel_autopilot_relay (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL,
    installation_id   UUID NOT NULL,
    autopilot_id      UUID NOT NULL,
    chat_id           TEXT NOT NULL,
    requester_open_id TEXT NOT NULL DEFAULT '',
    -- The last run relayed, so one run is never posted twice.
    last_task_id      UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
