package moderation

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxContentRunes = 2000
	maxHistoryRunes = 300
)

var linkPattern = regexp.MustCompile(`(?i)\bhttps?://\S+|\bdiscord(?:\.gg|(?:app)?\.com/invite)/\S+`)

// state is the JSON document Jev evaluates. Field names are referenced from
// question instructions, so keep the two in sync.
type state struct {
	Message        messageState  `json:"message"`
	Author         authorState   `json:"author"`
	RecentMessages []recentState `json:"recent_messages"`
	Signals        signalsState  `json:"signals"`
}

type messageState struct {
	Content          string `json:"content"`
	Attachments      int    `json:"attachments"`
	UserMentions     int    `json:"user_mentions"`
	MentionsEveryone bool   `json:"mentions_everyone"`
}

type authorState struct {
	AccountAge    string `json:"account_age"`
	MembershipAge string `json:"membership_age"`
}

type recentState struct {
	Content string `json:"content"`
	Channel string `json:"channel"`
	SentAgo string `json:"sent"`
}

type signalsState struct {
	ContainsLinks           bool `json:"contains_links"`
	IdenticalRecentMessages int  `json:"identical_recent_messages"`
	ChannelsUsedRecently    int  `json:"channels_used_recently"`
	MessagesLastMinute      int  `json:"messages_last_minute"`
}

// buildState turns raw message context into the labelled state Jev sees.
// Following TypeSafe's guidance, numeric facts such as ages are passed as
// human-readable labels rather than raw timestamps.
//
// Parameters:
//   - in (Input): message, author and history to describe.
func buildState(in Input) state {
	current := normalize(in.Message.Content)
	channels := map[string]struct{}{in.Message.ChannelID: {}}

	s := state{
		Message: messageState{
			Content:          truncate(in.Message.Content, maxContentRunes),
			Attachments:      in.Message.Attachments,
			UserMentions:     in.Message.UserMentions,
			MentionsEveryone: in.Message.MentionsEveryone,
		},
		Author: authorState{
			AccountAge:    accountAgeLabel(in.Author.CreatedAt, in.Now),
			MembershipAge: membershipAgeLabel(in.Author.JoinedAt, in.Now),
		},
		RecentMessages: make([]recentState, 0, len(in.History)),
		Signals: signalsState{
			ContainsLinks:      linkPattern.MatchString(in.Message.Content),
			MessagesLastMinute: 1,
		},
	}

	for _, h := range in.History {
		channel := "other channel"
		if h.ChannelID == in.Message.ChannelID {
			channel = "same channel"
		}
		s.RecentMessages = append(s.RecentMessages, recentState{
			Content: truncate(h.Content, maxHistoryRunes),
			Channel: channel,
			SentAgo: humanDuration(in.Now.Sub(h.At)) + " ago",
		})

		channels[h.ChannelID] = struct{}{}
		if current != "" && normalize(h.Content) == current {
			s.Signals.IdenticalRecentMessages++
		}
		if in.Now.Sub(h.At) <= time.Minute {
			s.Signals.MessagesLastMinute++
		}
	}
	s.Signals.ChannelsUsedRecently = len(channels)

	return s
}

// accountAgeLabel describes how old a Discord account is.
//
// Parameters:
//   - created (time.Time): account creation time; zero means unknown.
//   - now (time.Time): reference time.
func accountAgeLabel(created, now time.Time) string {
	if created.IsZero() {
		return "unknown"
	}
	age := now.Sub(created)
	var bucket string
	switch {
	case age < 24*time.Hour:
		bucket = "brand new account"
	case age < 7*24*time.Hour:
		bucket = "new account"
	case age < 30*24*time.Hour:
		bucket = "recent account"
	default:
		bucket = "established account"
	}
	return fmt.Sprintf("%s (%s)", humanDuration(age), bucket)
}

// membershipAgeLabel describes how long ago a member joined the server.
//
// Parameters:
//   - joined (time.Time): server join time; zero means unknown.
//   - now (time.Time): reference time.
func membershipAgeLabel(joined, now time.Time) string {
	if joined.IsZero() {
		return "unknown"
	}
	age := now.Sub(joined)
	var bucket string
	switch {
	case age < 10*time.Minute:
		bucket = "just joined"
	case age < 24*time.Hour:
		bucket = "joined today"
	case age < 7*24*time.Hour:
		bucket = "joined this week"
	default:
		bucket = "established member"
	}
	return fmt.Sprintf("%s (%s)", humanDuration(age), bucket)
}

// humanDuration renders d in its largest sensible unit, e.g. "3 days".
//
// Parameters:
//   - d (time.Duration): duration to render; negatives are treated as zero.
func humanDuration(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return plural(int(max(d, 0)/time.Second), "second")
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < day:
		return plural(int(d/time.Hour), "hour")
	case d < 30*day:
		return plural(int(d/day), "day")
	case d < 365*day:
		return plural(int(d/(30*day)), "month")
	default:
		return plural(int(d/(365*day)), "year")
	}
}

// plural formats n with unit, pluralising the unit when n != 1.
//
// Parameters:
//   - n (int): quantity.
//   - unit (string): singular unit name.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// truncate shortens s to at most n runes, marking the cut with an ellipsis.
//
// Parameters:
//   - s (string): text to shorten.
//   - n (int): maximum number of runes, ellipsis included.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

// normalize folds case and whitespace so trivially varied copies of the same
// message compare equal.
//
// Parameters:
//   - s (string): message content.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
