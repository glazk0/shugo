package bot_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/bot"
)

// fakeRecorder records guild joins and removals.
type fakeRecorder struct {
	mu      sync.Mutex
	joined  []string
	removed []string
}

// RecordGuild implements bot.GuildRecorder.
//
// Parameters:
//   - _ (context.Context): unused.
//   - guildID (string): recorded.
func (f *fakeRecorder) RecordGuild(_ context.Context, guildID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = append(f.joined, guildID)
	return nil
}

// RecordRemoval implements bot.GuildRecorder.
//
// Parameters:
//   - _ (context.Context): unused.
//   - guildID (string): recorded.
func (f *fakeRecorder) RecordRemoval(_ context.Context, guildID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, guildID)
	return nil
}

func TestGuildEvents(t *testing.T) {
	t.Parallel()

	rec := &fakeRecorder{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	onCreate := bot.OnGuildCreate(t.Context(), rec, logger)
	onDelete := bot.OnGuildDelete(t.Context(), rec, logger)

	onCreate(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "a"}})
	onCreate(nil, &discordgo.GuildCreate{})
	// An outage is not a removal.
	onDelete(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "a", Unavailable: true}})
	onDelete(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "b"}})
	onDelete(nil, &discordgo.GuildDelete{})

	if !slices.Equal(rec.joined, []string{"a"}) || !slices.Equal(rec.removed, []string{"b"}) {
		t.Errorf("joined/removed = %v/%v, want [a]/[b]", rec.joined, rec.removed)
	}
}
