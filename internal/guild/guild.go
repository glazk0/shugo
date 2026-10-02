// Package guild stores the settings each Discord guild configures for itself:
// where reports go and which channels and roles moderation skips.
package guild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

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

// Settings is one guild's configuration. The zero value is the default:
// reports are off and nothing is exempt.
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

// ExemptsAnyRole reports whether any of roleIDs is exempt.
//
// Parameters:
//   - roleIDs ([]string): roles held by a member.
func (s Settings) ExemptsAnyRole(roleIDs []string) bool {
	return slices.ContainsFunc(roleIDs, func(id string) bool {
		return slices.Contains(s.ExemptRoleIDs, id)
	})
}

// setLogChannel holds one upsert per LogKind, so column names never come
// from a variable.
var setLogChannel = map[LogKind]string{
	LogFlags: `INSERT INTO guild_settings (guild_id, flag_channel_id) VALUES (?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET flag_channel_id = excluded.flag_channel_id`,
	LogActions: `INSERT INTO guild_settings (guild_id, action_channel_id) VALUES (?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET action_channel_id = excluded.action_channel_id`,
}

// Store reads and writes guild settings in SQLite. It is safe for
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
	var out Settings
	err := s.db.QueryRowContext(ctx,
		`SELECT flag_channel_id, action_channel_id FROM guild_settings WHERE guild_id = ?`, guildID,
	).Scan(&out.FlagChannelID, &out.ActionChannelID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
	if _, err := s.db.ExecContext(ctx, query, guildID, channelID); err != nil {
		return fmt.Errorf("guild: set %s channel for %s: %w", kind, guildID, err)
	}
	return nil
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
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO guild_exemptions (guild_id, kind, target_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		guildID, kind, targetID)
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
