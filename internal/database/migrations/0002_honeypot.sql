-- A guild's honeypot: a channel where any post gets its author softbanned.
-- An empty honeypot_channel_id turns it off. honeypot_purge_seconds is how
-- far back the ban deletes the member's messages, at most the 7 days Discord
-- allows; it defaults to one day.
ALTER TABLE guild_settings ADD COLUMN honeypot_channel_id TEXT NOT NULL DEFAULT '';
ALTER TABLE guild_settings ADD COLUMN honeypot_purge_seconds INTEGER NOT NULL DEFAULT 86400
    CHECK (honeypot_purge_seconds BETWEEN 0 AND 604800);
