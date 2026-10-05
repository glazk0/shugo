// Package guild stores the settings each Discord guild configures for itself:
// where reports go, which channels and roles moderation skips, and the
// honeypot channel.
package guild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/glazk0/shugo/internal/moderation"
)

// LogKind names one of a guild's two report channels.
type LogKind string

const (
	// LogFlags receives reports of flagged messages, which need a human.
	LogFlags LogKind = "flags"
	// LogActions receives reports of deletes and timeouts.
	LogActions LogKind = "actions"
)

// ExemptionKind is what an exemption applies to. The values match the kind
// column of the guild_exemptions table.
type ExemptionKind string

const (
	// ExemptChannel skips messages sent in a channel or in its threads.
	ExemptChannel ExemptionKind = "channel"
	// ExemptRole skips messages from members holding a role.
	ExemptRole ExemptionKind = "role"
)

// Honeypot purge bounds. Discord deletes at most 7 days of messages with a
// ban.
const (
	// DefaultHoneypotPurge is the purge a honeypot gets when the guild picks
	// none.
	DefaultHoneypotPurge = 24 * time.Hour
	// MaxHoneypotPurge is the longest purge Discord accepts.
	MaxHoneypotPurge = 7 * 24 * time.Hour
)

// Settings is one guild's configuration. The zero value is the default:
// reports are off, nothing is exempt and there is no honeypot.
type Settings struct {
	// FlagChannelID receives reports of flagged messages; empty falls back to
	// ActionChannelID.
	FlagChannelID string
	// ActionChannelID receives reports of deletes and timeouts; empty falls
	// back to FlagChannelID.
	ActionChannelID string
	// ExemptChannelIDs and ExemptRoleIDs are sorted.
	ExemptChannelIDs []string
	ExemptRoleIDs    []string
	// HoneypotChannelID is the channel where any post gets its author
	// softbanned; empty when the guild has no honeypot.
	HoneypotChannelID string
	// HoneypotPurge is how far back the softban deletes the author's
	// messages.
	HoneypotPurge time.Duration
}

// ReportChannel returns the channel that should receive the report for an
// action, or "" when the guild has no report channel at all.
//
// Parameters:
//   - a (moderation.Action): enforced action; ActionFlag goes to the flag
//     channel, anything stronger to the action channel.
func (s Settings) ReportChannel(a moderation.Action) string {
	primary, fallback := s.ActionChannelID, s.FlagChannelID
	if a == moderation.ActionFlag {
		primary, fallback = fallback, primary
	}
	if primary != "" {
		return primary
	}
	return fallback
}

// ExemptsChannel reports whether moderation skips channelID.
//
// Parameters:
//   - channelID (string): channel to check; "" is never exempt.
func (s Settings) ExemptsChannel(channelID string) bool {
	return channelID != "" && slices.Contains(s.ExemptChannelIDs, channelID)
}

// IsHoneypot reports whether channelID is the guild's honeypot.
//
// Parameters:
//   - channelID (string): channel to check; "" is never the honeypot.
func (s Settings) IsHoneypot(channelID string) bool {
	return channelID != "" && channelID == s.HoneypotChannelID
}

// ExemptsAnyRole reports whether any of roleIDs is exempt.
//
// Parameters:
//   - roleIDs ([]string): roles held by a member.
func (s Settings) ExemptsAnyRole(roleIDs []string) bool {
	return slices.ContainsFunc(roleIDs, func(id string) bool {
		return slices.Contains(s.ExemptRoleIDs, id)
	})
}

// ensureGuild creates the guilds row a settings write refers to, in case the
// gateway event that normally records the guild was never stored.
const ensureGuild = `INSERT INTO guilds (id) VALUES (?) ON CONFLICT (id) DO NOTHING`

// setLogChannel holds one upsert per LogKind, so column names never come
// from a variable.
var setLogChannel = map[LogKind]string{
	LogFlags: `INSERT INTO guild_settings (guild_id, flag_channel_id) VALUES (?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET
			flag_channel_id = excluded.flag_channel_id,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
	LogActions: `INSERT INTO guild_settings (guild_id, action_channel_id) VALUES (?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET
			action_channel_id = excluded.action_channel_id,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
}

// Store reads and writes guilds and their settings in SQLite. It is safe for
// concurrent use.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store backed by db, which must already be migrated.
//
// Parameters:
//   - db (*sql.DB): database opened with database.Open.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Get returns the settings of guildID, or the zero Settings when the guild
// never configured anything.
//
// Parameters:
//   - ctx (context.Context): bounds the queries.
//   - guildID (string): guild to look up.
func (s *Store) Get(ctx context.Context, guildID string) (Settings, error) {
	out := Settings{HoneypotPurge: DefaultHoneypotPurge}
	var purgeSeconds int64
	err := s.db.QueryRowContext(ctx,
		`SELECT flag_channel_id, action_channel_id, honeypot_channel_id, honeypot_purge_seconds
		FROM guild_settings WHERE guild_id = ?`, guildID,
	).Scan(&out.FlagChannelID, &out.ActionChannelID, &out.HoneypotChannelID, &purgeSeconds)
	switch {
	case err == nil:
		out.HoneypotPurge = time.Duration(purgeSeconds) * time.Second
	case !errors.Is(err, sql.ErrNoRows):
		return Settings{}, fmt.Errorf("guild: get settings for %s: %w", guildID, err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, target_id FROM guild_exemptions WHERE guild_id = ? ORDER BY kind, target_id`, guildID)
	if err != nil {
		return Settings{}, fmt.Errorf("guild: get exemptions for %s: %w", guildID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind ExemptionKind
		var id string
		if err := rows.Scan(&kind, &id); err != nil {
			return Settings{}, fmt.Errorf("guild: get exemptions for %s: %w", guildID, err)
		}
		switch kind {
		case ExemptChannel:
			out.ExemptChannelIDs = append(out.ExemptChannelIDs, id)
		case ExemptRole:
			out.ExemptRoleIDs = append(out.ExemptRoleIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return Settings{}, fmt.Errorf("guild: get exemptions for %s: %w", guildID, err)
	}
	return out, nil
}

// SetLogChannel points one of guildID's report channels at channelID.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild to configure.
//   - kind (LogKind): which report channel to change.
//   - channelID (string): new channel; "" clears it.
func (s *Store) SetLogChannel(ctx context.Context, guildID string, kind LogKind, channelID string) error {
	query, ok := setLogChannel[kind]
	if !ok {
		return fmt.Errorf("guild: unknown log channel kind %q", kind)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, ensureGuild, guildID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, query, guildID, channelID)
		return err
	})
	if err != nil {
		return fmt.Errorf("guild: set %s channel for %s: %w", kind, guildID, err)
	}
	return nil
}

// SetHoneypot makes channelID guildID's honeypot, purging purge worth of a
// softbanned member's messages.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild to configure.
//   - channelID (string): new honeypot channel; "" turns the honeypot off.
//   - purge (time.Duration): how far back a softban deletes messages, in
//     whole seconds from 0 to MaxHoneypotPurge.
func (s *Store) SetHoneypot(ctx context.Context, guildID, channelID string, purge time.Duration) error {
	if purge < 0 || purge > MaxHoneypotPurge || purge%time.Second != 0 {
		return fmt.Errorf("guild: honeypot purge %s must be whole seconds between 0 and %s", purge, MaxHoneypotPurge)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, ensureGuild, guildID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO guild_settings (guild_id, honeypot_channel_id, honeypot_purge_seconds) VALUES (?, ?, ?)
			ON CONFLICT (guild_id) DO UPDATE SET
				honeypot_channel_id = excluded.honeypot_channel_id,
				honeypot_purge_seconds = excluded.honeypot_purge_seconds,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
			guildID, channelID, int64(purge/time.Second))
		return err
	})
	if err != nil {
		return fmt.Errorf("guild: set honeypot for %s: %w", guildID, err)
	}
	return nil
}

// FormatPurge describes a honeypot purge for replies and reports, such as
// "6 hours" or "3 days".
//
// Parameters:
//   - d (time.Duration): purge to describe.
func FormatPurge(d time.Duration) string {
	n, unit := int64(d/time.Hour), "hour"
	switch {
	case d%(24*time.Hour) == 0:
		n, unit = int64(d/(24*time.Hour)), "day"
	case d%time.Hour != 0:
		n, unit = int64(d/time.Minute), "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return strconv.FormatInt(n, 10) + " " + unit
}

// AddExemption exempts a channel or role in guildID. It reports false when
// the exemption already existed.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild to configure.
//   - kind (ExemptionKind): whether targetID is a channel or a role.
//   - targetID (string): channel or role to exempt.
func (s *Store) AddExemption(ctx context.Context, guildID string, kind ExemptionKind, targetID string) (bool, error) {
	var res sql.Result
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, ensureGuild, guildID); err != nil {
			return err
		}
		var err error
		res, err = tx.ExecContext(ctx,
			`INSERT INTO guild_exemptions (guild_id, kind, target_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			guildID, kind, targetID)
		return err
	})
	return changed(res, err, "add", kind, guildID)
}

// RemoveExemption lifts an exemption in guildID. It reports false when there
// was nothing to remove.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild to configure.
//   - kind (ExemptionKind): whether targetID is a channel or a role.
//   - targetID (string): channel or role to stop exempting.
func (s *Store) RemoveExemption(ctx context.Context, guildID string, kind ExemptionKind, targetID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM guild_exemptions WHERE guild_id = ? AND kind = ? AND target_id = ?`,
		guildID, kind, targetID)
	return changed(res, err, "remove", kind, guildID)
}

// changed reports whether an exemption write touched a row.
//
// Parameters:
//   - res (sql.Result): result of the write; ignored when err is set.
//   - err (error): error from the write.
//   - verb (string): "add" or "remove", for the error message.
//   - kind (ExemptionKind): exemption kind, for the error message.
//   - guildID (string): guild, for the error message.
func changed(res sql.Result, err error, verb string, kind ExemptionKind, guildID string) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("guild: %s %s exemption for %s: %w", verb, kind, guildID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("guild: %s %s exemption for %s: %w", verb, kind, guildID, err)
	}
	return n > 0, nil
}

// RecordGuild records that the bot is in guildID, clearing removed_at for a
// guild that invited it back. A guild already present is left untouched, so
// the gateway replaying every guild at startup costs no writes.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild the bot is in.
func (s *Store) RecordGuild(ctx context.Context, guildID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO guilds (id) VALUES (?)
		ON CONFLICT (id) DO UPDATE SET
			removed_at = NULL,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE removed_at IS NOT NULL`,
		guildID)
	if err != nil {
		return fmt.Errorf("guild: record guild %s: %w", guildID, err)
	}
	return nil
}

// RecordRemoval marks guildID as left, keeping its settings in case the bot
// is invited back.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild the bot was removed from.
func (s *Store) RecordRemoval(ctx context.Context, guildID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE guilds SET
			removed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND removed_at IS NULL`,
		guildID)
	if err != nil {
		return fmt.Errorf("guild: record removal of %s: %w", guildID, err)
	}
	return nil
}

// inTx runs fn in a transaction, committing when it returns nil.
//
// Parameters:
//   - ctx (context.Context): bounds the transaction.
//   - fn (func(*sql.Tx) error): statements to run.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback is a no-op once Commit has succeeded.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
