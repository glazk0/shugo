package bot_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/bot"
	"github.com/glazk0/shugo/internal/guild"
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

type banCall struct {
	guildID, userID string
	purge           time.Duration
}

// fakeDiscord records enforcement calls and can be told to fail them.
type fakeDiscord struct {
	mu sync.Mutex
	// ctxErrs holds ctx.Err() as seen by each call, in call order.
	ctxErrs  []error
	deleted  []string
	timeouts []timeoutCall
	reports  []bot.Report
	// reportChannels holds the channel of each report, in call order.
	reportChannels []string
	deleteErr      error
	timeoutErr     error
	bans           []banCall
	// unbans holds the user ID of each unban, in call order.
	unbans   []string
	banErr   error
	unbanErr error
}

// DeleteMessage implements bot.Discord.
//
// Parameters:
//   - ctx (context.Context): its error is recorded.
//   - _ (string): channel ID, unused.
//   - messageID (string): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) DeleteMessage(ctx context.Context, _, messageID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.deleted = append(f.deleted, messageID)
	return f.deleteErr
}

// TimeoutMember implements bot.Discord.
//
// Parameters:
//   - ctx (context.Context): its error is recorded.
//   - guildID (string): recorded.
//   - userID (string): recorded.
//   - until (time.Time): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) TimeoutMember(ctx context.Context, guildID, userID string, until time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.timeouts = append(f.timeouts, timeoutCall{guildID, userID, until})
	return f.timeoutErr
}

// BanMember implements bot.Discord.
//
// Parameters:
//   - ctx (context.Context): its error is recorded.
//   - guildID (string): recorded.
//   - userID (string): recorded.
//   - purge (time.Duration): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) BanMember(ctx context.Context, guildID, userID string, purge time.Duration, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.bans = append(f.bans, banCall{guildID, userID, purge})
	return f.banErr
}

// UnbanMember implements bot.Discord.
//
// Parameters:
//   - ctx (context.Context): its error is recorded.
//   - _ (string): guild ID, unused.
//   - userID (string): recorded.
//   - _ (string): reason, unused.
func (f *fakeDiscord) UnbanMember(ctx context.Context, _, userID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.unbans = append(f.unbans, userID)
	return f.unbanErr
}

// SendReport implements bot.Discord.
//
// Parameters:
//   - ctx (context.Context): its error is recorded.
//   - channelID (string): recorded.
//   - r (bot.Report): recorded.
func (f *fakeDiscord) SendReport(ctx context.Context, channelID string, r bot.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.reports = append(f.reports, r)
	f.reportChannels = append(f.reportChannels, channelID)
	return nil
}

// fakeSettings serves fixed guild settings, or fails when err is set.
type fakeSettings struct {
	settings guild.Settings
	err      error
}

// Get implements bot.GuildSettings.
//
// Parameters:
//   - _ (context.Context): unused.
//   - _ (string): guild ID, unused.
func (f fakeSettings) Get(_ context.Context, _ string) (guild.Settings, error) {
	return f.settings, f.err
}

// defaultSettings reports flags and actions to separate channels.
var defaultSettings = fakeSettings{settings: guild.Settings{FlagChannelID: "flags", ActionChannelID: "actions"}}

var defaultOpts = bot.Options{
	TimeoutDuration:    10 * time.Minute,
	QueueTimeout:       time.Second,
	EvaluationTimeout:  time.Second,
	EnforcementTimeout: time.Second,
	MaxConcurrency:     4,
}

// setup builds a Handler around fresh fakes and defaultSettings.
//
// Parameters:
//   - verdict (moderation.Verdict): verdict returned by the fake moderator.
//   - opts (bot.Options): handler options.
func setup(verdict moderation.Verdict, opts bot.Options) (*bot.Handler, *fakeModerator, *fakeDiscord) {
	return setupWithSettings(verdict, opts, defaultSettings)
}

// setupWithSettings builds a Handler around fresh fakes and the given guild
// settings.
//
// Parameters:
//   - verdict (moderation.Verdict): verdict returned by the fake moderator.
//   - opts (bot.Options): handler options.
//   - settings (bot.GuildSettings): guild settings source.
func setupWithSettings(verdict moderation.Verdict, opts bot.Options, settings bot.GuildSettings) (*bot.Handler, *fakeModerator, *fakeDiscord) {
	mod := &fakeModerator{verdict: verdict}
	dc := &fakeDiscord{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := bot.NewHandler(mod, history.New(10, time.Hour), settings, dc, logger, opts)
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

func TestHandleSkipsGuildExemptions(t *testing.T) {
	t.Parallel()

	settings := fakeSettings{settings: guild.Settings{
		ActionChannelID:  "actions",
		ExemptChannelIDs: []string{"bots"},
		ExemptRoleIDs:    []string{"trusted"},
	}}
	tests := []struct {
		name   string
		mutate func(*bot.Message)
	}{
		{"exempt channel", func(m *bot.Message) { m.ChannelID = "bots" }},
		{"thread in exempt channel", func(m *bot.Message) { m.ChannelID, m.ParentChannelID = "thread", "bots" }},
		{"exempt role", func(m *bot.Message) { m.RoleIDs = []string{"member", "trusted"} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, mod, dc := setupWithSettings(moderation.Verdict{Action: moderation.ActionTimeout}, defaultOpts, settings)
			msg := message("1", "free nitro")
			tt.mutate(&msg)

			if err := h.Handle(t.Context(), msg); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if mod.calls() != 0 || len(dc.deleted)+len(dc.reports) != 0 {
				t.Errorf("moderated an exempt message: calls = %d, discord = %+v", mod.calls(), dc)
			}
		})
	}

	// Other channels and roles in the same guild are still moderated.
	h, mod, _ := setupWithSettings(moderation.Verdict{}, defaultOpts, settings)
	msg := message("2", "hello")
	msg.RoleIDs = []string{"member"}
	if err := h.Handle(t.Context(), msg); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if mod.calls() != 1 {
		t.Errorf("moderator called %d times, want 1", mod.calls())
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
		settings     fakeSettings
		wantDeleted  int
		wantTimeouts int
		wantReport   string
	}{
		{"none", moderation.ActionNone, defaultOpts, defaultSettings, 0, 0, ""},
		{"flag reports only", moderation.ActionFlag, defaultOpts, defaultSettings, 0, 0, "flags"},
		{"delete", moderation.ActionDelete, defaultOpts, defaultSettings, 1, 0, "actions"},
		{"timeout also deletes", moderation.ActionTimeout, defaultOpts, defaultSettings, 1, 1, "actions"},
		{"dry run only reports", moderation.ActionTimeout, withDryRun(defaultOpts), defaultSettings, 0, 0, "actions"},
		{"no log channel", moderation.ActionDelete, defaultOpts, fakeSettings{}, 1, 0, ""},
		{"flag falls back to action channel", moderation.ActionFlag, defaultOpts, fakeSettings{settings: guild.Settings{ActionChannelID: "actions"}}, 0, 0, "actions"},
		{"settings unavailable", moderation.ActionDelete, defaultOpts, fakeSettings{err: errors.New("disk I/O error")}, 1, 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, _, dc := setupWithSettings(moderation.Verdict{Action: tt.action, Category: "spam", Risk: 0.9}, tt.opts, tt.settings)
			start := time.Now()

			if err := h.Handle(t.Context(), message("42", "spam spam")); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}

			wantReports := 0
			if tt.wantReport != "" {
				wantReports = 1
			}
			if len(dc.deleted) != tt.wantDeleted || len(dc.timeouts) != tt.wantTimeouts || len(dc.reports) != wantReports {
				t.Fatalf("deleted/timeouts/reports = %d/%d/%d, want %d/%d/%d",
					len(dc.deleted), len(dc.timeouts), len(dc.reports),
					tt.wantDeleted, tt.wantTimeouts, wantReports)
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
			if wantReports > 0 {
				if dc.reportChannels[0] != tt.wantReport {
					t.Errorf("report sent to %q, want %q", dc.reportChannels[0], tt.wantReport)
				}
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
	h := bot.NewHandler(mod, history.New(10, time.Hour), defaultSettings, &fakeDiscord{}, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)

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

func TestHandleDropsMessagesAfterQueueTimeout(t *testing.T) {
	t.Parallel()

	opts := defaultOpts
	opts.MaxConcurrency = 1
	opts.QueueTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	mod := &blockingModerator{started: make(chan struct{}), release: block}
	h := bot.NewHandler(mod, history.New(10, time.Hour), defaultSettings, &fakeDiscord{}, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)

	done := make(chan error, 1)
	go func() { done <- h.Handle(context.Background(), message("1", "a")) }()
	<-mod.started

	if err := h.Handle(t.Context(), message("2", "b")); !errors.Is(err, bot.ErrOverloaded) {
		t.Errorf("Handle() error = %v, want ErrOverloaded", err)
	}

	close(block)
	if err := <-done; err != nil {
		t.Errorf("first Handle() error = %v", err)
	}
}

func TestHandleEnforcesAfterSlowEvaluation(t *testing.T) {
	t.Parallel()

	opts := defaultOpts
	opts.EvaluationTimeout = 20 * time.Millisecond
	// The verdict arrives just as the evaluation deadline expires.
	mod := &deadlineModerator{verdict: moderation.Verdict{Action: moderation.ActionTimeout, Category: "scam"}}
	dc := &fakeDiscord{}
	h := bot.NewHandler(mod, history.New(10, time.Hour), defaultSettings, dc, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)

	if err := h.Handle(t.Context(), message("1", "free nitro")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(dc.ctxErrs) != 3 {
		t.Fatalf("Discord calls = %d, want delete, timeout and report", len(dc.ctxErrs))
	}
	for i, err := range dc.ctxErrs {
		if err != nil {
			t.Errorf("Discord call %d ran with an expired context: %v", i, err)
		}
	}
}

func TestHandleSeesConcurrentMessagesFromSameAuthor(t *testing.T) {
	t.Parallel()

	const n = 8
	opts := defaultOpts
	opts.MaxConcurrency = n
	h, mod, _ := setup(moderation.Verdict{}, opts)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if err := h.Handle(t.Context(), message(fmt.Sprint(i), "free nitro")); err != nil {
				t.Errorf("Handle() error = %v", err)
			}
		})
	}
	wg.Wait()

	// Every message must see all those recorded before it, so the history
	// lengths are exactly 0, 1, …, n-1 in some order.
	lengths := make(map[int]bool, n)
	for _, in := range mod.inputs {
		lengths[len(in.History)] = true
	}
	if len(lengths) != n {
		t.Errorf("history lengths = %v, want each of 0..%d exactly once", lengths, n-1)
	}
}

// honeypotSettings makes "trap" the honeypot, purging 6 hours, and exempts
// the "trusted" role and the "bots" and "trap" channels. Exempting the
// honeypot itself checks that the honeypot wins.
var honeypotSettings = fakeSettings{settings: guild.Settings{
	FlagChannelID:     "flags",
	ActionChannelID:   "actions",
	ExemptChannelIDs:  []string{"bots", "trap"},
	ExemptRoleIDs:     []string{"trusted"},
	HoneypotChannelID: "trap",
	HoneypotPurge:     6 * time.Hour,
}}

func TestHandleSoftbansHoneypotPosters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*bot.Message)
	}{
		{"message", func(*bot.Message) {}},
		{"thread in the honeypot", func(m *bot.Message) { m.ChannelID, m.ParentChannelID = "thread", "trap" }},
		{"exempt role", func(m *bot.Message) { m.RoleIDs = []string{"trusted"} }},
		{"attachment only", func(m *bot.Message) { m.Content, m.Attachments = "", 1 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, mod, dc := setupWithSettings(moderation.Verdict{}, defaultOpts, honeypotSettings)
			msg := message("1", "hello")
			msg.ChannelID = "trap"
			tt.mutate(&msg)

			if err := h.Handle(t.Context(), msg); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if mod.calls() != 0 {
				t.Errorf("moderator called %d times, want the honeypot to skip Jev", mod.calls())
			}
			if len(dc.bans) != 1 || dc.bans[0] != (banCall{"g", "u", 6 * time.Hour}) {
				t.Fatalf("bans = %+v, want u banned from g purging 6h", dc.bans)
			}
			if len(dc.unbans) != 1 || dc.unbans[0] != "u" {
				t.Errorf("unbans = %v, want u unbanned", dc.unbans)
			}
			if len(dc.deleted)+len(dc.timeouts) != 0 {
				t.Errorf("deleted/timeouts = %v/%v, want the ban's purge only", dc.deleted, dc.timeouts)
			}
			if len(dc.reports) != 1 || dc.reportChannels[0] != "actions" {
				t.Fatalf("reports = %+v to %v, want one to the action channel", dc.reports, dc.reportChannels)
			}
			r := dc.reports[0]
			if r.Softban == nil || r.Softban.Purge != 6*time.Hour || r.Softban.StillBanned || r.Err != nil || r.Message.ID != "1" {
				t.Errorf("report = %+v, softban = %+v", r, r.Softban)
			}
		})
	}
}

func TestHandleHoneypotSkips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*bot.Message)
	}{
		{"moderator", func(m *bot.Message) { m.Exempt = true }},
		{"bot author", func(m *bot.Message) { m.AuthorIsBot = true }},
		{"edit", func(m *bot.Message) { m.Edited = true }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, mod, dc := setupWithSettings(moderation.Verdict{Action: moderation.ActionTimeout}, defaultOpts, honeypotSettings)
			msg := message("1", "hello")
			msg.ChannelID = "trap"
			tt.mutate(&msg)

			if err := h.Handle(t.Context(), msg); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if mod.calls()+len(dc.bans)+len(dc.reports) != 0 {
				t.Errorf("acted on a skipped message: calls = %d, discord = %+v", mod.calls(), dc)
			}
		})
	}

	// Outside the honeypot, messages go through regular moderation.
	h, mod, dc := setupWithSettings(moderation.Verdict{}, defaultOpts, honeypotSettings)
	if err := h.Handle(t.Context(), message("2", "hello")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if mod.calls() != 1 || len(dc.bans) != 0 {
		t.Errorf("calls = %d, bans = %+v; want regular moderation", mod.calls(), dc.bans)
	}
}

func TestHandleSoftbansOncePerBurst(t *testing.T) {
	t.Parallel()

	const n = 5
	h, _, dc := setupWithSettings(moderation.Verdict{}, defaultOpts, honeypotSettings)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			msg := message(fmt.Sprint(i), "join my server")
			msg.ChannelID = "trap"
			if err := h.Handle(t.Context(), msg); err != nil {
				t.Errorf("Handle() error = %v", err)
			}
		})
	}
	wg.Wait()

	if len(dc.bans) != 1 || len(dc.reports) != 1 {
		t.Errorf("bans/reports = %d/%d, want one softban for the burst", len(dc.bans), len(dc.reports))
	}

	// Another member in the same burst gets their own softban.
	other := message("x", "join my server")
	other.ChannelID, other.AuthorID = "trap", "u2"
	if err := h.Handle(t.Context(), other); err != nil {
		t.Fatalf("Handle(other) error = %v", err)
	}
	if len(dc.bans) != 2 || dc.bans[1].userID != "u2" {
		t.Errorf("bans = %+v, want u2 softbanned too", dc.bans)
	}
}

func TestHandleSoftbanFailures(t *testing.T) {
	t.Parallel()

	t.Run("dry run only reports", func(t *testing.T) {
		t.Parallel()

		h, _, dc := setupWithSettings(moderation.Verdict{}, withDryRun(defaultOpts), honeypotSettings)
		msg := message("1", "hello")
		msg.ChannelID = "trap"
		if err := h.Handle(t.Context(), msg); err != nil {
			t.Fatalf("Handle() error = %v", err)
		}
		if len(dc.bans)+len(dc.unbans) != 0 || len(dc.reports) != 1 || !dc.reports[0].DryRun {
			t.Errorf("bans/unbans/reports = %v/%v/%+v, want a dry-run report only", dc.bans, dc.unbans, dc.reports)
		}
	})

	t.Run("failed ban skips the unban", func(t *testing.T) {
		t.Parallel()

		h, _, dc := setupWithSettings(moderation.Verdict{}, defaultOpts, honeypotSettings)
		dc.banErr = errors.New("missing permissions")
		msg := message("1", "hello")
		msg.ChannelID = "trap"

		if err := h.Handle(t.Context(), msg); !errors.Is(err, dc.banErr) {
			t.Errorf("Handle() error = %v, want the ban error", err)
		}
		if len(dc.unbans) != 0 {
			t.Errorf("unbans = %v, want none after a failed ban", dc.unbans)
		}
		if len(dc.reports) != 1 || !errors.Is(dc.reports[0].Err, dc.banErr) || dc.reports[0].Softban.StillBanned {
			t.Errorf("reports = %+v, want the ban failure reported", dc.reports)
		}
	})

	t.Run("failed unban reports the member still banned", func(t *testing.T) {
		t.Parallel()

		h, _, dc := setupWithSettings(moderation.Verdict{}, defaultOpts, honeypotSettings)
		dc.unbanErr = errors.New("service unavailable")
		msg := message("1", "hello")
		msg.ChannelID = "trap"

		if err := h.Handle(t.Context(), msg); !errors.Is(err, dc.unbanErr) {
			t.Errorf("Handle() error = %v, want the unban error", err)
		}
		if len(dc.reports) != 1 || !dc.reports[0].Softban.StillBanned || !errors.Is(dc.reports[0].Err, dc.unbanErr) {
			t.Errorf("reports = %+v, want the member reported as still banned", dc.reports)
		}
	})
}

// deadlineModerator waits for its context to expire, then answers anyway,
// like a Jev response that lands right at the evaluation deadline.
type deadlineModerator struct {
	verdict moderation.Verdict
}

// Moderate implements bot.Moderator.
//
// Parameters:
//   - ctx (context.Context): waited on until done.
//   - _ (moderation.Input): unused.
func (d *deadlineModerator) Moderate(ctx context.Context, _ moderation.Input) (moderation.Verdict, error) {
	<-ctx.Done()
	return d.verdict, nil
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
