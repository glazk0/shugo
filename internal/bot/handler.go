// Package bot connects the moderator to Discord: it turns gateway events into
// moderation inputs, keeps per-member history and enforces verdicts.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/moderation"
)

// ErrOverloaded is returned when a message waited longer than
// Options.QueueTimeout for an evaluation slot and was dropped.
var ErrOverloaded = errors.New("bot: moderation queue is full")

// Message is a guild message reduced to what moderation needs, independent
// of the Discord library.
type Message struct {
	ID        string
	GuildID   string
	ChannelID string
	AuthorID  string
	Content   string
	SentAt    time.Time

	Attachments      int
	UserMentions     int
	MentionsEveryone bool

	// AuthorCreatedAt is when the author's account was created.
	AuthorCreatedAt time.Time
	// JoinedAt is when the author joined the guild; zero if unknown.
	JoinedAt time.Time
	// AuthorIsBot is true for bot and webhook authors.
	AuthorIsBot bool
	// Exempt is true for authors who moderate the channel themselves.
	Exempt bool
}

// Report describes an enforcement for the moderation log.
type Report struct {
	Message Message
	Verdict moderation.Verdict
	// DryRun is true when the action was only simulated.
	DryRun bool
	// Err holds any enforcement failure, nil on success.
	Err error
}

// Discord performs the REST calls needed to enforce verdicts.
type Discord interface {
	DeleteMessage(ctx context.Context, channelID, messageID, reason string) error
	TimeoutMember(ctx context.Context, guildID, userID string, until time.Time, reason string) error
	SendReport(ctx context.Context, channelID string, r Report) error
}

// Moderator judges a message. *moderation.Moderator satisfies it.
type Moderator interface {
	Moderate(ctx context.Context, in moderation.Input) (moderation.Verdict, error)
}

// Options tunes the Handler.
type Options struct {
	// LogChannelID receives reports; empty disables them.
	LogChannelID string
	// DryRun reports verdicts without deleting or timing out.
	DryRun bool
	// TimeoutDuration is how long offending members are timed out for.
	TimeoutDuration time.Duration
	// QueueTimeout bounds how long a message waits for an evaluation slot.
	// A verdict that arrives long after the message was posted is of little
	// use, and an unbounded queue grows without limit during a raid.
	QueueTimeout time.Duration
	// EvaluationTimeout bounds the Jev evaluation of one message.
	EvaluationTimeout time.Duration
	// EnforcementTimeout bounds the delete and timeout calls for one verdict,
	// and separately the report, so a slow evaluation cannot use up the time
	// needed to act on it.
	EnforcementTimeout time.Duration
	// MaxConcurrency caps in-flight evaluations.
	MaxConcurrency int
}

// Handler moderates incoming messages. It is safe for concurrent use.
type Handler struct {
	moderator Moderator
	history   *history.Store
	discord   Discord
	logger    *slog.Logger
	opts      Options
	sem       chan struct{}
	now       func() time.Time
}

// NewHandler wires a Handler together.
//
// Parameters:
//   - moderator (Moderator): judges each message.
//   - store (*history.Store): per-member message history.
//   - discord (Discord): enforces verdicts.
//   - logger (*slog.Logger): structured logger.
//   - opts (Options): behaviour settings.
func NewHandler(moderator Moderator, store *history.Store, discord Discord, logger *slog.Logger, opts Options) *Handler {
	return &Handler{
		moderator: moderator,
		history:   store,
		discord:   discord,
		logger:    logger,
		opts:      opts,
		sem:       make(chan struct{}, max(opts.MaxConcurrency, 1)),
		now:       time.Now,
	}
}

// Handle moderates msg and enforces the resulting verdict. Messages from
// bots, outside guilds or from exempt members are ignored. An edited message
// is handled like a new one, replacing its earlier version in the history.
//
// Parameters:
//   - ctx (context.Context): cancels evaluation and enforcement.
//   - msg (Message): the message to moderate.
func (h *Handler) Handle(ctx context.Context, msg Message) error {
	if msg.GuildID == "" || msg.AuthorIsBot || msg.Exempt {
		return nil
	}

	now := h.now()
	prior := h.history.Record(msg.GuildID, msg.AuthorID, history.Entry{
		MessageID: msg.ID,
		ChannelID: msg.ChannelID,
		Content:   msg.Content,
		At:        msg.SentAt,
	}, now)

	if strings.TrimSpace(msg.Content) == "" {
		// Attachment-only messages carry nothing Jev can read yet.
		return nil
	}

	if err := h.acquire(ctx); err != nil {
		return fmt.Errorf("moderate message %s: %w", msg.ID, err)
	}
	defer func() { <-h.sem }()

	evalCtx, cancel := context.WithTimeout(ctx, h.opts.EvaluationTimeout)
	defer cancel()

	verdict, err := h.moderator.Moderate(evalCtx, moderation.Input{
		Message: moderation.Message{
			ChannelID:        msg.ChannelID,
			Content:          msg.Content,
			Attachments:      msg.Attachments,
			UserMentions:     msg.UserMentions,
			MentionsEveryone: msg.MentionsEveryone,
		},
		Author: moderation.Author{
			CreatedAt: msg.AuthorCreatedAt,
			JoinedAt:  msg.JoinedAt,
		},
		History: prior,
		Now:     now,
	})
	if err != nil {
		return fmt.Errorf("moderate message %s: %w", msg.ID, err)
	}

	h.logger.LogAttrs(ctx, slog.LevelDebug, "message evaluated",
		slog.String("guild_id", msg.GuildID),
		slog.String("message_id", msg.ID),
		slog.String("author_id", msg.AuthorID),
		slog.String("action", verdict.Action.String()),
		slog.String("category", verdict.Category),
		slog.Float64("risk", verdict.Risk),
		slog.Float64("severity", verdict.Severity),
		slog.Float64("suspicion", verdict.Suspicion),
		slog.Int("input_tokens", verdict.Usage.InputTokens),
	)

	if verdict.Action == moderation.ActionNone {
		return nil
	}
	return h.enforce(ctx, msg, verdict)
}

// acquire takes an evaluation slot, waiting at most Options.QueueTimeout.
//
// Parameters:
//   - ctx (context.Context): aborts the wait.
func (h *Handler) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	default:
	}

	timer := time.NewTimer(h.opts.QueueTimeout)
	defer timer.Stop()
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrOverloaded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enforce applies verdict to msg and reports the outcome. The Discord calls
// and the report each get their own EnforcementTimeout, so a failed or slow
// enforcement still leaves time to tell moderators about it.
//
// Parameters:
//   - ctx (context.Context): parent context, not bound by the evaluation
//     deadline.
//   - msg (Message): offending message.
//   - verdict (moderation.Verdict): decision to apply; Action is not None.
func (h *Handler) enforce(ctx context.Context, msg Message, verdict moderation.Verdict) error {
	reason := fmt.Sprintf("shugo: %s (risk %.0f%%)", verdict.Category, verdict.Risk*100)

	var errs []error
	if !h.opts.DryRun {
		actCtx, cancel := context.WithTimeout(ctx, h.opts.EnforcementTimeout)
		if verdict.Action >= moderation.ActionDelete {
			if err := h.discord.DeleteMessage(actCtx, msg.ChannelID, msg.ID, reason); err != nil {
				errs = append(errs, fmt.Errorf("delete message: %w", err))
			}
		}
		if verdict.Action >= moderation.ActionTimeout {
			until := h.now().Add(h.opts.TimeoutDuration)
			if err := h.discord.TimeoutMember(actCtx, msg.GuildID, msg.AuthorID, until, reason); err != nil {
				errs = append(errs, fmt.Errorf("timeout member: %w", err))
			}
		}
		cancel()
	}
	enforceErr := errors.Join(errs...)

	h.logger.LogAttrs(ctx, slog.LevelInfo, "moderation action",
		slog.String("guild_id", msg.GuildID),
		slog.String("channel_id", msg.ChannelID),
		slog.String("message_id", msg.ID),
		slog.String("author_id", msg.AuthorID),
		slog.String("action", verdict.Action.String()),
		slog.String("category", verdict.Category),
		slog.Float64("risk", verdict.Risk),
		slog.Bool("dry_run", h.opts.DryRun),
		slog.Any("error", enforceErr),
	)

	if h.opts.LogChannelID != "" {
		reportCtx, cancel := context.WithTimeout(ctx, h.opts.EnforcementTimeout)
		defer cancel()
		report := Report{Message: msg, Verdict: verdict, DryRun: h.opts.DryRun, Err: enforceErr}
		if err := h.discord.SendReport(reportCtx, h.opts.LogChannelID, report); err != nil {
			errs = append(errs, fmt.Errorf("send report: %w", err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("enforce on message %s: %w", msg.ID, err)
	}
	return nil
}
