-- One relay per (autopilot, bot); run completion looks relays up by autopilot.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_autopilot_relay_autopilot_installation_uidx
    ON channel_autopilot_relay (autopilot_id, installation_id);
