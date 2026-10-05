package guild_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/database"
	"github.com/glazk0/shugo/internal/guild"
	"github.com/glazk0/shugo/internal/moderation"
)

// newStore returns a Store over a fresh, migrated database.
//
// Parameters:
//   - t (*testing.T): registers cleanup and fails the test on errors.
func newStore(t *testing.T) *guild.Store {
	t.Helper()

	store, _ := newStoreWithDB(t)
	return store
}

// newStoreWithDB returns a Store over a fresh, migrated database, along with
// the database itself for inspecting rows directly.
//
// Parameters:
//   - t (*testing.T): registers cleanup and fails the test on errors.
func newStoreWithDB(t *testing.T) (*guild.Store, *sql.DB) {
	t.Helper()

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "shugo.db"))
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return guild.NewStore(db), db
}

// guildRow is a row of the guilds table.
type guildRow struct {
	createdAt, updatedAt string
	removedAt            sql.NullString
}

// readGuild loads the guilds row for id.
//
// Parameters:
//   - t (*testing.T): fails the test on query errors.
//   - db (*sql.DB): database to read.
//   - id (string): guild ID.
func readGuild(t *testing.T, db *sql.DB, id string) guildRow {
	t.Helper()

	var r guildRow
	err := db.QueryRowContext(t.Context(),
		`SELECT created_at, updated_at, removed_at FROM guilds WHERE id = ?`, id,
	).Scan(&r.createdAt, &r.updatedAt, &r.removedAt)
	if err != nil {
		t.Fatalf("read guild %s: %v", id, err)
	}
	return r
}

func TestRecordGuildLifecycle(t *testing.T) {
	t.Parallel()

	s, db := newStoreWithDB(t)
	ctx := t.Context()

	if err := s.RecordGuild(ctx, "g"); err != nil {
		t.Fatalf("RecordGuild() error = %v", err)
	}
	joined := readGuild(t, db, "g")
	if joined.removedAt.Valid {
		t.Fatalf("guild after join = %+v", joined)
	}
	if _, err := time.Parse(time.RFC3339Nano, joined.createdAt); err != nil {
		t.Errorf("created_at %q is not RFC 3339: %v", joined.createdAt, err)
	}
	if err := s.SetLogChannel(ctx, "g", guild.LogFlags, "f"); err != nil {
		t.Fatalf("SetLogChannel() error = %v", err)
	}

	if err := s.RecordRemoval(ctx, "g"); err != nil {
		t.Fatalf("RecordRemoval() error = %v", err)
	}
	if removed := readGuild(t, db, "g"); !removed.removedAt.Valid {
		t.Errorf("removed_at not set after removal: %+v", removed)
	}
	// Settings survive the removal so a re-invite keeps them.
	if got, _ := s.Get(ctx, "g"); got.FlagChannelID != "f" {
		t.Errorf("FlagChannelID after removal = %q, want f", got.FlagChannelID)
	}

	if err := s.RecordGuild(ctx, "g"); err != nil {
		t.Fatalf("RecordGuild() error = %v", err)
	}
	back := readGuild(t, db, "g")
	if back.removedAt.Valid || back.createdAt != joined.createdAt {
		t.Errorf("guild after rejoin = %+v, want not removed and the same created_at", back)
	}
	if back.updatedAt < joined.updatedAt {
		t.Errorf("updated_at went backwards: %q < %q", back.updatedAt, joined.updatedAt)
	}

	if err := s.RecordRemoval(ctx, "unknown"); err != nil {
		t.Errorf("RecordRemoval(unknown) error = %v", err)
	}
}

func TestSettingsWriteCreatesGuild(t *testing.T) {
	t.Parallel()

	s, db := newStoreWithDB(t)
	if _, err := s.AddExemption(t.Context(), "g", guild.ExemptRole, "r"); err != nil {
		t.Fatalf("AddExemption() error = %v", err)
	}
	if row := readGuild(t, db, "g"); row.removedAt.Valid {
		t.Errorf("guild row = %+v", row)
	}
}

func TestGetReturnsDefaultsForUnknownGuild(t *testing.T) {
	t.Parallel()

	got, err := newStore(t).Get(t.Context(), "g")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.FlagChannelID != "" || got.ActionChannelID != "" || got.ExemptChannelIDs != nil || got.ExemptRoleIDs != nil ||
		got.HoneypotChannelID != "" || got.HoneypotPurge != guild.DefaultHoneypotPurge {
		t.Errorf("Get() = %+v, want default settings", got)
	}
}

func TestSetLogChannel(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := t.Context()

	if err := s.SetLogChannel(ctx, "g", guild.LogFlags, "flags"); err != nil {
		t.Fatalf("SetLogChannel(flags) error = %v", err)
	}
	if err := s.SetLogChannel(ctx, "g", guild.LogActions, "actions"); err != nil {
		t.Fatalf("SetLogChannel(actions) error = %v", err)
	}
	// Changing one channel must leave the other alone.
	if err := s.SetLogChannel(ctx, "g", guild.LogFlags, "flags2"); err != nil {
		t.Fatalf("SetLogChannel(flags) error = %v", err)
	}

	got, err := s.Get(ctx, "g")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.FlagChannelID != "flags2" || got.ActionChannelID != "actions" {
		t.Errorf("Get() = %+v, want flags2/actions", got)
	}

	if err := s.SetLogChannel(ctx, "g", guild.LogActions, ""); err != nil {
		t.Fatalf("clear actions error = %v", err)
	}
	if got, _ := s.Get(ctx, "g"); got.ActionChannelID != "" || got.FlagChannelID != "flags2" {
		t.Errorf("after clear Get() = %+v", got)
	}

	if other, _ := s.Get(ctx, "other"); other.FlagChannelID != "" {
		t.Errorf("settings leaked into another guild: %+v", other)
	}
	if err := s.SetLogChannel(ctx, "g", "bogus", "c"); err == nil {
		t.Error("SetLogChannel(bogus) error = nil, want an error")
	}
}

func TestSetHoneypot(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := t.Context()

	if err := s.SetLogChannel(ctx, "g", guild.LogActions, "actions"); err != nil {
		t.Fatalf("SetLogChannel() error = %v", err)
	}
	// A guild that configured something else keeps the default purge.
	if got, _ := s.Get(ctx, "g"); got.HoneypotChannelID != "" || got.HoneypotPurge != guild.DefaultHoneypotPurge {
		t.Errorf("Get() before SetHoneypot = %+v", got)
	}

	if err := s.SetHoneypot(ctx, "g", "trap", 6*time.Hour); err != nil {
		t.Fatalf("SetHoneypot() error = %v", err)
	}
	got, err := s.Get(ctx, "g")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.HoneypotChannelID != "trap" || got.HoneypotPurge != 6*time.Hour || got.ActionChannelID != "actions" {
		t.Errorf("Get() = %+v, want trap purging 6h and the action channel kept", got)
	}

	if err := s.SetHoneypot(ctx, "g", "", guild.DefaultHoneypotPurge); err != nil {
		t.Fatalf("clear honeypot error = %v", err)
	}
	if got, _ := s.Get(ctx, "g"); got.HoneypotChannelID != "" {
		t.Errorf("after clear HoneypotChannelID = %q", got.HoneypotChannelID)
	}

	for _, purge := range []time.Duration{-time.Second, guild.MaxHoneypotPurge + time.Second, 1500 * time.Millisecond} {
		if err := s.SetHoneypot(ctx, "g", "trap", purge); err == nil {
			t.Errorf("SetHoneypot(purge %s) error = nil, want an error", purge)
		}
	}
	if err := s.SetHoneypot(ctx, "g", "trap", guild.MaxHoneypotPurge); err != nil {
		t.Errorf("SetHoneypot(max purge) error = %v", err)
	}
}

func TestFormatPurge(t *testing.T) {
	t.Parallel()

	tests := map[time.Duration]string{
		time.Hour:              "1 hour",
		12 * time.Hour:         "12 hours",
		24 * time.Hour:         "1 day",
		guild.MaxHoneypotPurge: "7 days",
		36 * time.Hour:         "36 hours",
		90 * time.Minute:       "90 minutes",
	}
	for d, want := range tests {
		if got := guild.FormatPurge(d); got != want {
			t.Errorf("FormatPurge(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestExemptions(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ctx := t.Context()

	for _, e := range []struct {
		kind guild.ExemptionKind
		id   string
	}{
		{guild.ExemptChannel, "c2"},
		{guild.ExemptChannel, "c1"},
		{guild.ExemptRole, "r1"},
	} {
		added, err := s.AddExemption(ctx, "g", e.kind, e.id)
		if err != nil || !added {
			t.Fatalf("AddExemption(%s, %s) = %v, %v; want true, nil", e.kind, e.id, added, err)
		}
	}
	if added, err := s.AddExemption(ctx, "g", guild.ExemptChannel, "c1"); err != nil || added {
		t.Errorf("duplicate AddExemption() = %v, %v; want false, nil", added, err)
	}

	got, err := s.Get(ctx, "g")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !slices.Equal(got.ExemptChannelIDs, []string{"c1", "c2"}) || !slices.Equal(got.ExemptRoleIDs, []string{"r1"}) {
		t.Errorf("Get() = %+v", got)
	}

	if removed, err := s.RemoveExemption(ctx, "g", guild.ExemptChannel, "c1"); err != nil || !removed {
		t.Errorf("RemoveExemption() = %v, %v; want true, nil", removed, err)
	}
	if removed, err := s.RemoveExemption(ctx, "g", guild.ExemptRole, "c2"); err != nil || removed {
		t.Errorf("RemoveExemption(wrong kind) = %v, %v; want false, nil", removed, err)
	}
	if got, _ := s.Get(ctx, "g"); !slices.Equal(got.ExemptChannelIDs, []string{"c2"}) {
		t.Errorf("after remove ExemptChannelIDs = %v, want [c2]", got.ExemptChannelIDs)
	}

	if _, err := s.AddExemption(ctx, "g", "bogus", "x"); err == nil {
		t.Error("AddExemption(bogus kind) error = nil, want the CHECK constraint to reject it")
	}
}

func TestReportChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings guild.Settings
		action   moderation.Action
		want     string
	}{
		{"flag to flag channel", guild.Settings{FlagChannelID: "f", ActionChannelID: "a"}, moderation.ActionFlag, "f"},
		{"delete to action channel", guild.Settings{FlagChannelID: "f", ActionChannelID: "a"}, moderation.ActionDelete, "a"},
		{"timeout to action channel", guild.Settings{FlagChannelID: "f", ActionChannelID: "a"}, moderation.ActionTimeout, "a"},
		{"flag falls back", guild.Settings{ActionChannelID: "a"}, moderation.ActionFlag, "a"},
		{"delete falls back", guild.Settings{FlagChannelID: "f"}, moderation.ActionDelete, "f"},
		{"none configured", guild.Settings{}, moderation.ActionTimeout, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.settings.ReportChannel(tt.action); got != tt.want {
				t.Errorf("ReportChannel(%s) = %q, want %q", tt.action, got, tt.want)
			}
		})
	}
}

func TestExempts(t *testing.T) {
	t.Parallel()

	s := guild.Settings{ExemptChannelIDs: []string{"c"}, ExemptRoleIDs: []string{"r"}}

	if !s.ExemptsChannel("c") || s.ExemptsChannel("other") || s.ExemptsChannel("") {
		t.Error("ExemptsChannel() gave the wrong answer")
	}
	if !s.ExemptsAnyRole([]string{"x", "r"}) || s.ExemptsAnyRole([]string{"x"}) || s.ExemptsAnyRole(nil) {
		t.Error("ExemptsAnyRole() gave the wrong answer")
	}
}

func TestIsHoneypot(t *testing.T) {
	t.Parallel()

	s := guild.Settings{HoneypotChannelID: "trap"}
	if !s.IsHoneypot("trap") || s.IsHoneypot("other") || s.IsHoneypot("") {
		t.Error("IsHoneypot() gave the wrong answer")
	}
	if (guild.Settings{}).IsHoneypot("") {
		t.Error("IsHoneypot(\"\") is true without a honeypot")
	}
}
