package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/moderation"
)

// Intents are the gateway intents the bot needs. MessageContent is
// privileged and must be enabled in the Discord developer portal.
const Intents = discordgo.IntentsGuilds |
	discordgo.IntentsGuildMessages |
	discordgo.IntentMessageContent

// exemptPermissions lets members who can already moderate a channel bypass
// automated moderation.
const exemptPermissions = discordgo.PermissionAdministrator | discordgo.PermissionManageMessages

const maxReportContent = 1000

// Session adapts a discordgo session to the Discord interface.
type Session struct {
	s *discordgo.Session
}

// NewSession wraps s.
//
// Parameters:
//   - s (*discordgo.Session): an authenticated session.
func NewSession(s *discordgo.Session) *Session {
	return &Session{s: s}
}

// DeleteMessage deletes a message, recording reason in the audit log.
//
// Parameters:
//   - ctx (context.Context): request context.
//   - channelID (string): channel containing the message.
//   - messageID (string): message to delete.
//   - reason (string): audit log reason.
func (d *Session) DeleteMessage(ctx context.Context, channelID, messageID, reason string) error {
	return d.s.ChannelMessageDelete(channelID, messageID,
		discordgo.WithContext(ctx), discordgo.WithAuditLogReason(reason))
}

// TimeoutMember times a member out until the given time.
//
// Parameters:
//   - ctx (context.Context): request context.
//   - guildID (string): guild the member belongs to.
//   - userID (string): member to time out.
//   - until (time.Time): when the timeout ends.
//   - reason (string): audit log reason.
func (d *Session) TimeoutMember(ctx context.Context, guildID, userID string, until time.Time, reason string) error {
	return d.s.GuildMemberTimeout(guildID, userID, &until,
		discordgo.WithContext(ctx), discordgo.WithAuditLogReason(reason))
}

// SendReport posts r as an embed in channelID.
//
// Parameters:
//   - ctx (context.Context): request context.
//   - channelID (string): moderation log channel.
//   - r (Report): enforcement to describe.
func (d *Session) SendReport(ctx context.Context, channelID string, r Report) error {
	_, err := d.s.ChannelMessageSendEmbed(channelID, reportEmbed(r), discordgo.WithContext(ctx))
	return err
}

// OnMessageCreate returns a discordgo handler that moderates every created
// message with h, tracking each run in wg so shutdown can wait for them.
//
// Parameters:
//   - ctx (context.Context): parent context for every evaluation.
//   - h (*Handler): handler to run.
//   - wg (*sync.WaitGroup): tracks in-flight evaluations.
//   - logger (*slog.Logger): receives handler errors.
func OnMessageCreate(ctx context.Context, h *Handler, wg *sync.WaitGroup, logger *slog.Logger) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if ctx.Err() != nil {
			return
		}
		msg, ok := FromDiscord(s.State, m.Message)
		if !ok {
			return
		}
		// discordgo already runs each handler on its own goroutine; the wait
		// group only exists for graceful shutdown.
		wg.Add(1)
		defer wg.Done()
		if err := h.Handle(ctx, msg); err != nil {
			logger.LogAttrs(ctx, slog.LevelError, "handle message",
				slog.String("guild_id", msg.GuildID),
				slog.String("message_id", msg.ID),
				slog.Any("error", err))
		}
	}
}

// FromDiscord converts a gateway message into a Message. It returns false
// for messages that should never be moderated: DMs, system messages and
// messages without an author.
//
// Parameters:
//   - state (*discordgo.State): cache used to resolve the author's
//     permissions; may be nil, in which case nobody is exempt.
//   - m (*discordgo.Message): message from a MESSAGE_CREATE event.
func FromDiscord(state *discordgo.State, m *discordgo.Message) (Message, bool) {
	if m == nil || m.Author == nil || m.GuildID == "" {
		return Message{}, false
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return Message{}, false
	}

	msg := Message{
		ID:               m.ID,
		GuildID:          m.GuildID,
		ChannelID:        m.ChannelID,
		AuthorID:         m.Author.ID,
		Content:          m.Content,
		SentAt:           m.Timestamp,
		Attachments:      len(m.Attachments),
		UserMentions:     len(m.Mentions),
		MentionsEveryone: m.MentionEveryone,
		AuthorIsBot:      m.Author.Bot || m.WebhookID != "",
	}
	if msg.SentAt.IsZero() {
		msg.SentAt = time.Now()
	}
	if created, err := discordgo.SnowflakeTimestamp(m.Author.ID); err == nil {
		msg.AuthorCreatedAt = created
	}
	if m.Member != nil {
		msg.JoinedAt = m.Member.JoinedAt
		if state != nil {
			// MessagePermissions needs the guild and channel in the state
			// cache, which the Guilds intent provides.
			if perms, err := state.MessagePermissions(m); err == nil {
				msg.Exempt = perms&exemptPermissions != 0
			}
		}
	}
	return msg, true
}

// reportEmbed renders r for the moderation log channel.
//
// Parameters:
//   - r (Report): enforcement to describe.
func reportEmbed(r Report) *discordgo.MessageEmbed {
	v := r.Verdict

	title, color := "Message flagged", 0xF1C40F
	switch v.Action {
	case moderation.ActionDelete:
		title, color = "Message deleted", 0xE67E22
	case moderation.ActionTimeout:
		title, color = "Member timed out", 0xE74C3C
	}
	if r.DryRun && v.Action > moderation.ActionFlag {
		title = "[dry run] Would have: " + strings.ToLower(title)
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Author", Value: fmt.Sprintf("<@%s>", r.Message.AuthorID), Inline: true},
		{Name: "Channel", Value: fmt.Sprintf("<#%s>", r.Message.ChannelID), Inline: true},
		{Name: "Category", Value: v.Category, Inline: true},
		{Name: "Risk", Value: percent(v.Risk), Inline: true},
		{Name: "Severity", Value: percent(v.Severity), Inline: true},
		{Name: "Suspicion", Value: percent(v.Suspicion), Inline: true},
	}
	if v.Action == moderation.ActionFlag {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Message",
			Value: fmt.Sprintf("https://discord.com/channels/%s/%s/%s", r.Message.GuildID, r.Message.ChannelID, r.Message.ID),
		})
	}
	if r.Err != nil {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Enforcement failed",
			Value: truncateRunes(r.Err.Error(), 1000),
		})
		color = 0x95A5A6
	}

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: quote(r.Message.Content),
		Color:       color,
		Fields:      fields,
		Timestamp:   r.Message.SentAt.UTC().Format(time.RFC3339),
		Footer:      &discordgo.MessageEmbedFooter{Text: "Jev " + v.Model},
	}
}

// percent formats a probability as a whole percentage.
//
// Parameters:
//   - p (float64): value in [0, 1].
func percent(p float64) string {
	return fmt.Sprintf("%.0f%%", p*100)
}

// quote renders content as a Markdown block quote. Mentions inside embeds
// never ping, so the content is shown as-is.
//
// Parameters:
//   - content (string): original message text.
func quote(content string) string {
	return "> " + strings.ReplaceAll(truncateRunes(content, maxReportContent), "\n", "\n> ")
}

// truncateRunes shortens s to at most n runes, ellipsis included.
//
// Parameters:
//   - s (string): text to shorten.
//   - n (int): maximum length in runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
