-- Timestamps are UTC ISO 8601 text with milliseconds, e.g.
-- 2026-10-02T13:55:49.079Z, so they read well and sort correctly.

-- Every guild Shugo has been added to, kept in step with the gateway. A row
-- outlives the bot's removal, so a server that invites Shugo back keeps its
-- settings; removed_at records when it left.
CREATE TABLE guilds (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    removed_at TEXT
) STRICT;

-- Settings a guild configures for itself through /settings. A guild without
-- a row uses the defaults: no report channels.
CREATE TABLE guild_settings (
    guild_id          TEXT PRIMARY KEY REFERENCES guilds (id) ON DELETE CASCADE,
    flag_channel_id   TEXT NOT NULL DEFAULT '',
    action_channel_id TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- Channels and roles that moderation skips in a guild, on top of members who
-- can moderate the channel themselves.
CREATE TABLE guild_exemptions (
    guild_id   TEXT NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('channel', 'role')),
    target_id  TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (guild_id, kind, target_id)
) STRICT, WITHOUT ROWID;
