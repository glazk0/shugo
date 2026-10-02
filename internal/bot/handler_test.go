package bot_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/bot"
	"github.com/glazk0/shugo/internal/history"
	"github.com/glazk0/shugo/internal/moderation"
)

// fakeModerator returns a fixed verdict and records every input.
type fakeModerator struct {
	mu      sync.Mutex
	verdict moderation.Verdict
	err     error
	inputs  []moderation.Input
}

// Moderate implements bot.Moderator.
//
// Parameters:
//   - _ (context.Context): unused.
//   - in (moderation.Input): recorded for assertions.
func (f *fakeModerator) Moderate(_ context.Context, in moderation.Input) (moderation.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	return f.verdict, f.err
}

// calls returns the number of Moderate calls so far.
func (f *fakeModerator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

type timeoutCall struct {
	guildID, userID string
	until           time.Time
}

// fakeDiscord records enforcement calls and can be told to fail them.
type fakeDiscord struct {
	mu         sync.Mutex
	deleted    []string
	timeouts   []timeoutCall
	reports    []bot.Report
	deleteErr  error
	timeoutErr error
}

// DeleteMessage implements bot.Discord.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): channel ID, unused.
//   - messageID (string): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) DeleteMessage(_ context.Context, _, messageID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return f.deleteErr
}

// TimeoutMember implements bot.Discord.
//
// Parameters:
//   - _ (context.Context): unused.
//   - guildID (string): recorded.
//   - userID (string): recorded.
//   - until (time.Time): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) TimeoutMember(_ context.Context, guildID, userID string, until time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeouts = append(f.timeouts, timeoutCall{guildID, userID, until})
	return f.timeoutErr
}

// SendReport implements bot.Discord.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): channel ID, unused.
//   - r (bot.Report): recorded.
func (f *fakeDiscord) SendReport(_ context.Context, _ string, r bot.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
	return nil
}

var defaultOpts = bot.Options{
	LogChannelID:      "log",
	TimeoutDuration:   10 * time.Minute,
	EvaluationTimeout: time.Second,
	MaxConcurrency:    4,
}

// setup builds a Handler around fresh fakes.
//
// Parameters:
//   - verdict (moderation.Verdict): verdict returned by the fake moderator.
//   - opts (bot.Options): handler options.
func setup(verdict moderation.Verdict, opts bot.Options) (*bot.Handler, *fakeModerator, *fakeDiscord) {
	mod := &fakeModerator{verdict: verdict}
	dc := &fakeDiscord{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := bot.NewHandler(mod, history.New(10, time.Hour), dc, logger, opts)
	return h, mod, dc
}

// message builds a guild message from a regular member.
//
// Parameters:
//   - id (string): message ID.
//   - content (string): message text.
func message(id, content string) bot.Message {
	return bot.Message{
		ID:              id,
		GuildID:         "g",
		ChannelID:       "c",
		AuthorID:        "u",
		Content:         content,
		SentAt:          time.Now(),
		AuthorCreatedAt: time.Now().Add(-time.Hour),
		JoinedAt:        time.Now().Add(-time.Minute),
	}
}

func TestHandleSkipsIgnoredMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*bot.Message)
	}{
		{"direct message", func(m *bot.Message) { m.GuildID = "" }},
		{"bot author", func(m *bot.Message) { m.AuthorIsBot = true }},
		{"exempt member", func(m *bot.Message) { m.Exempt = true }},
		{"blank content", func(m *bot.Message) { m.Content = "  \n" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, mod, dc := setup(moderation.Verdict{Action: moderation.ActionTimeout}, defaultOpts)
			msg := message("1", "hello")
			tt.mutate(&msg)

			if err := h.Handle(t.Context(), msg); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if mod.calls() != 0 {
				t.Errorf("moderator called %d times, want 0", mod.calls())
			}
			if len(dc.deleted)+len(dc.timeouts)+len(dc.reports) != 0 {
				t.Errorf("unexpected enforcement: %+v", dc)
			}
		})
	}
}

func TestHandlePassesHistoryAndContext(t *testing.T) {
	t.Parallel()

	h, mod, _ := setup(moderation.Verdict{Action: moderation.ActionNone}, defaultOpts)

	first := message("1", "first")
	first.Attachments = 2
	if err := h.Handle(t.Context(), first); err != nil {
		t.Fatalf("Handle(first) error = %v", err)
	}
	second := message("2", "second")
	second.UserMentions = 5
	second.MentionsEveryone = true
	if err := h.Handle(t.Context(), second); err != nil {
		t.Fatalf("Handle(second) error = %v", err)
	}

	if len(mod.inputs) != 2 {
		t.Fatalf("moderator called %d times, want 2", len(mod.inputs))
	}
	if got := mod.inputs[0]; len(got.History) != 0 || got.Message.Attachments != 2 {
		t.Errorf("first input = %+v", got)
	}

	got := mod.inputs[1]
	if len(got.History) != 1 || got.History[0].MessageID != "1" || got.History[0].Content != "first" {
		t.Errorf("second input history = %+v, want the first message", got.History)
	}
	if got.Message.Content != "second" || got.Message.UserMentions != 5 || !got.Message.MentionsEveryone {
		t.Errorf("second input message = %+v", got.Message)
	}
	if !got.Author.CreatedAt.Equal(second.AuthorCreatedAt) || !got.Author.JoinedAt.Equal(second.JoinedAt) {
		t.Errorf("second input author = %+v", got.Author)
	}
}

func TestHandleRecordsBlankMessagesInHistory(t *testing.T) {
	t.Parallel()

	h, mod, _ := setup(moderation.Verdict{}, defaultOpts)
	if err := h.Handle(t.Context(), message("1", "")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if err := h.Handle(t.Context(), message("2", "text")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if mod.calls() != 1 || len(mod.inputs[0].History) != 1 {
		t.Errorf("inputs = %+v, want one call with the blank message in history", mod.inputs)
	}
}

func TestHandleEnforcesVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		action       moderation.Action
		opts         bot.Options
		wantDeleted  int
		wantTimeouts int
		wantReports  int
	}{
		{"none", moderation.ActionNone, defaultOpts, 0, 0, 0},
		{"flag reports only", moderation.ActionFlag, defaultOpts, 0, 0, 1},
		{"delete", moderation.ActionDelete, defaultOpts, 1, 0, 1},
		{"timeout also deletes", moderation.ActionTimeout, defaultOpts, 1, 1, 1},
		{"dry run only reports", moderation.ActionTimeout, withDryRun(defaultOpts), 0, 0, 1},
		{"no log channel", moderation.ActionDelete, withoutLogChannel(defaultOpts), 1, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, _, dc := setup(moderation.Verdict{Action: tt.action, Category: "spam", Risk: 0.9}, tt.opts)
			start := time.Now()

			if err := h.Handle(t.Context(), message("42", "spam spam")); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}

			if len(dc.deleted) != tt.wantDeleted || len(dc.timeouts) != tt.wantTimeouts || len(dc.reports) != tt.wantReports {
				t.Fatalf("deleted/timeouts/reports = %d/%d/%d, want %d/%d/%d",
					len(dc.deleted), len(dc.timeouts), len(dc.reports),
					tt.wantDeleted, tt.wantTimeouts, tt.wantReports)
			}
			if tt.wantDeleted > 0 && dc.deleted[0] != "42" {
				t.Errorf("deleted %q, want 42", dc.deleted[0])
			}
			if tt.wantTimeouts > 0 {
				call := dc.timeouts[0]
				if call.guildID != "g" || call.userID != "u" {
					t.Errorf("timeout call = %+v", call)
				}
				if d := call.until.Sub(start); d < tt.opts.TimeoutDuration || d > tt.opts.TimeoutDuration+time.Minute {
					t.Errorf("timeout lasts %s, want about %s", d, tt.opts.TimeoutDuration)
				}
			}
			if tt.wantReports > 0 {
				r := dc.reports[0]
				if r.DryRun != tt.opts.DryRun || r.Verdict.Action != tt.action || r.Message.ID != "42" || r.Err != nil {
					t.Errorf("report = %+v", r)
				}
			}
		})
	}
}

func TestHandleReportsEnforcementFailures(t *testing.T) {
	t.Parallel()

	h, _, dc := setup(moderation.Verdict{Action: moderation.ActionTimeout}, defaultOpts)
	deleteErr := errors.New("missing permissions")
	timeoutErr := errors.New("member is admin")
	dc.deleteErr, dc.timeoutErr = deleteErr, timeoutErr

	err := h.Handle(t.Context(), message("1", "bad"))
	if !errors.Is(err, deleteErr) || !errors.Is(err, timeoutErr) {
		t.Errorf("Handle() error = %v, want both enforcement errors", err)
	}
	// A failed delete must not prevent the timeout or the report.
	if len(dc.timeouts) != 1 || len(dc.reports) != 1 {
		t.Fatalf("timeouts/reports = %d/%d, want 1/1", len(dc.timeouts), len(dc.reports))
	}
	if !errors.Is(dc.reports[0].Err, deleteErr) {
		t.Errorf("report error = %v, want delete error", dc.reports[0].Err)
	}
}

func TestHandleReturnsModeratorError(t *testing.T) {
	t.Parallel()

	h, mod, dc := setup(moderation.Verdict{}, defaultOpts)
	mod.err = errors.New("jev down")

	if err := h.Handle(t.Context(), message("1", "hi")); !errors.Is(err, mod.err) {
		t.Errorf("Handle() error = %v, want moderator error", err)
	}
	if len(dc.deleted)+len(dc.reports) != 0 {
		t.Error("enforced despite moderator error")
	}
}

func TestHandleRespectsCancelledContext(t *testing.T) {
	t.Parallel()

	opts := defaultOpts
	opts.MaxConcurrency = 1
	block := make(chan struct{})
	mod := &blockingModerator{started: make(chan struct{}), release: block}
	h := bot.NewHandler(mod, history.New(10, time.Hour), &fakeDiscord{}, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)

	// Occupy the only slot.
	done := make(chan error, 1)
	go func() { done <- h.Handle(context.Background(), message("1", "a")) }()
	<-mod.started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.Handle(ctx, message("2", "b")); !errors.Is(err, context.Canceled) {
		t.Errorf("Handle() error = %v, want context.Canceled", err)
	}

	close(block)
	if err := <-done; err != nil {
		t.Errorf("first Handle() error = %v", err)
	}
}

// blockingModerator blocks until release is closed.
type blockingModerator struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

// Moderate implements bot.Moderator.
//
// Parameters:
//   - ctx (context.Context): aborts the wait.
//   - _ (moderation.Input): unused.
func (b *blockingModerator) Moderate(ctx context.Context, _ moderation.Input) (moderation.Verdict, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return moderation.Verdict{}, nil
	case <-ctx.Done():
		return moderation.Verdict{}, ctx.Err()
	}
}

// withDryRun returns a copy of o with DryRun enabled.
//
// Parameters:
//   - o (bot.Options): base options.
func withDryRun(o bot.Options) bot.Options {
	o.DryRun = true
	return o
}

// withoutLogChannel returns a copy of o with reports disabled.
//
// Parameters:
//   - o (bot.Options): base options.
func withoutLogChannel(o bot.Options) bot.Options {
	o.LogChannelID = ""
	return o
}
