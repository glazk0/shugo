package bot

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/glazk0/shugo/internal/moderation"
)

// A real snowflake: 175928847299117063 was created on 2016-04-30 11:18:25.796 UTC.
const (
	authorID      = "175928847299117063"
	guildID       = "g"
	channelID     = "c"
	modRoleID     = "mod"
	everyoneRole  = guildID
	ownerID       = "owner"
	threadID      = "t"
	overwriteID   = "overwritten"
	authorCreated = 1462015105796
)

// testState returns a state cache holding one guild with a moderator role.
//
// Parameters:
//   - t (*testing.T): fails the test on setup errors.
func testState(t *testing.T) *discordgo.State {
	t.Helper()

	state := discordgo.NewState()
	err := state.GuildAdd(&discordgo.Guild{
		ID:      guildID,
		OwnerID: ownerID,
		Roles: []*discordgo.Role{
			{ID: everyoneRole, Permissions: discordgo.PermissionSendMessages},
			{ID: modRoleID, Permissions: discordgo.PermissionManageMessages},
		},
		Channels: []*discordgo.Channel{
			{ID: channelID, GuildID: guildID},
			{
				ID:      overwriteID,
				GuildID: guildID,
				PermissionOverwrites: []*discordgo.PermissionOverwrite{
					{ID: authorID, Type: discordgo.PermissionOverwriteTypeMember, Allow: discordgo.PermissionManageMessages},
				},
			},
		},
		Threads: []*discordgo.Channel{
			{ID: threadID, GuildID: guildID, ParentID: overwriteID, Type: discordgo.ChannelTypeGuildPublicThread},
		},
	})
	if err != nil {
		t.Fatalf("GuildAdd() error = %v", err)
	}
	return state
}

// gatewayMessage builds a MESSAGE_CREATE payload from a regular member.
func gatewayMessage() *discordgo.Message {
	return &discordgo.Message{
		ID:              "m",
		GuildID:         guildID,
		ChannelID:       channelID,
		Content:         "hello @everyone",
		Timestamp:       time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		Author:          &discordgo.User{ID: authorID},
		Member:          &discordgo.Member{JoinedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), Roles: []string{"member"}},
		Attachments:     []*discordgo.MessageAttachment{{}, {}},
		Mentions:        []*discordgo.User{{ID: "x"}},
		MentionEveryone: true,
	}
}

func TestFromDiscord(t *testing.T) {
	t.Parallel()

	msg, ok := FromDiscord(gatewayMessage())
	if !ok {
		t.Fatal("FromDiscord() ok = false")
	}

	want := Message{
		ID:               "m",
		GuildID:          guildID,
		ChannelID:        channelID,
		AuthorID:         authorID,
		RoleIDs:          []string{"member"},
		Content:          "hello @everyone",
		SentAt:           time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		Attachments:      2,
		UserMentions:     1,
		MentionsEveryone: true,
		AuthorCreatedAt:  time.UnixMilli(authorCreated).UTC(),
		JoinedAt:         time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	msg.AuthorCreatedAt = msg.AuthorCreatedAt.UTC()
	if !reflect.DeepEqual(msg, want) {
		t.Errorf("FromDiscord() =\n%+v\nwant\n%+v", msg, want)
	}
}

func TestParentChannel(t *testing.T) {
	t.Parallel()

	state := testState(t)
	tests := []struct {
		name      string
		state     *discordgo.State
		channelID string
		want      string
	}{
		{"thread", state, threadID, overwriteID},
		{"regular channel", state, channelID, ""},
		{"unknown channel", state, "missing", ""},
		{"no state", nil, threadID, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := parentChannel(tt.state, tt.channelID); got != tt.want {
				t.Errorf("parentChannel(%q) = %q, want %q", tt.channelID, got, tt.want)
			}
		})
	}
}

func TestFromDiscordBotAuthors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*discordgo.Message)
		wantBot bool
	}{
		{"regular member", func(*discordgo.Message) {}, false},
		{"bot author", func(m *discordgo.Message) { m.Author.Bot = true }, true},
		{"webhook", func(m *discordgo.Message) { m.WebhookID = "w" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := gatewayMessage()
			tt.mutate(m)
			msg, ok := FromDiscord(m)
			if !ok {
				t.Fatal("FromDiscord() ok = false")
			}
			if msg.AuthorIsBot != tt.wantBot {
				t.Errorf("AuthorIsBot = %v, want %v", msg.AuthorIsBot, tt.wantBot)
			}
		})
	}
}

func TestExempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*discordgo.Message)
		want   bool
	}{
		{"regular member", func(*discordgo.Message) {}, false},
		{"moderator role", func(m *discordgo.Message) { m.Member.Roles = []string{modRoleID} }, true},
		{"guild owner", func(m *discordgo.Message) { m.Author.ID = ownerID }, true},
		{"channel overwrite", func(m *discordgo.Message) { m.ChannelID = overwriteID }, true},
		{"thread inherits parent overwrite", func(m *discordgo.Message) { m.ChannelID = threadID }, true},
		{"uncached channel falls back to roles", func(m *discordgo.Message) {
			m.ChannelID = "uncached"
			m.Member.Roles = []string{modRoleID}
		}, true},
		{"uncached channel regular member", func(m *discordgo.Message) { m.ChannelID = "uncached" }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := gatewayMessage()
			tt.mutate(m)
			got, err := Exempt(testState(t), m)
			if err != nil {
				t.Fatalf("Exempt() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Exempt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExemptLooksUpMissingMember(t *testing.T) {
	t.Parallel()

	state := testState(t)
	if err := state.MemberAdd(&discordgo.Member{GuildID: guildID, User: &discordgo.User{ID: authorID}, Roles: []string{modRoleID}}); err != nil {
		t.Fatalf("MemberAdd() error = %v", err)
	}
	m := gatewayMessage()
	m.Member = nil

	if got, err := Exempt(state, m); err != nil || !got {
		t.Errorf("Exempt() = %v, %v; want true from the cached member", got, err)
	}
}

func TestExemptReportsUnknownGuild(t *testing.T) {
	t.Parallel()

	m := gatewayMessage()
	m.GuildID = "unknown"
	if got, err := Exempt(testState(t), m); err == nil || got {
		t.Errorf("Exempt() = %v, %v; want an error", got, err)
	}
}

func TestFromDiscordRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*discordgo.Message)
	}{
		{"direct message", func(m *discordgo.Message) { m.GuildID = "" }},
		{"no author", func(m *discordgo.Message) { m.Author = nil }},
		{"system message", func(m *discordgo.Message) { m.Type = discordgo.MessageTypeGuildMemberJoin }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := gatewayMessage()
			tt.mutate(m)
			if _, ok := FromDiscord(m); ok {
				t.Error("FromDiscord() ok = true, want false")
			}
		})
	}

	if _, ok := FromDiscord(nil); ok {
		t.Error("FromDiscord(nil) ok = true, want false")
	}
}

func TestExemptWithoutState(t *testing.T) {
	t.Parallel()

	m := gatewayMessage()
	m.Member.Roles = []string{modRoleID}
	if got, err := Exempt(nil, m); err != nil || got {
		t.Errorf("Exempt(nil state) = %v, %v; want not exempt", got, err)
	}
}

func TestReportEmbed(t *testing.T) {
	t.Parallel()

	base := Report{
		Message: Message{ID: "m", GuildID: guildID, ChannelID: channelID, AuthorID: "u", Content: "line one\nline two", SentAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		Verdict: moderation.Verdict{Category: "scam", Risk: 0.934, Severity: 0.8, Suspicion: 0.51, Model: "jev-1.13.0"},
	}

	tests := []struct {
		name      string
		action    moderation.Action
		dryRun    bool
		err       error
		wantTitle string
		wantField string
	}{
		{"flag", moderation.ActionFlag, false, nil, "Message flagged", "https://discord.com/channels/g/c/m"},
		{"delete", moderation.ActionDelete, false, nil, "Message deleted", "93%"},
		{"timeout", moderation.ActionTimeout, false, nil, "Member timed out", "<@u>"},
		{"dry run", moderation.ActionTimeout, true, nil, "[dry run] Would have: member timed out", "scam"},
		{"dry run flag stays a flag", moderation.ActionFlag, true, nil, "Message flagged", "<#c>"},
		{"failure", moderation.ActionDelete, false, errors.New("missing access"), "Message deleted", "missing access"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := base
			r.Verdict.Action = tt.action
			r.DryRun = tt.dryRun
			r.Err = tt.err
			e := reportEmbed(r)

			if e.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", e.Title, tt.wantTitle)
			}
			var values []string
			for _, f := range e.Fields {
				values = append(values, f.Value)
			}
			if joined := strings.Join(values, "|"); !strings.Contains(joined, tt.wantField) {
				t.Errorf("fields %q do not contain %q", joined, tt.wantField)
			}
			if e.Description != "> line one\n> line two" {
				t.Errorf("Description = %q", e.Description)
			}
			if e.Footer.Text != "Jev jev-1.13.0" || e.Timestamp != "2026-01-01T12:00:00Z" {
				t.Errorf("Footer/Timestamp = %q/%q", e.Footer.Text, e.Timestamp)
			}
		})
	}
}

func TestQuoteTruncatesLongContent(t *testing.T) {
	t.Parallel()

	got := quote(strings.Repeat("a", maxReportContent+10))
	if n := len([]rune(got)); n != maxReportContent+2 {
		t.Errorf("quote() has %d runes, want %d", n, maxReportContent+2)
	}
}
