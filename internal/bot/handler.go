// Package bot connects the moderator to Discord: it turns gateway events into
// moderation inputs, keeps per-member history and enforces verdicts.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/glazk0/shugo/internal/guild"
	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/moderation"
)

// ErrOverloaded is returned when a message waited longer than
// Options.QueueTimeout for an evaluation slot and was dropped.
var ErrOverloaded = errors.New("bot: moderation queue is full")

// softbanCooldown is how long a softban covers its member's later messages.
// A raider often posts a burst into the honeypot, and messages sent before
// the ban still reach the bot afterwards; one softban and one report cover
// them all.
const softbanCooldown = time.Minute

// Message is a guild message reduced to what moderation needs, independent
// of the Discord library.
type Message struct {
	ID        string
	GuildID   string
	ChannelID string
	// ParentChannelID is the channel a thread belongs to; empty outside
	// threads.
	ParentChannelID string
	AuthorID        string
	// RoleIDs are the author's roles in the guild; nil if unknown.
	RoleIDs []string
	Content string
	SentAt  time.Time

	Attachments      int
	UserMentions     int
	MentionsEveryone bool

	AuthorCreatedAt time.Time
	// JoinedAt is when the author joined the guild; zero if unknown.
	JoinedAt time.Time
	// AuthorIsBot is true for bot and webhook authors.
	AuthorIsBot bool
	// Exempt is true for authors who moderate the channel themselves.
	Exempt bool
	// Edited is true when the message is an edit of an earlier one.
	Edited bool
}

// Report describes an enforcement for the moderation log.
type Report struct {
	Message Message
	Verdict moderation.Verdict
	DryRun  bool
	Err     error
	// Softban is set when the message was posted in the guild's honeypot,
	// which skips Jev; Verdict is then empty.
	Softban *Softban
}

// Softban describes a honeypot softban: a ban that purges the member's
// recent messages, then an unban so the account can rejoin.
type Softban struct {
	// Purge is how far back the ban deleted the member's messages.
	Purge time.Duration
	// StillBanned is true when the ban went through but the unban failed.
	StillBanned bool
}

// Discord performs the REST calls needed to enforce verdicts.
type Discord interface {
	DeleteMessage(ctx context.Context, channelID, messageID, reason string) error
	TimeoutMember(ctx context.Context, guildID, userID string, until time.Time, reason string) error
	BanMember(ctx context.Context, guildID, userID string, purge time.Duration, reason string) error
	UnbanMember(ctx context.Context, guildID, userID, reason string) error
	SendReport(ctx context.Context, channelID string, r Report) error
}

// Moderator judges a message. *moderation.Moderator satisfies it.
type Moderator interface {
	Moderate(ctx context.Context, in moderation.Input) (moderation.Verdict, error)
}

// GuildSettings looks up the configuration a guild chose for itself.
// *guild.Store satisfies it.
type GuildSettings interface {
	Get(ctx context.Context, guildID string) (guild.Settings, error)
}

// Options tunes the Handler.
type Options struct {
	DryRun          bool
	TimeoutDuration time.Duration
	// QueueTimeout bounds how long a message waits for an evaluation slot.
	QueueTimeout time.Duration
	// EvaluationTimeout bounds the Jev evaluation of one message.
	EvaluationTimeout time.Duration
	// EnforcementTimeout bounds the delete and timeout calls for one verdict,
	// and separately the report.
	EnforcementTimeout time.Duration
	MaxConcurrency     int
}

// Handler moderates incoming messages. It is safe for concurrent use.
type Handler struct {
	moderator Moderator
	history   *history.Store
	settings  GuildSettings
	discord   Discord
	logger    *slog.Logger
	opts      Options
	sem       chan struct{}
	now       func() time.Time

	// softbans maps "guildID/userID" to when that member's last honeypot
	// softban started.
	mu       sync.Mutex
	softbans map[string]time.Time
}

// NewHandler wires a Handler together.
//
// Parameters:
//   - moderator (Moderator): judges each message.
//   - store (*history.Store): per-member message history.
//   - settings (GuildSettings): per-guild report channels and exemptions.
//   - discord (Discord): enforces verdicts.
//   - logger (*slog.Logger): structured logger.
//   - opts (Options): behaviour settings.
func NewHandler(moderator Moderator, store *history.Store, settings GuildSettings, discord Discord, logger *slog.Logger, opts Options) *Handler {
	return &Handler{
		moderator: moderator,
		history:   store,
		settings:  settings,
		discord:   discord,
		logger:    logger,
		opts:      opts,
		sem:       make(chan struct{}, max(opts.MaxConcurrency, 1)),
		now:       time.Now,
		softbans:  make(map[string]time.Time),
	}
}

// Handle moderates msg and enforces the resulting verdict. Messages from
// bots, outside guilds, from exempt members, or in channels and from roles the
// guild exempted are ignored. An edited message is handled like a new one,
// replacing its earlier version in the history. A message in the guild's
// honeypot skips Jev and softbans its author instead, even in an exempt
// channel or from an exempt role.
//
// Parameters:
//   - ctx (context.Context): cancels evaluation and enforcement.
//   - msg (Message): the message to moderate.
func (h *Handler) Handle(ctx context.Context, msg Message) error {
	if msg.GuildID == "" || msg.AuthorIsBot || msg.Exempt {
		return nil
	}

	settings, err := h.settings.Get(ctx, msg.GuildID)
	if err != nil {
		// Moderate with the defaults rather than letting every message
		// through while the database is unavailable.
		h.logger.LogAttrs(ctx, slog.LevelWarn, "load guild settings",
			slog.String("guild_id", msg.GuildID),
			slog.String("message_id", msg.ID),
			slog.Any("error", err))
	}
	if settings.IsHoneypot(msg.ChannelID) || settings.IsHoneypot(msg.ParentChannelID) {
		return h.softban(ctx, msg, settings)
	}
	if settings.ExemptsChannel(msg.ChannelID) || settings.ExemptsChannel(msg.ParentChannelID) ||
		settings.ExemptsAnyRole(msg.RoleIDs) {
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
	return h.enforce(ctx, msg, verdict, settings.ReportChannel(verdict.Action))
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
//   - reportChannelID (string): channel that receives the report; "" sends
//     none.
func (h *Handler) enforce(ctx context.Context, msg Message, verdict moderation.Verdict, reportChannelID string) error {
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

	report := Report{Message: msg, Verdict: verdict, DryRun: h.opts.DryRun, Err: enforceErr}
	if err := h.report(ctx, reportChannelID, report); err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("enforce on message %s: %w", msg.ID, err)
	}
	return nil
}

// softban bans the author of a honeypot message, purging their recent
// messages in every channel, then unbans them so the account can rejoin once
// it is back in safe hands. Edits are ignored, since the original post
// already triggered the softban, and so are later messages within
// softbanCooldown.
//
// Parameters:
//   - ctx (context.Context): parent context for the Discord calls.
//   - msg (Message): message posted in the honeypot.
//   - settings (guild.Settings): the guild's settings, holding the purge
//     window and report channels.
func (h *Handler) softban(ctx context.Context, msg Message, settings guild.Settings) error {
	if msg.Edited || !h.claimSoftban(msg.GuildID, msg.AuthorID) {
		return nil
	}

	const reason = "shugo: posted in the honeypot channel"
	outcome := &Softban{Purge: settings.HoneypotPurge}
	var errs []error
	if !h.opts.DryRun {
		actCtx, cancel := context.WithTimeout(ctx, h.opts.EnforcementTimeout)
		if err := h.discord.BanMember(actCtx, msg.GuildID, msg.AuthorID, outcome.Purge, reason); err != nil {
			errs = append(errs, fmt.Errorf("ban member: %w", err))
		} else if err := h.discord.UnbanMember(actCtx, msg.GuildID, msg.AuthorID, reason); err != nil {
			errs = append(errs, fmt.Errorf("unban member: %w", err))
			outcome.StillBanned = true
		}
		cancel()
	}
	enforceErr := errors.Join(errs...)

	h.logger.LogAttrs(ctx, slog.LevelInfo, "honeypot softban",
		slog.String("guild_id", msg.GuildID),
		slog.String("channel_id", msg.ChannelID),
		slog.String("message_id", msg.ID),
		slog.String("author_id", msg.AuthorID),
		slog.Duration("purge", outcome.Purge),
		slog.Bool("still_banned", outcome.StillBanned),
		slog.Bool("dry_run", h.opts.DryRun),
		slog.Any("error", enforceErr),
	)

	// A softban removes the member, so it is reported with the strongest
	// actions.
	report := Report{Message: msg, DryRun: h.opts.DryRun, Err: enforceErr, Softban: outcome}
	if err := h.report(ctx, settings.ReportChannel(moderation.ActionTimeout), report); err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("softban author of message %s: %w", msg.ID, err)
	}
	return nil
}

// claimSoftban reports whether the caller should softban a member, which is
// false while an earlier softban of theirs is within softbanCooldown. It
// also forgets softbans past the cooldown, so the map stays small.
//
// Parameters:
//   - guildID (string): guild the member posted in.
//   - userID (string): member to softban.
func (h *Handler) claimSoftban(guildID, userID string) bool {
	now := h.now()
	key := guildID + "/" + userID

	h.mu.Lock()
	defer h.mu.Unlock()
	if at, ok := h.softbans[key]; ok && now.Sub(at) < softbanCooldown {
		return false
	}
	for k, at := range h.softbans {
		if now.Sub(at) >= softbanCooldown {
			delete(h.softbans, k)
		}
	}
	h.softbans[key] = now
	return true
}

// report posts r to channelID under its own EnforcementTimeout.
//
// Parameters:
//   - ctx (context.Context): parent context, not bound by the enforcement
//     deadline.
//   - channelID (string): channel that receives the report; "" sends none.
//   - r (Report): enforcement to describe.
func (h *Handler) report(ctx context.Context, channelID string, r Report) error {
	if channelID == "" {
		return nil
	}
	reportCtx, cancel := context.WithTimeout(ctx, h.opts.EnforcementTimeout)
	defer cancel()
	if err := h.discord.SendReport(reportCtx, channelID, r); err != nil {
		return fmt.Errorf("send report: %w", err)
	}
	return nil
}
