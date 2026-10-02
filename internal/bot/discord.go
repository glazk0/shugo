package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
// message with h, running each one through tracker so shutdown can wait for
// it.
//
// Parameters:
//   - ctx (context.Context): parent context for every evaluation. It should
//     outlive the shutdown signal so in-flight messages can finish.
//   - h (*Handler): handler to run.
//   - tracker (*Tracker): tracks in-flight messages.
//   - logger (*slog.Logger): receives handler errors.
func OnMessageCreate(ctx context.Context, h *Handler, tracker *Tracker, logger *slog.Logger) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		handleMessage(ctx, h, tracker, logger, s.State, m.Message)
	}
}

// OnMessageUpdate returns a discordgo handler that moderates every edited
// message with h, so harmless text cannot be edited into a violation after
// it was judged.
//
// Parameters:
//   - ctx (context.Context): parent context for every evaluation.
//   - h (*Handler): handler to run.
//   - tracker (*Tracker): tracks in-flight messages.
//   - logger (*slog.Logger): receives handler errors.
func OnMessageUpdate(ctx context.Context, h *Handler, tracker *Tracker, logger *slog.Logger) func(*discordgo.Session, *discordgo.MessageUpdate) {
	return func(s *discordgo.Session, m *discordgo.MessageUpdate) {
		// Discord also sends updates when it unfurls link embeds or when a
		// message is pinned; only an author's edit sets EditedTimestamp.
		if m.Message == nil || m.EditedTimestamp == nil {
			return
		}
		handleMessage(ctx, h, tracker, logger, s.State, m.Message)
	}
}

// handleMessage converts m and moderates it with h, logging any failure.
//
// Parameters:
//   - ctx (context.Context): parent context for the evaluation.
//   - h (*Handler): handler to run.
//   - tracker (*Tracker): tracks the run; nothing happens once it is closed.
//   - logger (*slog.Logger): receives handler errors.
//   - state (*discordgo.State): cache used to resolve exemptions.
//   - m (*discordgo.Message): message from a gateway event.
func handleMessage(ctx context.Context, h *Handler, tracker *Tracker, logger *slog.Logger, state *discordgo.State, m *discordgo.Message) {
	msg, ok := FromDiscord(m)
	if !ok {
		return
	}
	// discordgo already runs each handler on its own goroutine; the tracker
	// only exists for graceful shutdown.
	tracker.Do(func() {
		exempt, err := Exempt(state, m)
		if err != nil {
			// Treat the author as a regular member: skipping moderation
			// whenever the cache is cold would give spammers a way through.
			logger.LogAttrs(ctx, slog.LevelWarn, "resolve author permissions",
				slog.String("guild_id", msg.GuildID),
				slog.String("message_id", msg.ID),
				slog.Any("error", err))
		}
		msg.Exempt = exempt
		msg.ParentChannelID = parentChannel(state, msg.ChannelID)
		if err := h.Handle(ctx, msg); err != nil {
			logger.LogAttrs(ctx, slog.LevelError, "handle message",
				slog.String("guild_id", msg.GuildID),
				slog.String("message_id", msg.ID),
				slog.Any("error", err))
		}
	})
}

// FromDiscord converts a gateway message into a Message, leaving Exempt
// unset. It returns false for messages that should never be moderated: DMs,
// system messages and messages without an author.
//
// Parameters:
//   - m (*discordgo.Message): message from a MESSAGE_CREATE or MESSAGE_UPDATE
//     event.
func FromDiscord(m *discordgo.Message) (Message, bool) {
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
		msg.RoleIDs = m.Member.Roles
	}
	return msg, true
}

// parentChannel returns the parent of channelID when it is a thread, so a
// guild's channel exemption also covers the channel's threads. It returns ""
// for other channels and for channels missing from the state cache.
//
// Parameters:
//   - state (*discordgo.State): cache holding the guild's channels and
//     threads; nil resolves nothing.
//   - channelID (string): channel the message was sent in.
func parentChannel(state *discordgo.State, channelID string) string {
	if state == nil {
		return ""
	}
	channel, err := state.Channel(channelID)
	if err != nil || !channel.IsThread() {
		return ""
	}
	return channel.ParentID
}

// Exempt reports whether the author of m can moderate the channel the message
// was sent in. Threads take their permissions from their parent channel, as
// they do in Discord. When the channel is missing from the state cache, only
// guild-wide role permissions are considered, which still exempts server
// moderators.
//
// Parameters:
//   - state (*discordgo.State): cache holding the guild, its channels and
//     members, which the Guilds intent provides; nil exempts nobody.
//   - m (*discordgo.Message): message whose author to check.
func Exempt(state *discordgo.State, m *discordgo.Message) (bool, error) {
	if state == nil || m == nil || m.Author == nil {
		return false, nil
	}

	guild, err := state.Guild(m.GuildID)
	if err != nil {
		return false, fmt.Errorf("look up guild %s: %w", m.GuildID, err)
	}
	member := m.Member
	if member == nil {
		if member, err = state.Member(m.GuildID, m.Author.ID); err != nil {
			return false, fmt.Errorf("look up member %s: %w", m.Author.ID, err)
		}
	}

	channel, err := state.Channel(m.ChannelID)
	if err == nil && channel.IsThread() {
		channel, err = state.Channel(channel.ParentID)
	}
	switch {
	case errors.Is(err, discordgo.ErrStateNotFound):
		return guildPermissions(guild, m.Author.ID, member.Roles)&exemptPermissions != 0, nil
	case err != nil:
		return false, fmt.Errorf("look up channel %s: %w", m.ChannelID, err)
	}

	// MessagePermissions applies the overwrites of the message's channel, so
	// point a copy at the channel whose overwrites actually apply.
	resolved := *m
	resolved.ChannelID = channel.ID
	resolved.Member = member
	perms, err := state.MessagePermissions(&resolved)
	if err != nil {
		return false, fmt.Errorf("compute permissions in channel %s: %w", channel.ID, err)
	}
	return perms&exemptPermissions != 0, nil
}

// guildPermissions returns a member's guild-wide permissions, ignoring
// channel overwrites.
//
// Parameters:
//   - guild (*discordgo.Guild): guild with its roles.
//   - userID (string): member to compute permissions for.
//   - roles ([]string): IDs of the member's roles.
func guildPermissions(guild *discordgo.Guild, userID string, roles []string) int64 {
	if userID == guild.OwnerID {
		return discordgo.PermissionAll
	}
	var perms int64
	for _, role := range guild.Roles {
		// The @everyone role shares the guild's ID.
		if role.ID == guild.ID || slices.Contains(roles, role.ID) {
			perms |= role.Permissions
		}
	}
	return perms
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
