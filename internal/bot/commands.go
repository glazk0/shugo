package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/guild"
)

// commandTimeout bounds the database work behind a command. Discord drops an
// interaction that gets no response within three seconds.
const commandTimeout = 2 * time.Second

// SettingsEditor reads and changes guild settings. *guild.Store satisfies it.
type SettingsEditor interface {
	GuildSettings
	SetLogChannel(ctx context.Context, guildID string, kind guild.LogKind, channelID string) error
	AddExemption(ctx context.Context, guildID string, kind guild.ExemptionKind, targetID string) (bool, error)
	RemoveExemption(ctx context.Context, guildID string, kind guild.ExemptionKind, targetID string) (bool, error)
}

// manageGuild hides /config from members who cannot manage the server.
// Server admins can still grant it to other roles under Integrations.
var manageGuild int64 = discordgo.PermissionManageGuild

// logKindOption picks which report channel a log-channel command changes.
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

// Commands are the application commands the bot registers globally.
var Commands = []*discordgo.ApplicationCommand{{
	Name:                     "config",
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
}}

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

// OnInteractionCreate returns a discordgo handler that answers /config with
// an ephemeral reply, running each command through tracker so shutdown can
// wait for it.
//
// Parameters:
//   - ctx (context.Context): parent context for every command.
//   - editor (SettingsEditor): guild settings store.
//   - tracker (*Tracker): tracks in-flight commands.
//   - logger (*slog.Logger): receives command errors.
func OnInteractionCreate(ctx context.Context, editor SettingsEditor, tracker *Tracker, logger *slog.Logger) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		if data.Name != Commands[0].Name {
			return
		}

		tracker.Do(func() {
			cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
			reply, err := RunConfigCommand(cmdCtx, editor, i.GuildID, data)
			cancel()
			if err != nil {
				logger.LogAttrs(ctx, slog.LevelError, "run config command",
					slog.String("guild_id", i.GuildID),
					slog.Any("error", err))
				reply = "Shugo could not update the settings. Try again in a moment."
			}

			err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: reply,
					Flags:   discordgo.MessageFlagsEphemeral,
					// The reply mentions channels and roles to name them,
					// never to ping anyone.
					AllowedMentions: &discordgo.MessageAllowedMentions{},
				},
			}, discordgo.WithContext(ctx))
			if err != nil {
				logger.LogAttrs(ctx, slog.LevelError, "respond to config command",
					slog.String("guild_id", i.GuildID),
					slog.Any("error", err))
			}
		})
	}
}

// RunConfigCommand applies a /config invocation to guildID's settings and
// returns the reply to show the invoker. Errors are storage failures; invalid
// requests produce an explanatory reply instead.
//
// Parameters:
//   - ctx (context.Context): bounds the database work.
//   - editor (SettingsEditor): guild settings store.
//   - guildID (string): guild the command was used in; "" outside guilds.
//   - data (discordgo.ApplicationCommandInteractionData): the invocation.
func RunConfigCommand(ctx context.Context, editor SettingsEditor, guildID string, data discordgo.ApplicationCommandInteractionData) (string, error) {
	if guildID == "" {
		return "Use this command in a server.", nil
	}

	path, opts := subcommand(data.Options)
	switch path {
	case "show":
		settings, err := editor.Get(ctx, guildID)
		if err != nil {
			return "", err
		}
		return describe(settings), nil

	case "log-channel set", "log-channel clear":
		kind := guild.LogKind(option(opts, "kind"))
		channelID := option(opts, "channel")
		if path == "log-channel set" && channelID == "" {
			return "Pick a channel.", nil
		}
		if err := editor.SetLogChannel(ctx, guildID, kind, channelID); err != nil {
			return "", err
		}
		if channelID == "" {
			return fmt.Sprintf("%s no longer have their own channel.", reportName(kind)), nil
		}
		return fmt.Sprintf("%s now go to <#%s>. Make sure Shugo can send messages and embed links there.",
			reportName(kind), channelID), nil

	case "exempt-channel add", "exempt-channel remove":
		channelID := option(opts, "channel")
		return exempt(ctx, editor, guildID, guild.ExemptChannel, path == "exempt-channel add", channelID, "<#"+channelID+">")

	case "exempt-role add", "exempt-role remove":
		roleID := option(opts, "role")
		add := path == "exempt-role add"
		// The @everyone role shares the guild's ID; exempting it would
		// silently switch moderation off for the whole server.
		if add && roleID == guildID {
			return "Exempting @everyone would turn moderation off for every member. Exempt specific roles instead.", nil
		}
		return exempt(ctx, editor, guildID, guild.ExemptRole, add, roleID, "<@&"+roleID+">")

	default:
		return "Unknown command. Discord may still be showing an outdated version of /config.", nil
	}
}

// exempt adds or removes one exemption and describes the outcome.
//
// Parameters:
//   - ctx (context.Context): bounds the write.
//   - editor (SettingsEditor): guild settings store.
//   - guildID (string): guild to configure.
//   - kind (guild.ExemptionKind): channel or role.
//   - add (bool): true to add the exemption, false to remove it.
//   - targetID (string): channel or role ID.
//   - mention (string): targetID rendered as a mention for the reply.
func exempt(ctx context.Context, editor SettingsEditor, guildID string, kind guild.ExemptionKind, add bool, targetID, mention string) (string, error) {
	if targetID == "" {
		return "Pick a " + string(kind) + ".", nil
	}

	if add {
		added, err := editor.AddExemption(ctx, guildID, kind, targetID)
		if err != nil {
			return "", err
		}
		if !added {
			return mention + " is already exempt.", nil
		}
		return mention + " is now exempt from moderation.", nil
	}

	removed, err := editor.RemoveExemption(ctx, guildID, kind, targetID)
	if err != nil {
		return "", err
	}
	if !removed {
		return mention + " was not exempt.", nil
	}
	return mention + " is moderated again.", nil
}

// subcommand flattens the invoked subcommand into a space-separated path such
// as "log-channel set" and returns the options passed to it.
//
// Parameters:
//   - opts ([]*discordgo.ApplicationCommandInteractionDataOption): top-level
//     options of the invocation.
func subcommand(opts []*discordgo.ApplicationCommandInteractionDataOption) (string, []*discordgo.ApplicationCommandInteractionDataOption) {
	var path []string
	for len(opts) == 1 {
		o := opts[0]
		if o.Type != discordgo.ApplicationCommandOptionSubCommandGroup && o.Type != discordgo.ApplicationCommandOptionSubCommand {
			break
		}
		path = append(path, o.Name)
		opts = o.Options
	}
	return strings.Join(path, " "), opts
}

// option returns the value of the named option as a string, which is how
// Discord sends string, channel and role options, or "" when it is missing.
//
// Parameters:
//   - opts ([]*discordgo.ApplicationCommandInteractionDataOption): options
//     passed to a subcommand.
//   - name (string): option name.
func option(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			v, _ := o.Value.(string)
			return v
		}
	}
	return ""
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

// describe renders a guild's settings for /config show.
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
