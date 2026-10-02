// Command shugo runs the Discord auto-moderation bot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/bot"
	"github.com/glazk0/shugo/internal/config"
	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/jev"
	"github.com/glazk0/shugo/internal/moderation"
)

// version is overridden at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("shugo stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads the configuration, connects to Discord and blocks until the
// process receives SIGINT or SIGTERM.
func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := jev.NewClient(cfg.TypeSafeAPIKey,
		jev.WithEndpoint(cfg.JevEndpoint),
		jev.WithModel(cfg.JevModel),
	)
	moderator, err := moderation.New(client, cfg.Policy)
	if err != nil {
		return err
	}

	store := history.New(cfg.HistorySize, cfg.HistoryTTL)
	go store.Run(ctx, time.Minute)

	session, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}
	session.Identify.Intents = bot.Intents

	handler := bot.NewHandler(moderator, store, bot.NewSession(session), logger, bot.Options{
		LogChannelID:      cfg.LogChannelID,
		DryRun:            cfg.DryRun,
		TimeoutDuration:   cfg.TimeoutDuration,
		EvaluationTimeout: cfg.EvaluationTimeout,
		MaxConcurrency:    cfg.MaxConcurrency,
	})

	var inflight sync.WaitGroup
	session.AddHandler(bot.OnMessageCreate(ctx, handler, &inflight, logger))
	session.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		logger.Info("connected to discord",
			slog.String("user", r.User.Username),
			slog.Int("guilds", len(r.Guilds)))
	})

	if err := session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	logger.Info("shugo started",
		slog.String("version", version),
		slog.String("jev_model", cfg.JevModel),
		slog.Bool("dry_run", cfg.DryRun))

	<-ctx.Done()
	logger.Info("shutting down")

	closeErr := session.Close()
	inflight.Wait()
	if closeErr != nil {
		return fmt.Errorf("close discord gateway: %w", closeErr)
	}
	return nil
}
