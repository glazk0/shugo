// Command shugo runs the Discord auto-moderation bot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/bot"
	"github.com/glazk0/shugo/internal/config"
	"github.com/glazk0/shugo/internal/database"
	"github.com/glazk0/shugo/internal/guild"
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
// process receives SIGINT or SIGTERM, then lets in-flight messages finish for
// up to the shutdown timeout. A second signal stops the process immediately.
func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	for _, w := range cfg.Warnings {
		logger.Warn(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	settings := guild.NewStore(db)

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

	handler := bot.NewHandler(moderator, store, settings, bot.NewSession(session), logger, bot.Options{
		DryRun:             cfg.DryRun,
		TimeoutDuration:    cfg.TimeoutDuration,
		QueueTimeout:       cfg.QueueTimeout,
		EvaluationTimeout:  cfg.EvaluationTimeout,
		EnforcementTimeout: cfg.EnforcementTimeout,
		MaxConcurrency:     cfg.MaxConcurrency,
	})

	// Evaluations run on their own context rather than the signal context,
	// so the shutdown signal stops new work without aborting Jev calls and
	// Discord actions that are already under way.
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()

	var tracker bot.Tracker
	session.AddHandler(bot.OnMessageCreate(workCtx, handler, &tracker, logger))
	session.AddHandler(bot.OnMessageUpdate(workCtx, handler, &tracker, logger))
	session.AddHandler(bot.OnInteractionCreate(workCtx, settings, &tracker, logger))
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		logger.Info("connected to discord",
			slog.String("user", r.User.Username),
			slog.Int("guilds", len(r.Guilds)))
		// Overwriting on every Ready is idempotent and keeps the commands in
		// step with this build after an upgrade.
		appID := r.User.ID
		if r.Application != nil {
			appID = r.Application.ID
		}
		if _, err := s.ApplicationCommandBulkOverwrite(appID, "", bot.Commands); err != nil {
			logger.Error("register slash commands", slog.Any("error", err))
		}
	})

	if err := session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	logger.Info("shugo started",
		slog.String("version", version),
		slog.String("jev_model", cfg.JevModel),
		slog.Bool("dry_run", cfg.DryRun))

	<-ctx.Done()
	stop()
	logger.Info("shutting down")

	closeErr := session.Close()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelDrain()
	if err := tracker.Close(drainCtx); err != nil {
		logger.Warn("abandoning in-flight messages", slog.Duration("shutdown_timeout", cfg.ShutdownTimeout))
	}
	cancelWork()
	if closeErr != nil {
		return fmt.Errorf("close discord gateway: %w", closeErr)
	}
	return nil
}
