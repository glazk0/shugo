package bot_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/bot"
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

// invocation builds /config data for a subcommand path such as
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
	return discordgo.ApplicationCommandInteractionData{Name: "config", Options: options}
}

// run invokes /config in guild "g" and fails the test on storage errors.
//
// Parameters:
//   - t (*testing.T): fails the test on errors.
//   - editor (bot.SettingsEditor): settings store.
//   - path (string): subcommand path.
//   - opts (map[string]string): option values by name.
func run(t *testing.T, editor bot.SettingsEditor, path string, opts map[string]string) string {
	t.Helper()

	reply, err := bot.RunConfigCommand(t.Context(), editor, "g", invocation(path, opts))
	if err != nil {
		t.Fatalf("RunConfigCommand(%s) error = %v", path, err)
	}
	return reply
}

func TestConfigLogChannels(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)

	if reply := run(t, store, "show", nil); !strings.Contains(reply, "not reported") {
		t.Errorf("show on a new guild = %q, want reports to be off", reply)
	}

	if reply := run(t, store, "log-channel set", map[string]string{"kind": "actions", "channel": "a"}); !strings.Contains(reply, "<#a>") {
		t.Errorf("set reply = %q, want it to mention the channel", reply)
	}
	got, _ := store.Get(t.Context(), "g")
	if got.ActionChannelID != "a" || got.FlagChannelID != "" {
		t.Fatalf("settings = %+v, want only the action channel set", got)
	}
	if reply := run(t, store, "show", nil); !strings.Contains(reply, "<#a> (shared with deletes and timeouts)") {
		t.Errorf("show = %q, want flags to fall back to the action channel", reply)
	}

	run(t, store, "log-channel set", map[string]string{"kind": "flags", "channel": "f"})
	run(t, store, "log-channel clear", map[string]string{"kind": "actions"})
	got, _ = store.Get(t.Context(), "g")
	if got.FlagChannelID != "f" || got.ActionChannelID != "" {
		t.Errorf("settings = %+v, want flags f and actions cleared", got)
	}

	if reply := run(t, store, "log-channel set", map[string]string{"kind": "flags"}); reply != "Pick a channel." {
		t.Errorf("set without channel = %q", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); got.FlagChannelID != "f" {
		t.Errorf("set without channel changed the flag channel to %q", got.FlagChannelID)
	}
}

func TestConfigExemptions(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)

	if reply := run(t, store, "exempt-channel add", map[string]string{"channel": "c"}); !strings.Contains(reply, "now exempt") {
		t.Errorf("add channel = %q", reply)
	}
	if reply := run(t, store, "exempt-channel add", map[string]string{"channel": "c"}); !strings.Contains(reply, "already exempt") {
		t.Errorf("add channel twice = %q", reply)
	}
	run(t, store, "exempt-role add", map[string]string{"role": "r"})

	got, _ := store.Get(t.Context(), "g")
	if !slices.Equal(got.ExemptChannelIDs, []string{"c"}) || !slices.Equal(got.ExemptRoleIDs, []string{"r"}) {
		t.Fatalf("settings = %+v", got)
	}
	if reply := run(t, store, "show", nil); !strings.Contains(reply, "<#c>") || !strings.Contains(reply, "<@&r>") {
		t.Errorf("show = %q, want both exemptions listed", reply)
	}

	if reply := run(t, store, "exempt-role remove", map[string]string{"role": "r"}); !strings.Contains(reply, "moderated again") {
		t.Errorf("remove role = %q", reply)
	}
	if reply := run(t, store, "exempt-role remove", map[string]string{"role": "r"}); !strings.Contains(reply, "was not exempt") {
		t.Errorf("remove role twice = %q", reply)
	}

	// The @everyone role shares the guild ID.
	if reply := run(t, store, "exempt-role add", map[string]string{"role": "g"}); !strings.Contains(reply, "@everyone") {
		t.Errorf("exempting @everyone = %q, want a refusal", reply)
	}
	if got, _ := store.Get(t.Context(), "g"); len(got.ExemptRoleIDs) != 0 {
		t.Errorf("ExemptRoleIDs = %v, want none", got.ExemptRoleIDs)
	}
}

func TestConfigOutsideGuildAndUnknownCommand(t *testing.T) {
	t.Parallel()

	store := newGuildStore(t)

	reply, err := bot.RunConfigCommand(t.Context(), store, "", invocation("show", nil))
	if err != nil || !strings.Contains(reply, "server") {
		t.Errorf("RunConfigCommand(no guild) = %q, %v", reply, err)
	}
	if reply := run(t, store, "bogus", nil); !strings.Contains(reply, "Unknown command") {
		t.Errorf("unknown subcommand = %q", reply)
	}
}

func TestConfigReturnsStorageErrors(t *testing.T) {
	t.Parallel()

	editor := failingEditor{err: errors.New("database is locked")}
	for _, path := range []string{"show", "log-channel set", "exempt-channel add", "exempt-role remove"} {
		_, err := bot.RunConfigCommand(t.Context(), editor, "g",
			invocation(path, map[string]string{"kind": "flags", "channel": "c", "role": "r"}))
		if !errors.Is(err, editor.err) {
			t.Errorf("RunConfigCommand(%s) error = %v, want the storage error", path, err)
		}
	}
}

func TestCommandsAreValid(t *testing.T) {
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
	for _, c := range bot.Commands {
		check(c.Name, c.Description, c.Options)
	}
}

// failingEditor fails every operation with err.
type failingEditor struct {
	err error
}

// Get implements bot.SettingsEditor.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
func (f failingEditor) Get(context.Context, string) (guild.Settings, error) {
	return guild.Settings{}, f.err
}

// SetLogChannel implements bot.SettingsEditor.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.LogKind): unused.
//   - _ (string): channel ID, unused.
func (f failingEditor) SetLogChannel(context.Context, string, guild.LogKind, string) error {
	return f.err
}

// AddExemption implements bot.SettingsEditor.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.ExemptionKind): unused.
//   - _ (string): target ID, unused.
func (f failingEditor) AddExemption(context.Context, string, guild.ExemptionKind, string) (bool, error) {
	return false, f.err
}

// RemoveExemption implements bot.SettingsEditor.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
//   - _ (guild.ExemptionKind): unused.
//   - _ (string): target ID, unused.
func (f failingEditor) RemoveExemption(context.Context, string, guild.ExemptionKind, string) (bool, error) {
	return false, f.err
}
