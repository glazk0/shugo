package commands_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/commands"
	"github.com/glazk0/shugo/internal/database"
	"github.com/glazk0/shugo/internal/guild"
)

// newGuildStore returns a guild.Store over a fresh, migrated database.
//
// Parameters:
//   - t (*testing.T): registers cleanup and fails the test on errors.
func newGuildStore(t *testing.T) *guild.Store {
	t.Helper()

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "shugo.db"))
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return guild.NewStore(db)
}

// newRouter returns a Router serving /settings over store.
//
// Parameters:
//   - t (*testing.T): fails the test on errors.
//   - store (commands.SettingsStore): settings storage.
func newRouter(t *testing.T, store commands.SettingsStore) *commands.Router {
	t.Helper()

	r, err := commands.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), commands.Settings(store))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return r
}

// invocation builds /settings data for a subcommand path such as
// "log-channel set", passing opts to the innermost subcommand.
//
// Parameters:
//   - path (string): space-separated subcommand path.
//   - opts (map[string]string): option values by name.
func invocation(path string, opts map[string]string) discordgo.ApplicationCommandInteractionData {
	var leaf []*discordgo.ApplicationCommandInteractionDataOption
	for name, value := range opts {
		leaf = append(leaf, &discordgo.ApplicationCommandInteractionDataOption{
			Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value,
		})
	}

	parts := strings.Fields(path)
	options := leaf
	for i := len(parts) - 1; i >= 0; i-- {
		typ := discordgo.ApplicationCommandOptionSubCommand
		if i < len(parts)-1 {
			typ = discordgo.ApplicationCommandOptionSubCommandGroup
		}
		options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: parts[i], Type: typ, Options: options}}
	}
	return discordgo.ApplicationCommandInteractionData{Name: "settings", Options: options}
}

// banMembers is what a correctly invited Shugo holds for the honeypot.
const banMembers int64 = discordgo.PermissionBanMembers

// run invokes /settings in guild "g", as a bot holding Ban Members, and fails
// the test on storage errors.
//
// Parameters:
//   - t (*testing.T): fails the test on errors.
//   - r (*commands.Router): router serving /settings.
//   - path (string): subcommand path.
//   - opts (map[string]string): option values by name.
func run(t *testing.T, r *commands.Router, path string, opts map[string]string) string {
	t.Helper()

	reply, err := r.Dispatch(t.Context(), "g", banMembers, invocation(path, opts))
	if err != nil {
		t.Fatalf("Dispatch(%s) error = %v", path, err)
	}
	return reply
}

func TestSettingsLogChannels(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)
	r := newRouter(t, store)

	if reply := run(t, r, "show", nil); !strings.Contains(reply, "not reported") {
		t.Errorf("show on a new guild = %q, want reports to be off", reply)
	}

	if reply := run(t, r, "log-channel set", map[string]string{"kind": "actions", "channel": "a"}); !strings.Contains(reply, "<#a>") {
		t.Errorf("set reply = %q, want it to mention the channel", reply)
	}
	got, _ := store.Get(t.Context(), "g")
	if got.ActionChannelID != "a" || got.FlagChannelID != "" {
		t.Fatalf("settings = %+v, want only the action channel set", got)
	}
	if reply := run(t, r, "show", nil); !strings.Contains(reply, "<#a> (shared with deletes and timeouts)") {
		t.Errorf("show = %q, want flags to fall back to the action channel", reply)
	}

	run(t, r, "log-channel set", map[string]string{"kind": "flags", "channel": "f"})
	run(t, r, "log-channel clear", map[string]string{"kind": "actions"})
	got, _ = store.Get(t.Context(), "g")
	if got.FlagChannelID != "f" || got.ActionChannelID != "" {
		t.Errorf("settings = %+v, want flags f and actions cleared", got)
	}

	if reply := run(t, r, "log-channel set", map[string]string{"kind": "flags"}); reply != "Pick a channel." {
		t.Errorf("set without channel = %q", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); got.FlagChannelID != "f" {
		t.Errorf("set without channel changed the flag channel to %q", got.FlagChannelID)
	}
}

func TestSettingsExemptions(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)
	r := newRouter(t, store)

	if reply := run(t, r, "exempt-channel add", map[string]string{"channel": "c"}); !strings.Contains(reply, "now exempt") {
		t.Errorf("add channel = %q", reply)
	}
	if reply := run(t, r, "exempt-channel add", map[string]string{"channel": "c"}); !strings.Contains(reply, "already exempt") {
		t.Errorf("add channel twice = %q", reply)
	}
	run(t, r, "exempt-role add", map[string]string{"role": "r"})

	got, _ := store.Get(t.Context(), "g")
	if !slices.Equal(got.ExemptChannelIDs, []string{"c"}) || !slices.Equal(got.ExemptRoleIDs, []string{"r"}) {
		t.Fatalf("settings = %+v", got)
	}
	if reply := run(t, r, "show", nil); !strings.Contains(reply, "<#c>") || !strings.Contains(reply, "<@&r>") {
		t.Errorf("show = %q, want both exemptions listed", reply)
	}

	if reply := run(t, r, "exempt-role remove", map[string]string{"role": "r"}); !strings.Contains(reply, "moderated again") {
		t.Errorf("remove role = %q", reply)
	}
	if reply := run(t, r, "exempt-role remove", map[string]string{"role": "r"}); !strings.Contains(reply, "was not exempt") {
		t.Errorf("remove role twice = %q", reply)
	}
	if reply := run(t, r, "exempt-channel remove", map[string]string{"channel": "c"}); !strings.Contains(reply, "moderated again") {
		t.Errorf("remove channel = %q", reply)
	}

	// The @everyone role shares the guild ID.
	if reply := run(t, r, "exempt-role add", map[string]string{"role": "g"}); !strings.Contains(reply, "@everyone") {
		t.Errorf("exempting @everyone = %q, want a refusal", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); len(got.ExemptRoleIDs) != 0 {
		t.Errorf("ExemptRoleIDs = %v, want none", got.ExemptRoleIDs)
	}
}

func TestSettingsHoneypot(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)
	r := newRouter(t, store)

	if reply := run(t, r, "show", nil); !strings.Contains(reply, "**Honeypot:** off") {
		t.Errorf("show on a new guild = %q, want the honeypot off", reply)
	}

	reply := run(t, r, "honeypot set", map[string]string{"channel": "trap"})
	if !strings.Contains(reply, "<#trap>") || !strings.Contains(reply, "1 day") || strings.Contains(reply, "missing") {
		t.Errorf("set reply = %q, want the channel and the default purge, without a permission warning", reply)
	}
	got, _ := store.Get(t.Context(), "g")
	if got.HoneypotChannelID != "trap" || got.HoneypotPurge != guild.DefaultHoneypotPurge {
		t.Fatalf("settings = %+v, want trap with the default purge", got)
	}

	run(t, r, "honeypot set", map[string]string{"channel": "trap2", "purge": "6h"})
	got, _ = store.Get(t.Context(), "g")
	if got.HoneypotChannelID != "trap2" || got.HoneypotPurge != 6*time.Hour {
		t.Fatalf("settings = %+v, want trap2 purging 6 hours", got)
	}
	if reply := run(t, r, "show", nil); !strings.Contains(reply, "<#trap2> (deletes the last 6 hours of messages)") {
		t.Errorf("show = %q, want the honeypot and its purge", reply)
	}

	for _, purge := range []string{"forever", "0s", "-1h", "200h"} {
		if reply := run(t, r, "honeypot set", map[string]string{"channel": "other", "purge": purge}); !strings.Contains(reply, "Pick a purge window") {
			t.Errorf("set with purge %q = %q, want a refusal", purge, reply)
		}
	}
	if reply := run(t, r, "honeypot set", nil); reply != "Pick a channel." {
		t.Errorf("set without channel = %q", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); got.HoneypotChannelID != "trap2" {
		t.Errorf("rejected sets changed the honeypot to %q", got.HoneypotChannelID)
	}

	if reply := run(t, r, "honeypot clear", nil); !strings.Contains(reply, "off") {
		t.Errorf("clear reply = %q", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); got.HoneypotChannelID != "" {
		t.Errorf("after clear HoneypotChannelID = %q, want none", got.HoneypotChannelID)
	}
}

func TestSettingsHoneypotWarnsWithoutBanPermission(t *testing.T) {
	t.Parallel()

	r := newRouter(t, newGuildStore(t))
	tests := []struct {
		name        string
		permissions int64
		wantWarning bool
	}{
		{"no ban permission", discordgo.PermissionSendMessages, true},
		{"ban members", banMembers, false},
		{"administrator", discordgo.PermissionAdministrator, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reply, err := r.Dispatch(t.Context(), "g", tt.permissions,
				invocation("honeypot set", map[string]string{"channel": "trap"}))
			if err != nil {
				t.Fatalf("Dispatch() error = %v", err)
			}
			if got := strings.Contains(reply, "missing the Ban Members permission"); got != tt.wantWarning {
				t.Errorf("reply = %q, want warning %v", reply, tt.wantWarning)
			}
		})
	}
}

func TestSettingsOutsideGuild(t *testing.T) {
	t.Parallel()

	r := newRouter(t, newGuildStore(t))
	reply, err := r.Dispatch(t.Context(), "", 0, invocation("show", nil))
	if err != nil || !strings.Contains(reply, "server") {
		t.Errorf("Dispatch(no guild) = %q, %v", reply, err)
	}
}

func TestSettingsReturnsStorageErrors(t *testing.T) {
	t.Parallel()

	store := failingStore{err: errors.New("database is locked")}
	r := newRouter(t, store)
	for _, path := range []string{"show", "log-channel set", "log-channel clear", "exempt-channel add", "exempt-role remove", "honeypot set", "honeypot clear"} {
		_, err := r.Dispatch(t.Context(), "g", 0,
			invocation(path, map[string]string{"kind": "flags", "channel": "c", "role": "r"}))
		if !errors.Is(err, store.err) {
			t.Errorf("Dispatch(%s) error = %v, want the storage error", path, err)
		}
	}
}

func TestSettingsDefinitionIsValid(t *testing.T) {
	t.Parallel()

	// Discord rejects the whole bulk overwrite if any name or description
	// breaks its limits, which would only surface at startup.
	var check func(name, description string, opts []*discordgo.ApplicationCommandOption)
	check = func(name, description string, opts []*discordgo.ApplicationCommandOption) {
		if name == "" || len(name) > 32 || strings.ToLower(name) != name || strings.ContainsAny(name, " ") {
			t.Errorf("invalid name %q", name)
		}
		if description == "" || len(description) > 100 {
			t.Errorf("%s: description %q must be 1-100 characters", name, description)
		}
		for _, o := range opts {
			check(o.Name, o.Description, o.Options)
		}
	}
	def := commands.Settings(failingStore{}).Definition
	check(def.Name, def.Description, def.Options)
}

// failingStore fails every operation with err.
type failingStore struct {
	err error
}

// Get implements commands.SettingsStore.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
func (f failingStore) Get(context.Context, string) (guild.Settings, error) {
	return guild.Settings{}, f.err
}

// SetLogChannel implements commands.SettingsStore.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.LogKind): unused.
//   - _ (string): channel ID, unused.
func (f failingStore) SetLogChannel(context.Context, string, guild.LogKind, string) error {
	return f.err
}

// AddExemption implements commands.SettingsStore.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.ExemptionKind): unused.
//   - _ (string): target ID, unused.
func (f failingStore) AddExemption(context.Context, string, guild.ExemptionKind, string) (bool, error) {
	return false, f.err
}

// RemoveExemption implements commands.SettingsStore.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.ExemptionKind): unused.
//   - _ (string): target ID, unused.
func (f failingStore) RemoveExemption(context.Context, string, guild.ExemptionKind, string) (bool, error) {
	return false, f.err
}

// SetHoneypot implements commands.SettingsStore.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (string): channel ID, unused.
//   - _ (time.Duration): purge, unused.
func (f failingStore) SetHoneypot(context.Context, string, string, time.Duration) error {
	return f.err
}
