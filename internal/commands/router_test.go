package commands

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// discardLogger drops every record.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// reply returns a Handler that always answers text.
//
// Parameters:
//   - text (string): the reply.
func reply(text string) Handler {
	return func(context.Context, Request) (string, error) { return text, nil }
}

// pingCommand is a command without subcommands that echoes its option.
var pingCommand = Command{
	Definition: &discordgo.ApplicationCommand{
		Name:        "ping",
		Description: "Ping",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "text", Description: "Text"},
		},
	},
	Handlers: map[string]Handler{
		"": func(_ context.Context, req Request) (string, error) { return "pong " + req.Option("text"), nil },
	},
}

// groupCommand has a top-level subcommand and a subcommand group.
//
// Parameters:
//   - handlers (map[string]Handler): handlers to attach.
func groupCommand(handlers map[string]Handler) Command {
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "group",
			Description: "Group",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "show", Description: "Show"},
				{
					Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "item", Description: "Item",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Add"},
						{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Remove"},
					},
				},
			},
		},
		Handlers: handlers,
	}
}

func TestNewRouterValidatesCommands(t *testing.T) {
	t.Parallel()

	complete := map[string]Handler{"show": reply("s"), "item add": reply("a"), "item remove": reply("r")}
	missing := map[string]Handler{"show": reply("s"), "item add": reply("a")}
	extra := map[string]Handler{"show": reply("s"), "item add": reply("a"), "item remove": reply("r"), "item edit": reply("e")}

	tests := []struct {
		name    string
		cmds    []Command
		wantErr string
	}{
		{"valid", []Command{pingCommand, groupCommand(complete)}, ""},
		{"duplicate name", []Command{pingCommand, pingCommand}, "duplicate"},
		{"missing handler", []Command{groupCommand(missing)}, `no handler for "item remove"`},
		{"handler without subcommand", []Command{groupCommand(extra)}, `unknown subcommand "item edit"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewRouter(discardLogger, tt.cmds...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewRouter() error = %v", err)
				}
				names := []string{}
				for _, d := range r.Definitions() {
					names = append(names, d.Name)
				}
				if !slices.Equal(names, []string{"ping", "group"}) {
					t.Errorf("Definitions() names = %v, want registration order", names)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewRouter() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDispatchRoutesBySubcommand(t *testing.T) {
	t.Parallel()

	r, err := NewRouter(discardLogger, pingCommand, groupCommand(map[string]Handler{
		"show": reply("show"), "item add": reply("add"), "item remove": reply("remove"),
	}))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	sub := func(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts}
	}
	group := func(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: opts}
	}
	text := &discordgo.ApplicationCommandInteractionDataOption{Name: "text", Type: discordgo.ApplicationCommandOptionString, Value: "hi"}

	tests := []struct {
		name string
		data discordgo.ApplicationCommandInteractionData
		want string
	}{
		{"no subcommands", discordgo.ApplicationCommandInteractionData{Name: "ping", Options: []*discordgo.ApplicationCommandInteractionDataOption{text}}, "pong hi"},
		{"subcommand", discordgo.ApplicationCommandInteractionData{Name: "group", Options: []*discordgo.ApplicationCommandInteractionDataOption{sub("show")}}, "show"},
		{"grouped subcommand", discordgo.ApplicationCommandInteractionData{Name: "group", Options: []*discordgo.ApplicationCommandInteractionDataOption{group("item", sub("remove"))}}, "remove"},
		{"stale subcommand", discordgo.ApplicationCommandInteractionData{Name: "group", Options: []*discordgo.ApplicationCommandInteractionDataOption{sub("gone")}}, "Unknown command"},
		{"unknown command", discordgo.ApplicationCommandInteractionData{Name: "nope"}, "Unknown command"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := r.Dispatch(t.Context(), "g", 0, tt.data)
			if err != nil {
				t.Fatalf("Dispatch() error = %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("Dispatch() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestRunHidesHandlerErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"failure", errors.New("disk I/O error"), "Something went wrong"},
		{"timeout", context.DeadlineExceeded, "took too long"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := pingCommand
			cmd.Handlers = map[string]Handler{"": func(context.Context, Request) (string, error) { return "", tt.err }}
			r, err := NewRouter(discardLogger, cmd)
			if err != nil {
				t.Fatalf("NewRouter() error = %v", err)
			}

			got := r.run(t.Context(), "g", 0, discordgo.ApplicationCommandInteractionData{Name: "ping"})
			if !strings.Contains(got, tt.want) || strings.Contains(got, "disk") {
				t.Errorf("run() = %q, want a generic reply containing %q", got, tt.want)
			}
		})
	}
}

func TestGuildOnly(t *testing.T) {
	t.Parallel()

	h := GuildOnly(reply("ok"))
	if got, _ := h(t.Context(), Request{}); !strings.Contains(got, "server") {
		t.Errorf("outside a guild = %q, want a refusal", got)
	}
	if got, _ := h(t.Context(), Request{GuildID: "g"}); got != "ok" {
		t.Errorf("inside a guild = %q, want the handler's reply", got)
	}
}
