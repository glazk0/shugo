// Package commands implements the bot's slash commands. Each command is a
// Command value built in its own file; the Router registers them with Discord
// and dispatches every invocation to the handler for its subcommand.
//
// To add a command, write a constructor returning a Command, then pass it to
// NewRouter in cmd/shugo.
package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// handlerTimeout bounds a handler. Discord drops an interaction that gets no
// response within three seconds.
const handlerTimeout = 2 * time.Second

// Request is one slash command invocation.
type Request struct {
	// GuildID is the guild the command was used in; "" outside guilds.
	GuildID string
	// Path is the invoked subcommand, such as "log-channel set"; "" for a
	// command without subcommands.
	Path    string
	options []*discordgo.ApplicationCommandInteractionDataOption
}

// Option returns the value of the named option, or "" when it was not
// passed. Discord sends string, channel, role, user and mentionable options
// as strings, the latter four as IDs.
//
// Parameters:
//   - name (string): option name.
func (r Request) Option(name string) string {
	for _, o := range r.options {
		if o.Name == name {
			v, _ := o.Value.(string)
			return v
		}
	}
	return ""
}

// Handler answers one subcommand with the reply to show the invoker. Errors
// are internal failures, which the Router logs and replaces with a generic
// reply; a handler explains invalid input in its reply instead.
type Handler func(ctx context.Context, req Request) (string, error)

// Command is one top-level slash command.
type Command struct {
	// Definition is what Discord shows and validates.
	Definition *discordgo.ApplicationCommand
	// Handlers maps every subcommand path in Definition, such as
	// "log-channel set", to its handler. A command without subcommands
	// uses the path "".
	Handlers map[string]Handler
}

// GuildOnly wraps h so it only runs inside a guild. Discord already hides
// guild-only commands elsewhere; this guards against stale definitions.
//
// Parameters:
//   - h (Handler): handler that needs a guild.
func GuildOnly(h Handler) Handler {
	return func(ctx context.Context, req Request) (string, error) {
		if req.GuildID == "" {
			return "Use this command in a server.", nil
		}
		return h(ctx, req)
	}
}

// Tracker runs work so shutdown can wait for it. *bot.Tracker satisfies it.
type Tracker interface {
	Do(f func()) bool
}

// Router dispatches interactions to registered commands.
type Router struct {
	commands map[string]Command
	order    []string
	logger   *slog.Logger
}

// NewRouter registers cmds. It fails when two commands share a name or when
// a command's handlers do not match its subcommands one to one, so a missing
// handler shows up at startup rather than when a member runs the command.
//
// Parameters:
//   - logger (*slog.Logger): receives handler and response errors.
//   - cmds (...Command): commands to serve.
func NewRouter(logger *slog.Logger, cmds ...Command) (*Router, error) {
	r := &Router{commands: make(map[string]Command, len(cmds)), logger: logger}
	for _, c := range cmds {
		name := c.Definition.Name
		if _, dup := r.commands[name]; dup {
			return nil, fmt.Errorf("commands: duplicate command %q", name)
		}

		paths := leafPaths(c.Definition.Options)
		for _, p := range paths {
			if c.Handlers[p] == nil {
				return nil, fmt.Errorf("commands: /%s has no handler for %q", name, p)
			}
		}
		for p := range c.Handlers {
			if !slices.Contains(paths, p) {
				return nil, fmt.Errorf("commands: /%s has a handler for unknown subcommand %q", name, p)
			}
		}

		r.commands[name] = c
		r.order = append(r.order, name)
	}
	return r, nil
}

// Definitions returns the definitions to register with Discord, in
// registration order.
func (r *Router) Definitions() []*discordgo.ApplicationCommand {
	defs := make([]*discordgo.ApplicationCommand, len(r.order))
	for i, name := range r.order {
		defs[i] = r.commands[name].Definition
	}
	return defs
}

// Dispatch runs the handler for an invocation and returns its reply.
//
// Parameters:
//   - ctx (context.Context): passed to the handler.
//   - guildID (string): guild the command was used in.
//   - data (discordgo.ApplicationCommandInteractionData): the invocation.
func (r *Router) Dispatch(ctx context.Context, guildID string, data discordgo.ApplicationCommandInteractionData) (string, error) {
	path, opts := subcommand(data.Options)
	handler := r.commands[data.Name].Handlers[path]
	if handler == nil {
		// Discord can briefly serve an outdated definition after an upgrade.
		return "Unknown command. Discord may still be showing an outdated version of it; try again in a minute.", nil
	}
	return handler(ctx, Request{GuildID: guildID, Path: path, options: opts})
}

// OnInteractionCreate returns a discordgo handler that answers every
// registered command with an ephemeral reply, running each one through
// tracker so shutdown can wait for it.
//
// Parameters:
//   - ctx (context.Context): parent context for every handler.
//   - tracker (Tracker): tracks in-flight commands.
func (r *Router) OnInteractionCreate(ctx context.Context, tracker Tracker) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		if _, ok := r.commands[data.Name]; !ok {
			return
		}

		tracker.Do(func() {
			reply := r.run(ctx, i.GuildID, data)
			err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: reply,
					Flags:   discordgo.MessageFlagsEphemeral,
					// Replies mention channels and roles to name them,
					// never to ping anyone.
					AllowedMentions: &discordgo.MessageAllowedMentions{},
				},
			}, discordgo.WithContext(ctx))
			if err != nil {
				r.logger.LogAttrs(ctx, slog.LevelError, "respond to command",
					slog.String("command", data.Name),
					slog.String("guild_id", i.GuildID),
					slog.Any("error", err))
			}
		})
	}
}

// run dispatches an invocation under handlerTimeout, turning a handler
// failure into a logged error and a generic reply.
//
// Parameters:
//   - ctx (context.Context): parent context.
//   - guildID (string): guild the command was used in.
//   - data (discordgo.ApplicationCommandInteractionData): the invocation.
func (r *Router) run(ctx context.Context, guildID string, data discordgo.ApplicationCommandInteractionData) string {
	runCtx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()

	reply, err := r.Dispatch(runCtx, guildID, data)
	if err == nil {
		return reply
	}
	r.logger.LogAttrs(ctx, slog.LevelError, "run command",
		slog.String("command", data.Name),
		slog.String("guild_id", guildID),
		slog.Any("error", err))
	if errors.Is(err, context.DeadlineExceeded) {
		return "That took too long. Try again in a moment."
	}
	return "Something went wrong. Try again in a moment."
}

// subcommand flattens the invoked subcommand into a space-separated path such
// as "log-channel set" and returns the options passed to it.
//
// Parameters:
//   - opts ([]*discordgo.ApplicationCommandInteractionDataOption): top-level
//     options of the invocation.
func subcommand(opts []*discordgo.ApplicationCommandInteractionDataOption) (string, []*discordgo.ApplicationCommandInteractionDataOption) {
	var path []string
	for len(opts) == 1 && isSubcommand(opts[0].Type) {
		path = append(path, opts[0].Name)
		opts = opts[0].Options
	}
	return strings.Join(path, " "), opts
}

// leafPaths lists the subcommand paths a definition accepts, or [""] when it
// has no subcommands.
//
// Parameters:
//   - opts ([]*discordgo.ApplicationCommandOption): top-level options of a
//     command definition.
func leafPaths(opts []*discordgo.ApplicationCommandOption) []string {
	var paths []string
	for _, o := range opts {
		switch o.Type {
		case discordgo.ApplicationCommandOptionSubCommand:
			paths = append(paths, o.Name)
		case discordgo.ApplicationCommandOptionSubCommandGroup:
			for _, sub := range o.Options {
				paths = append(paths, o.Name+" "+sub.Name)
			}
		}
	}
	if len(paths) == 0 {
		return []string{""}
	}
	return paths
}

// isSubcommand reports whether t is a subcommand or subcommand group.
//
// Parameters:
//   - t (discordgo.ApplicationCommandOptionType): option type.
func isSubcommand(t discordgo.ApplicationCommandOptionType) bool {
	return t == discordgo.ApplicationCommandOptionSubCommand || t == discordgo.ApplicationCommandOptionSubCommandGroup
}
