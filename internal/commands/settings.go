package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/guild"
)

// SettingsStore reads and changes guild settings. *guild.Store satisfies it.
type SettingsStore interface {
	Get(ctx context.Context, guildID string) (guild.Settings, error)
	SetLogChannel(ctx context.Context, guildID string, kind guild.LogKind, channelID string) error
	AddExemption(ctx context.Context, guildID string, kind guild.ExemptionKind, targetID string) (bool, error)
	RemoveExemption(ctx context.Context, guildID string, kind guild.ExemptionKind, targetID string) (bool, error)
}

// manageGuild hides /settings from members who cannot manage the server.
// Server admins can still grant it to other roles under Integrations.
var manageGuild int64 = discordgo.PermissionManageGuild

// logKindOption picks which report channel a log-channel subcommand changes.
var logKindOption = &discordgo.ApplicationCommandOption{
	Type:        discordgo.ApplicationCommandOptionString,
	Name:        "kind",
	Description: "Which reports",
	Required:    true,
	Choices: []*discordgo.ApplicationCommandOptionChoice{
		{Name: "Flagged messages", Value: string(guild.LogFlags)},
		{Name: "Deletes and timeouts", Value: string(guild.LogActions)},
	},
}

// exemptChannelTypes are the channels members can post in, threads aside:
// a thread follows its parent channel's exemption.
var exemptChannelTypes = []discordgo.ChannelType{
	discordgo.ChannelTypeGuildText,
	discordgo.ChannelTypeGuildNews,
	discordgo.ChannelTypeGuildForum,
	discordgo.ChannelTypeGuildMedia,
	discordgo.ChannelTypeGuildVoice,
	discordgo.ChannelTypeGuildStageVoice,
}

// Settings returns the /settings command, which shows and edits a guild's
// report channels and exemptions.
//
// Parameters:
//   - store (SettingsStore): guild settings storage.
func Settings(store SettingsStore) Command {
	s := settingsCommand{store: store}
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:                     "settings",
			Description:              "Configure Shugo for this server",
			DefaultMemberPermissions: &manageGuild,
			Contexts:                 &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild},
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "show",
					Description: "Show this server's settings",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
					Name:        "log-channel",
					Description: "Choose where Shugo reports",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionSubCommand,
							Name:        "set",
							Description: "Send reports to a channel",
							Options: []*discordgo.ApplicationCommandOption{
								logKindOption,
								{
									Type:         discordgo.ApplicationCommandOptionChannel,
									Name:         "channel",
									Description:  "Channel that receives the reports",
									Required:     true,
									ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews},
								},
							},
						},
						{
							Type:        discordgo.ApplicationCommandOptionSubCommand,
							Name:        "clear",
							Description: "Stop sending reports to their own channel",
							Options:     []*discordgo.ApplicationCommandOption{logKindOption},
						},
					},
				},
				exemptGroup("exempt-channel", "channel", "Skip moderation in a channel and its threads", discordgo.ApplicationCommandOptionChannel),
				exemptGroup("exempt-role", "role", "Skip moderation for members with a role", discordgo.ApplicationCommandOptionRole),
			},
		},
		Handlers: map[string]Handler{
			"show":                  GuildOnly(s.show),
			"log-channel set":       GuildOnly(s.setLogChannel),
			"log-channel clear":     GuildOnly(s.clearLogChannel),
			"exempt-channel add":    GuildOnly(s.exemptChannel(true)),
			"exempt-channel remove": GuildOnly(s.exemptChannel(false)),
			"exempt-role add":       GuildOnly(s.exemptRole(true)),
			"exempt-role remove":    GuildOnly(s.exemptRole(false)),
		},
	}
}

// exemptGroup builds an add/remove subcommand group for one exemption kind.
//
// Parameters:
//   - name (string): group name.
//   - noun (string): option name and the thing being exempted.
//   - description (string): group description.
//   - optionType (discordgo.ApplicationCommandOptionType): channel or role.
func exemptGroup(name, noun, description string, optionType discordgo.ApplicationCommandOptionType) *discordgo.ApplicationCommandOption {
	target := func(verb string) []*discordgo.ApplicationCommandOption {
		opt := &discordgo.ApplicationCommandOption{
			Type:        optionType,
			Name:        noun,
			Description: fmt.Sprintf("The %s to %s", noun, verb),
			Required:    true,
		}
		if optionType == discordgo.ApplicationCommandOptionChannel {
			opt.ChannelTypes = exemptChannelTypes
		}
		return []*discordgo.ApplicationCommandOption{opt}
	}
	return &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
		Name:        name,
		Description: description,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Exempt a " + noun, Options: target("exempt")},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Stop exempting a " + noun, Options: target("stop exempting")},
		},
	}
}

// settingsCommand holds the /settings handlers.
type settingsCommand struct {
	store SettingsStore
}

// show renders the guild's settings.
//
// Parameters:
//   - ctx (context.Context): bounds the read.
//   - req (Request): the invocation.
func (c settingsCommand) show(ctx context.Context, req Request) (string, error) {
	settings, err := c.store.Get(ctx, req.GuildID)
	if err != nil {
		return "", err
	}
	return describe(settings), nil
}

// setLogChannel points one report channel at the chosen channel.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - req (Request): the invocation, with kind and channel options.
func (c settingsCommand) setLogChannel(ctx context.Context, req Request) (string, error) {
	kind, channelID := guild.LogKind(req.Option("kind")), req.Option("channel")
	if channelID == "" {
		return "Pick a channel.", nil
	}
	if err := c.store.SetLogChannel(ctx, req.GuildID, kind, channelID); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s now go to <#%s>. Make sure Shugo can send messages and embed links there.",
		reportName(kind), channelID), nil
}

// clearLogChannel removes one report channel.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - req (Request): the invocation, with a kind option.
func (c settingsCommand) clearLogChannel(ctx context.Context, req Request) (string, error) {
	kind := guild.LogKind(req.Option("kind"))
	if err := c.store.SetLogChannel(ctx, req.GuildID, kind, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s no longer have their own channel.", reportName(kind)), nil
}

// exemptChannel returns the handler that adds or removes a channel
// exemption.
//
// Parameters:
//   - add (bool): true to exempt the channel, false to stop exempting it.
func (c settingsCommand) exemptChannel(add bool) Handler {
	return func(ctx context.Context, req Request) (string, error) {
		channelID := req.Option("channel")
		return c.exempt(ctx, req.GuildID, guild.ExemptChannel, add, channelID, "<#"+channelID+">")
	}
}

// exemptRole returns the handler that adds or removes a role exemption.
//
// Parameters:
//   - add (bool): true to exempt the role, false to stop exempting it.
func (c settingsCommand) exemptRole(add bool) Handler {
	return func(ctx context.Context, req Request) (string, error) {
		roleID := req.Option("role")
		// The @everyone role shares the guild's ID; exempting it would
		// silently switch moderation off for the whole server.
		if add && roleID == req.GuildID {
			return "Exempting @everyone would turn moderation off for every member. Exempt specific roles instead.", nil
		}
		return c.exempt(ctx, req.GuildID, guild.ExemptRole, add, roleID, "<@&"+roleID+">")
	}
}

// exempt adds or removes one exemption and describes the outcome.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - guildID (string): guild to configure.
//   - kind (guild.ExemptionKind): channel or role.
//   - add (bool): true to add the exemption, false to remove it.
//   - targetID (string): channel or role ID.
//   - mention (string): targetID rendered as a mention for the reply.
func (c settingsCommand) exempt(ctx context.Context, guildID string, kind guild.ExemptionKind, add bool, targetID, mention string) (string, error) {
	if targetID == "" {
		return "Pick a " + string(kind) + ".", nil
	}

	if add {
		added, err := c.store.AddExemption(ctx, guildID, kind, targetID)
		if err != nil {
			return "", err
		}
		if !added {
			return mention + " is already exempt.", nil
		}
		return mention + " is now exempt from moderation.", nil
	}

	removed, err := c.store.RemoveExemption(ctx, guildID, kind, targetID)
	if err != nil {
		return "", err
	}
	if !removed {
		return mention + " was not exempt.", nil
	}
	return mention + " is moderated again.", nil
}

// reportName names the reports a log channel receives.
//
// Parameters:
//   - kind (guild.LogKind): report channel.
func reportName(kind guild.LogKind) string {
	if kind == guild.LogFlags {
		return "Reports of flagged messages"
	}
	return "Reports of deletes and timeouts"
}

// describe renders a guild's settings for /settings show.
//
// Parameters:
//   - s (guild.Settings): settings to render.
func describe(s guild.Settings) string {
	var b strings.Builder
	b.WriteString("**Flagged messages:** " + describeChannel(s.FlagChannelID, s.ActionChannelID, "deletes and timeouts") + "\n")
	b.WriteString("**Deletes and timeouts:** " + describeChannel(s.ActionChannelID, s.FlagChannelID, "flagged messages") + "\n")
	b.WriteString("**Exempt channels:** " + mentions(s.ExemptChannelIDs, "<#%s>") + "\n")
	b.WriteString("**Exempt roles:** " + mentions(s.ExemptRoleIDs, "<@&%s>"))
	return b.String()
}

// describeChannel renders one report channel, noting when it falls back to
// the other one.
//
// Parameters:
//   - own (string): the channel's own setting.
//   - other (string): the other report channel, used as the fallback.
//   - otherName (string): what the other channel receives.
func describeChannel(own, other, otherName string) string {
	switch {
	case own != "":
		return "<#" + own + ">"
	case other != "":
		return fmt.Sprintf("<#%s> (shared with %s)", other, otherName)
	default:
		return "not reported"
	}
}

// mentions renders ids with format, or "none".
//
// Parameters:
//   - ids ([]string): channel or role IDs.
//   - format (string): mention format with one %s verb.
func mentions(ids []string, format string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf(format, id)
	}
	return strings.Join(out, ", ")
}
