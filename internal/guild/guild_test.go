package guild_test

import (
	"path/filepath"
	"slices"
	"testing"

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

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "shugo.db"))
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return guild.NewStore(db)
}

func TestGetReturnsDefaultsForUnknownGuild(t *testing.T) {
	t.Parallel()

	got, err := newStore(t).Get(t.Context(), "g")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.FlagChannelID != "" || got.ActionChannelID != "" || got.ExemptChannelIDs != nil || got.ExemptRoleIDs != nil {
		t.Errorf("Get() = %+v, want zero settings", got)
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
