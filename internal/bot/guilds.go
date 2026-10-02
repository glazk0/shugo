package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

// recordTimeout bounds one guilds table write.
const recordTimeout = 5 * time.Second

// GuildRecorder keeps the guilds table in step with the gateway.
// *guild.Store satisfies it.
type GuildRecorder interface {
	RecordGuild(ctx context.Context, guildID string) error
	RecordRemoval(ctx context.Context, guildID string) error
}

// OnGuildCreate returns a discordgo handler that records every guild the bot
// is in. The gateway sends GUILD_CREATE for each guild at startup and
// whenever the bot joins one.
//
// Parameters:
//   - ctx (context.Context): parent context for the write.
//   - recorder (GuildRecorder): guilds table.
//   - logger (*slog.Logger): receives write errors.
func OnGuildCreate(ctx context.Context, recorder GuildRecorder, logger *slog.Logger) func(*discordgo.Session, *discordgo.GuildCreate) {
	return func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		if g.Guild == nil {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, recordTimeout)
		defer cancel()
		if err := recorder.RecordGuild(writeCtx, g.ID); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "record guild",
				slog.String("guild_id", g.ID),
				slog.Any("error", err))
		}
	}
}

// OnGuildDelete returns a discordgo handler that records when the bot leaves
// a guild, by being kicked or the guild being deleted.
//
// Parameters:
//   - ctx (context.Context): parent context for the write.
//   - recorder (GuildRecorder): guilds table.
//   - logger (*slog.Logger): receives write errors.
func OnGuildDelete(ctx context.Context, recorder GuildRecorder, logger *slog.Logger) func(*discordgo.Session, *discordgo.GuildDelete) {
	return func(_ *discordgo.Session, g *discordgo.GuildDelete) {
		// An unavailable guild is a Discord outage, not a removal.
		if g.Guild == nil || g.Unavailable {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, recordTimeout)
		defer cancel()
		if err := recorder.RecordRemoval(writeCtx, g.ID); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "record guild removal",
				slog.String("guild_id", g.ID),
				slog.Any("error", err))
		}
	}
}
