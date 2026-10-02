package bot

import (
	"errors"
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
		Channels: []*discordgo.Channel{{ID: channelID, GuildID: guildID}},
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
		Member:          &discordgo.Member{JoinedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		Attachments:     []*discordgo.MessageAttachment{{}, {}},
		Mentions:        []*discordgo.User{{ID: "x"}},
		MentionEveryone: true,
	}
}

func TestFromDiscord(t *testing.T) {
	t.Parallel()

	msg, ok := FromDiscord(testState(t), gatewayMessage())
	if !ok {
		t.Fatal("FromDiscord() ok = false")
	}

	want := Message{
		ID:               "m",
		GuildID:          guildID,
		ChannelID:        channelID,
		AuthorID:         authorID,
		Content:          "hello @everyone",
		SentAt:           time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		Attachments:      2,
		UserMentions:     1,
		MentionsEveryone: true,
		AuthorCreatedAt:  time.UnixMilli(authorCreated).UTC(),
		JoinedAt:         time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	msg.AuthorCreatedAt = msg.AuthorCreatedAt.UTC()
	if msg != want {
		t.Errorf("FromDiscord() =\n%+v\nwant\n%+v", msg, want)
	}
}

func TestFromDiscordExemptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*discordgo.Message)
		wantExempt bool
		wantBot    bool
	}{
		{"regular member", func(*discordgo.Message) {}, false, false},
		{"moderator role", func(m *discordgo.Message) { m.Member.Roles = []string{modRoleID} }, true, false},
		{"guild owner", func(m *discordgo.Message) { m.Author.ID = ownerID }, true, false},
		{"bot author", func(m *discordgo.Message) { m.Author.Bot = true }, false, true},
		{"webhook", func(m *discordgo.Message) { m.WebhookID = "w" }, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := gatewayMessage()
			tt.mutate(m)
			msg, ok := FromDiscord(testState(t), m)
			if !ok {
				t.Fatal("FromDiscord() ok = false")
			}
			if msg.Exempt != tt.wantExempt || msg.AuthorIsBot != tt.wantBot {
				t.Errorf("Exempt/AuthorIsBot = %v/%v, want %v/%v", msg.Exempt, msg.AuthorIsBot, tt.wantExempt, tt.wantBot)
			}
		})
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
			if _, ok := FromDiscord(nil, m); ok {
				t.Error("FromDiscord() ok = true, want false")
			}
		})
	}

	if _, ok := FromDiscord(nil, nil); ok {
		t.Error("FromDiscord(nil) ok = true, want false")
	}
}

func TestFromDiscordWithoutState(t *testing.T) {
	t.Parallel()

	m := gatewayMessage()
	m.Member.Roles = []string{modRoleID}
	msg, ok := FromDiscord(nil, m)
	if !ok || msg.Exempt {
		t.Errorf("FromDiscord(nil state) = %+v, %v; want not exempt", msg, ok)
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
