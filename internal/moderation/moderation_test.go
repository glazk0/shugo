package moderation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/jev"
	"github.com/glazk0/shugo/internal/moderation"
)

// fakeEvaluator records the last request and returns a canned response.
type fakeEvaluator struct {
	resp *jev.Response
	err  error
	got  jev.Request
}

// Evaluate implements moderation.Evaluator.
//
// Parameters:
//   - _ (context.Context): unused.
//   - req (jev.Request): recorded for assertions.
func (f *fakeEvaluator) Evaluate(_ context.Context, req jev.Request) (*jev.Response, error) {
	f.got = req
	return f.resp, f.err
}

// response builds a well-formed Jev response.
//
// Parameters:
//   - category (string): chosen violation category.
//   - pNone (float64): probability of the "none" category.
//   - score (float64): severity score on the 0 (severe) to 3 (harmless) scale.
//   - suspicion (float64): P(suspicious author).
func response(category string, pNone, score, suspicion float64) *jev.Response {
	// The remaining mass goes to "spam" so it never overwrites "none".
	probabilities := map[string]float64{"none": pNone, "spam": 1 - pNone}
	return &jev.Response{
		Model: "jev-1.13.0",
		Answers: map[string]jev.Answer{
			"violation": {
				Type:          jev.TypeChoice,
				Choice:        category,
				Probabilities: probabilities,
			},
			"severity":          {Type: jev.TypeScore, Score: score},
			"suspicious_author": {Type: jev.TypeNoul, Noul: suspicion},
		},
		Usage: jev.Usage{InputTokens: 400, OutputTokens: 30},
	}
}

func TestPolicyDecide(t *testing.T) {
	t.Parallel()

	p := moderation.DefaultPolicy
	tests := []struct {
		name                      string
		risk, severity, suspicion float64
		want                      moderation.Action
	}{
		{"clean message", 0.1, 0, 0.1, moderation.ActionNone},
		{"suspicious author alone is not enough", 0.1, 0, 0.99, moderation.ActionNone},
		{"borderline is flagged", 0.6, 0.3, 0.2, moderation.ActionFlag},
		{"borderline from suspicious author is deleted", 0.6, 0.3, 0.9, moderation.ActionDelete},
		{"confident low severity is deleted", 0.9, 0.4, 0.2, moderation.ActionDelete},
		{"confident high severity times out", 0.9, 0.8, 0.2, moderation.ActionTimeout},
		{"confident from suspicious author times out", 0.9, 0.4, 0.85, moderation.ActionTimeout},
		{"thresholds are inclusive", 0.85, 0.75, 0, moderation.ActionTimeout},
		{"just below flag", 0.4999, 1, 1, moderation.ActionNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := p.Decide(tt.risk, tt.severity, tt.suspicion); got != tt.want {
				t.Errorf("Decide(%v, %v, %v) = %v, want %v", tt.risk, tt.severity, tt.suspicion, got, tt.want)
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  moderation.Policy
		wantErr bool
	}{
		{"default", moderation.DefaultPolicy, false},
		{"negative threshold", moderation.Policy{FlagRisk: -0.1, DeleteRisk: 0.9}, true},
		{"threshold above one", moderation.Policy{FlagRisk: 0.5, DeleteRisk: 1.1}, true},
		{"flag above delete", moderation.Policy{FlagRisk: 0.9, DeleteRisk: 0.5}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.policy.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	if _, err := moderation.New(nil, moderation.DefaultPolicy); err == nil {
		t.Error("New(nil evaluator) error = nil")
	}
	if _, err := moderation.New(&fakeEvaluator{}, moderation.Policy{FlagRisk: 2}); err == nil {
		t.Error("New(invalid policy) error = nil")
	}
}

func TestModerate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := moderation.Input{
		Message: moderation.Message{ChannelID: "c1", Content: "free nitro https://disc0rd.gift/claim", UserMentions: 0},
		Author: moderation.Author{
			CreatedAt: now.Add(-3 * time.Hour),
			JoinedAt:  now.Add(-2 * time.Minute),
		},
		History: []history.Entry{
			{ChannelID: "c2", Content: "free nitro https://disc0rd.gift/claim", At: now.Add(-30 * time.Second)},
		},
		Now: now,
	}

	tests := []struct {
		name         string
		resp         *jev.Response
		wantAction   moderation.Action
		wantCategory string
	}{
		{"scam from new account", response("scam", 0.02, 0.1, 0.95), moderation.ActionTimeout, "scam"},
		{"harmless", response("none", 0.97, 2.9, 0.1), moderation.ActionNone, "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEvaluator{resp: tt.resp}
			m, err := moderation.New(fake, moderation.DefaultPolicy)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			v, err := m.Moderate(t.Context(), in)
			if err != nil {
				t.Fatalf("Moderate() error = %v", err)
			}
			if v.Action != tt.wantAction || v.Category != tt.wantCategory {
				t.Errorf("Moderate() = %s/%s, want %s/%s", v.Action, v.Category, tt.wantAction, tt.wantCategory)
			}
			if v.Model != "jev-1.13.0" || v.Usage.InputTokens != 400 {
				t.Errorf("Moderate() did not carry model/usage: %+v", v)
			}

			for _, key := range []string{"violation", "severity", "suspicious_author"} {
				if _, ok := fake.got.Questions[key]; !ok {
					t.Errorf("request is missing question %q", key)
				}
			}
			if fake.got.State == nil {
				t.Error("request state is nil")
			}
		})
	}
}

func TestModerateNormalisesAnswers(t *testing.T) {
	t.Parallel()

	m, err := moderation.New(&fakeEvaluator{resp: response("spam", 0.25, 1.5, 0.3)}, moderation.DefaultPolicy)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	v, err := m.Moderate(t.Context(), moderation.Input{Now: time.Now()})
	if err != nil {
		t.Fatalf("Moderate() error = %v", err)
	}
	if v.Risk != 0.75 {
		t.Errorf("Risk = %v, want 0.75", v.Risk)
	}
	// Score 1.5 on a 0 (severe) .. 3 (harmless) scale is halfway.
	if v.Severity != 0.5 {
		t.Errorf("Severity = %v, want 0.5", v.Severity)
	}
	if v.Suspicion != 0.3 {
		t.Errorf("Suspicion = %v, want 0.3", v.Suspicion)
	}
}

func TestModerateRejectsMalformedResponses(t *testing.T) {
	t.Parallel()

	missing := response("spam", 0.1, 0, 0)
	delete(missing.Answers, "severity")

	wrongType := response("spam", 0.1, 0, 0)
	wrongType.Answers["suspicious_author"] = jev.Answer{Type: jev.TypeChoice}

	noNone := response("spam", 0.1, 0, 0)
	noNone.Answers["violation"] = jev.Answer{Type: jev.TypeChoice, Choice: "spam", Probabilities: map[string]float64{"spam": 1}}

	tests := []struct {
		name    string
		resp    *jev.Response
		wantMsg string
	}{
		{"missing answer", missing, `missing "severity"`},
		{"wrong type", wrongType, `"suspicious_author" answer has type "choice"`},
		{"missing none probability", noNone, `no "none" probability`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := moderation.New(&fakeEvaluator{resp: tt.resp}, moderation.DefaultPolicy)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, err = m.Moderate(t.Context(), moderation.Input{Now: time.Now()})
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Moderate() error = %v, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}

func TestModeratePropagatesEvaluatorError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	m, err := moderation.New(&fakeEvaluator{err: sentinel}, moderation.DefaultPolicy)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := m.Moderate(t.Context(), moderation.Input{Now: time.Now()}); !errors.Is(err, sentinel) {
		t.Errorf("Moderate() error = %v, want wrapped sentinel", err)
	}
}

func TestActionString(t *testing.T) {
	t.Parallel()

	for a, want := range map[moderation.Action]string{
		moderation.ActionNone:    "none",
		moderation.ActionFlag:    "flag",
		moderation.ActionDelete:  "delete",
		moderation.ActionTimeout: "timeout",
		moderation.Action(42):    "Action(42)",
	} {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", int(a), got, want)
		}
	}
}
