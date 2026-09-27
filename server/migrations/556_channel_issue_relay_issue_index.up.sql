-- One relay per (issue, bot); run completion looks relays up by issue.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_issue_relay_issue_installation_uidx
    ON channel_issue_relay (issue_id, installation_id);
