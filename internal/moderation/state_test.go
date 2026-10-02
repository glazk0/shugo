package moderation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/glazk0/shugo/internal/history"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestBuildState(t *testing.T) {
	t.Parallel()

	in := Input{
		Message: Message{
			ChannelID:        "c1",
			Content:          "Claim your FREE nitro: https://disc0rd.gift/x",
			Attachments:      1,
			UserMentions:     3,
			MentionsEveryone: true,
		},
		Author: Author{
			CreatedAt: now.Add(-5 * time.Hour),
			JoinedAt:  now.Add(-90 * time.Second),
		},
		History: []history.Entry{
			{ChannelID: "c2", Content: "claim your free   NITRO: https://disc0rd.gift/x", At: now.Add(-2 * time.Minute)},
			{ChannelID: "c3", Content: "hello", At: now.Add(-40 * time.Second)},
			{ChannelID: "c1", Content: "Claim your FREE nitro: https://disc0rd.gift/x", At: now.Add(-10 * time.Second)},
		},
		Now: now,
	}

	s := buildState(in)

	if s.Author.AccountAge != "5 hours (brand new account)" {
		t.Errorf("AccountAge = %q", s.Author.AccountAge)
	}
	if s.Author.MembershipAge != "1 minute (just joined)" {
		t.Errorf("MembershipAge = %q", s.Author.MembershipAge)
	}
	if !s.Signals.ContainsLinks {
		t.Error("ContainsLinks = false")
	}
	if s.Signals.IdenticalRecentMessages != 2 {
		t.Errorf("IdenticalRecentMessages = %d, want 2", s.Signals.IdenticalRecentMessages)
	}
	if s.Signals.ChannelsUsedRecently != 3 {
		t.Errorf("ChannelsUsedRecently = %d, want 3", s.Signals.ChannelsUsedRecently)
	}
	// The current message plus the two sent within the last minute.
	if s.Signals.MessagesLastMinute != 3 {
		t.Errorf("MessagesLastMinute = %d, want 3", s.Signals.MessagesLastMinute)
	}
	if len(s.RecentMessages) != 3 {
		t.Fatalf("RecentMessages has %d entries, want 3", len(s.RecentMessages))
	}
	if got := s.RecentMessages[0]; got.Channel != "other channel" || got.SentAgo != "2 minutes ago" {
		t.Errorf("RecentMessages[0] = %+v", got)
	}
	if got := s.RecentMessages[2]; got.Channel != "same channel" || got.SentAgo != "10 seconds ago" {
		t.Errorf("RecentMessages[2] = %+v", got)
	}
	if s.Message.Attachments != 1 || s.Message.UserMentions != 3 || !s.Message.MentionsEveryone {
		t.Errorf("Message = %+v", s.Message)
	}
}

func TestBuildStateJSONMatchesQuestionFieldPaths(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(buildState(Input{Now: now}))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	// Question instructions reference these paths; renaming a JSON tag must
	// be mirrored there.
	for _, path := range []string{`"message"`, `"content"`, `"author"`, `"account_age"`, `"membership_age"`, `"recent_messages":[]`, `"signals"`} {
		if !strings.Contains(string(raw), path) {
			t.Errorf("state JSON %s is missing %s", raw, path)
		}
	}
}

func TestBuildStateTruncatesLongContent(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", maxContentRunes+50)
	s := buildState(Input{
		Message: Message{Content: long},
		History: []history.Entry{{Content: long, At: now}},
		Now:     now,
	})

	if n := utf8.RuneCountInString(s.Message.Content); n != maxContentRunes {
		t.Errorf("message content has %d runes, want %d", n, maxContentRunes)
	}
	if n := utf8.RuneCountInString(s.RecentMessages[0].Content); n != maxHistoryRunes {
		t.Errorf("history content has %d runes, want %d", n, maxHistoryRunes)
	}
	if !strings.HasSuffix(s.Message.Content, "…") {
		t.Error("truncated content has no ellipsis")
	}
}

func TestAgeLabels(t *testing.T) {
	t.Parallel()

	const day = 24 * time.Hour
	tests := []struct {
		name  string
		label func(time.Time, time.Time) string
		age   time.Duration
		want  string
	}{
		{"account brand new", accountAgeLabel, 30 * time.Minute, "30 minutes (brand new account)"},
		{"account new", accountAgeLabel, 3 * day, "3 days (new account)"},
		{"account recent", accountAgeLabel, 20 * day, "20 days (recent account)"},
		{"account established", accountAgeLabel, 800 * day, "2 years (established account)"},
		{"member just joined", membershipAgeLabel, 5 * time.Second, "5 seconds (just joined)"},
		{"member today", membershipAgeLabel, 3 * time.Hour, "3 hours (joined today)"},
		{"member this week", membershipAgeLabel, 2 * day, "2 days (joined this week)"},
		{"member established", membershipAgeLabel, 90 * day, "3 months (established member)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.label(now.Add(-tt.age), now); got != tt.want {
				t.Errorf("label = %q, want %q", got, tt.want)
			}
		})
	}

	if got := accountAgeLabel(time.Time{}, now); got != "unknown" {
		t.Errorf("accountAgeLabel(zero) = %q, want unknown", got)
	}
	if got := membershipAgeLabel(time.Time{}, now); got != "unknown" {
		t.Errorf("membershipAgeLabel(zero) = %q, want unknown", got)
	}
}

func TestHumanDurationClampsNegative(t *testing.T) {
	t.Parallel()

	if got := humanDuration(-time.Minute); got != "0 seconds" {
		t.Errorf("humanDuration(-1m) = %q, want 0 seconds", got)
	}
}
