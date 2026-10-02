-- Settings each guild configures for itself through /config. A guild without
-- a row uses the defaults: no report channels and no exemptions.
CREATE TABLE guild_settings (
    guild_id          TEXT PRIMARY KEY,
    flag_channel_id   TEXT NOT NULL DEFAULT '',
    action_channel_id TEXT NOT NULL DEFAULT ''
) STRICT;

-- Channels and roles that moderation skips in a guild, on top of members who
-- can moderate the channel themselves.
CREATE TABLE guild_exemptions (
    guild_id  TEXT NOT NULL,
    kind      TEXT NOT NULL CHECK (kind IN ('channel', 'role')),
    target_id TEXT NOT NULL,
    PRIMARY KEY (guild_id, kind, target_id)
) STRICT, WITHOUT ROWID;
