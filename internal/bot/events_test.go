package bot_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/bot"
	"github.com/glazk0/shugo/internal/moderation"
)

// gatewayMessage builds a guild message as delivered by the gateway.
//
// Parameters:
//   - id (string): message ID.
//   - content (string): message text.
func gatewayMessage(id, content string) *discordgo.Message {
	return &discordgo.Message{
		ID:        id,
		GuildID:   "g",
		ChannelID: "c",
		Content:   content,
		Timestamp: time.Now(),
		Author:    &discordgo.User{ID: "175928847299117063"},
		Member:    &discordgo.Member{},
	}
}

func TestOnMessageUpdateModeratesEdits(t *testing.T) {
	t.Parallel()

	h, mod, dc := setup(moderation.Verdict{Action: moderation.ActionDelete, Category: "scam"}, defaultOpts)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var tracker bot.Tracker
	s := &discordgo.Session{State: discordgo.NewState()}

	bot.OnMessageCreate(t.Context(), h, &tracker, logger)(s, &discordgo.MessageCreate{Message: gatewayMessage("1", "hi")})

	edited := gatewayMessage("1", "free nitro https://dlscord.gift/claim")
	editedAt := time.Now()
	edited.EditedTimestamp = &editedAt
	bot.OnMessageUpdate(t.Context(), h, &tracker, logger)(s, &discordgo.MessageUpdate{Message: edited})

	if mod.calls() != 2 {
		t.Fatalf("moderator called %d times, want 2", mod.calls())
	}
	got := mod.inputs[1]
	if got.Message.Content != edited.Content {
		t.Errorf("edit evaluated %q, want the edited content", got.Message.Content)
	}
	if len(got.History) != 0 {
		t.Errorf("edit history = %+v, want the original version left out", got.History)
	}
	if len(dc.deleted) != 2 || dc.deleted[1] != "1" {
		t.Errorf("deleted = %v, want the edited message deleted", dc.deleted)
	}
}

func TestOnMessageUpdateIgnoresNonEdits(t *testing.T) {
	t.Parallel()

	h, mod, _ := setup(moderation.Verdict{}, defaultOpts)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var tracker bot.Tracker
	s := &discordgo.Session{State: discordgo.NewState()}
	onUpdate := bot.OnMessageUpdate(t.Context(), h, &tracker, logger)

	// An embed unfurl or pin carries no EditedTimestamp.
	onUpdate(s, &discordgo.MessageUpdate{Message: gatewayMessage("1", "https://example.com")})
	onUpdate(s, &discordgo.MessageUpdate{})

	if mod.calls() != 0 {
		t.Errorf("moderator called %d times, want 0", mod.calls())
	}
}

func TestOnMessageCreateStopsAfterTrackerClose(t *testing.T) {
	t.Parallel()

	h, mod, _ := setup(moderation.Verdict{}, defaultOpts)
	var tracker bot.Tracker
	if err := tracker.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	onCreate := bot.OnMessageCreate(t.Context(), h, &tracker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	onCreate(&discordgo.Session{State: discordgo.NewState()}, &discordgo.MessageCreate{Message: gatewayMessage("1", "hi")})

	if mod.calls() != 0 {
		t.Errorf("moderator called %d times after shutdown, want 0", mod.calls())
	}
}
