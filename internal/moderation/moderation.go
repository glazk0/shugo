// Package moderation decides what to do with a Discord message by asking
// Jev typed questions about the message, its author and their recent
// activity, then applying a deterministic threshold policy to the answers.
package moderation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/jev"
)

// Question keys used in the Jev request and response.
const (
	qViolation  = "violation"
	qSeverity   = "severity"
	qSuspicious = "suspicious_author"
)

// CategoryNone is the violation category for acceptable messages.
const CategoryNone = "none"

// categories are the violation options Jev chooses from.
var categories = map[string]string{
	CategoryNone:  "Normal conversation: questions, jokes, banter, mild profanity, disagreement or off-topic chat that targets no one",
	"spam":        "Flooding, repeated or low-effort messages, or the same text posted across messages or channels",
	"scam":        "Phishing, fake giveaways or free Nitro, crypto or investment schemes, links asking people to log in, verify or claim something",
	"advertising": "Unsolicited promotion of other Discord servers, products, services or social media accounts",
	"harassment":  "Insults, threats, bullying, doxxing or targeted attacks on a specific person",
	"hate":        "Slurs or attacks on people based on race, ethnicity, religion, gender, sexuality, disability or similar traits",
	"sexual":      "Sexually explicit or pornographic content",
}

// severityLevels are ordered worst to best, as the Score primitive expects.
var severityLevels = []string{
	"Severe: threats, slurs, scams or explicit content that must be removed immediately",
	"Moderate: clearly breaks common server rules",
	"Minor: rude, low quality or borderline",
	"Harmless: an acceptable message",
}

// questions is the fixed question set sent with every message.
var questions = map[string]jev.Question{
	qViolation: jev.Choice(
		"Which category best describes `message.content`? Use `recent_messages` and `signals` only as context for the current message.",
		categories,
	),
	qSeverity: jev.Score(
		"How harmful is `message.content` to the community?",
		severityLevels...,
	),
	qSuspicious: jev.Noul(
		"Judging by `author.account_age`, `author.membership_age`, `recent_messages` and `signals`, is the author behaving like a spam bot, raider or compromised account rather than a genuine community member?",
		"The author looks automated, malicious or compromised",
		"The author looks like a genuine member",
	),
}

// Evaluator evaluates a Jev request. *jev.Client satisfies it.
type Evaluator interface {
	Evaluate(ctx context.Context, req jev.Request) (*jev.Response, error)
}

// Message is the subset of a Discord message the moderator looks at.
type Message struct {
	ChannelID        string
	Content          string
	Attachments      int
	UserMentions     int
	MentionsEveryone bool
}

// Author describes who sent the message.
type Author struct {
	// CreatedAt is when the Discord account was created.
	CreatedAt time.Time
	// JoinedAt is when the author joined the guild; zero if unknown.
	JoinedAt time.Time
}

// Input is everything the moderator needs to judge one message.
type Input struct {
	Message Message
	Author  Author
	// History holds the author's previous messages in this guild, oldest
	// first, excluding Message itself.
	History []history.Entry
	// Now is the reference time used to compute ages.
	Now time.Time
}

// Action is the enforcement outcome for a message, in increasing order of
// severity.
type Action int

const (
	// ActionNone leaves the message alone.
	ActionNone Action = iota
	// ActionFlag reports the message to moderators without touching it.
	ActionFlag
	// ActionDelete deletes the message.
	ActionDelete
	// ActionTimeout deletes the message and times the author out.
	ActionTimeout
)

// String returns the lowercase name of the action.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionFlag:
		return "flag"
	case ActionDelete:
		return "delete"
	case ActionTimeout:
		return "timeout"
	default:
		return "Action(" + strconv.Itoa(int(a)) + ")"
	}
}

// Verdict is the moderator's decision together with the signals behind it.
type Verdict struct {
	Action Action
	// Category is the most likely violation category, CategoryNone included.
	Category string
	// Risk is the probability that the message violates any rule, i.e.
	// 1 - P(none).
	Risk float64
	// Severity is the expected harm normalised to [0, 1], 1 being severe.
	Severity float64
	// Suspicion is the probability that the author is a bot, raider or
	// compromised account.
	Suspicion float64
	// Model is the Jev model version that produced the answers.
	Model string
	Usage jev.Usage
}

// Policy maps Jev's answers to an Action. All values are in [0, 1].
type Policy struct {
	// FlagRisk is the risk at which a message is reported to moderators.
	FlagRisk float64
	// DeleteRisk is the risk at which a message is deleted.
	DeleteRisk float64
	// TimeoutSeverity is the severity at which a deleted message also
	// earns its author a timeout.
	TimeoutSeverity float64
	// Suspicion escalates enforcement by one step (flag to delete, delete to
	// timeout) when the author looks automated or compromised.
	Suspicion float64
}

// DefaultPolicy favours precision: it only deletes when Jev is confident.
var DefaultPolicy = Policy{
	FlagRisk:        0.5,
	DeleteRisk:      0.85,
	TimeoutSeverity: 0.75,
	Suspicion:       0.8,
}

// Validate reports whether the thresholds are in range and ordered.
func (p Policy) Validate() error {
	thresholds := []struct {
		name  string
		value float64
	}{
		{"flag risk", p.FlagRisk},
		{"delete risk", p.DeleteRisk},
		{"timeout severity", p.TimeoutSeverity},
		{"suspicion", p.Suspicion},
	}
	for _, t := range thresholds {
		if t.value < 0 || t.value > 1 {
			return fmt.Errorf("moderation: %s threshold %v is outside [0, 1]", t.name, t.value)
		}
	}
	if p.FlagRisk > p.DeleteRisk {
		return fmt.Errorf("moderation: flag risk %v exceeds delete risk %v", p.FlagRisk, p.DeleteRisk)
	}
	return nil
}

// Decide picks the action for a message with the given signals.
//
// Parameters:
//   - risk (float64): probability that the message breaks a rule.
//   - severity (float64): normalised expected harm.
//   - suspicion (float64): probability that the author is malicious.
func (p Policy) Decide(risk, severity, suspicion float64) Action {
	suspicious := suspicion >= p.Suspicion
	switch {
	case risk >= p.DeleteRisk && (severity >= p.TimeoutSeverity || suspicious):
		return ActionTimeout
	case risk >= p.DeleteRisk:
		return ActionDelete
	case risk >= p.FlagRisk && suspicious:
		return ActionDelete
	case risk >= p.FlagRisk:
		return ActionFlag
	default:
		return ActionNone
	}
}

// Moderator judges messages with Jev.
type Moderator struct {
	evaluator Evaluator
	policy    Policy
}

// New returns a Moderator.
//
// Parameters:
//   - evaluator (Evaluator): Jev client used to answer questions.
//   - policy (Policy): thresholds mapping answers to actions.
func New(evaluator Evaluator, policy Policy) (*Moderator, error) {
	if evaluator == nil {
		return nil, errors.New("moderation: nil evaluator")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Moderator{evaluator: evaluator, policy: policy}, nil
}

// Moderate asks Jev about the message and returns the resulting verdict.
//
// Parameters:
//   - ctx (context.Context): bounds the Jev call.
//   - in (Input): message, author and history to judge.
func (m *Moderator) Moderate(ctx context.Context, in Input) (Verdict, error) {
	resp, err := m.evaluator.Evaluate(ctx, jev.Request{
		State:     buildState(in),
		Questions: questions,
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("moderation: evaluate: %w", err)
	}

	v, err := readAnswers(resp)
	if err != nil {
		return Verdict{}, err
	}
	v.Action = m.policy.Decide(v.Risk, v.Severity, v.Suspicion)
	return v, nil
}

// readAnswers validates Jev's answers and converts them into a Verdict
// without an Action.
//
// Parameters:
//   - resp (*jev.Response): response to read.
func readAnswers(resp *jev.Response) (Verdict, error) {
	violation, err := answer(resp, qViolation, jev.TypeChoice)
	if err != nil {
		return Verdict{}, err
	}
	severity, err := answer(resp, qSeverity, jev.TypeScore)
	if err != nil {
		return Verdict{}, err
	}
	suspicious, err := answer(resp, qSuspicious, jev.TypeNoul)
	if err != nil {
		return Verdict{}, err
	}

	pNone, ok := violation.Probabilities[CategoryNone]
	if !ok {
		return Verdict{}, fmt.Errorf("moderation: %q answer has no %q probability", qViolation, CategoryNone)
	}

	// Score levels run worst (0) to best (n-1); flip so 1 means most severe.
	top := float64(len(severityLevels) - 1)

	return Verdict{
		Category:  violation.Choice,
		Risk:      clamp01(1 - pNone),
		Severity:  clamp01((top - severity.Score) / top),
		Suspicion: clamp01(suspicious.Noul),
		Model:     resp.Model,
		Usage:     resp.Usage,
	}, nil
}

// answer fetches the answer for key and checks its type.
//
// Parameters:
//   - resp (*jev.Response): response to read.
//   - key (string): question key.
//   - want (jev.QuestionType): expected answer type.
func answer(resp *jev.Response, key string, want jev.QuestionType) (jev.Answer, error) {
	a, ok := resp.Answers[key]
	if !ok {
		return jev.Answer{}, fmt.Errorf("moderation: missing %q answer", key)
	}
	if a.Type != want {
		return jev.Answer{}, fmt.Errorf("moderation: %q answer has type %q, want %q", key, a.Type, want)
	}
	return a, nil
}

// clamp01 bounds v to [0, 1].
//
// Parameters:
//   - v (float64): value to clamp.
func clamp01(v float64) float64 {
	return min(max(v, 0), 1)
}
