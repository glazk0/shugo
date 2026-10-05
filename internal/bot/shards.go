package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

const (
	// identifyInterval is how long Discord makes each identify bucket wait
	// between two session starts.
	identifyInterval = 5 * time.Second

	// minRetryDelay and maxRetryDelay bound the backoff between two attempts
	// to connect a shard.
	minRetryDelay = time.Second
	maxRetryDelay = time.Minute
)

// fatalCloseCodes are the gateway close codes that no retry can fix.
var fatalCloseCodes = map[int]string{
	4004: "authentication failed, check the bot token",
	4010: "invalid shard",
	4011: "sharding required, run more shards",
	4012: "invalid gateway version",
	4013: "invalid intents",
	4014: "disallowed intents, enable Message Content in the developer portal",
}

// ShardPlan is how many shards to run and how many of them may identify at
// once.
type ShardPlan struct {
	Count int
	// Recommended is the shard count Discord suggests for the bot's size.
	Recommended    int
	MaxConcurrency int
}

// PlanShards picks the shard count, falling back to Discord's recommendation
// when count is 0. It fails unless Discord's daily session start budget
// covers every shard twice. Going over the budget makes Discord reset the bot
// token, and the reserve leaves each shard room to identify again after
// losing its session.
//
// Parameters:
//   - gw (*discordgo.GatewayBotResponse): the GET /gateway/bot response.
//   - count (int): shards to run, or 0 for Discord's recommendation.
func PlanShards(gw *discordgo.GatewayBotResponse, count int) (ShardPlan, error) {
	plan := ShardPlan{
		Count:          count,
		Recommended:    max(gw.Shards, 1),
		MaxConcurrency: max(gw.SessionStartLimit.MaxConcurrency, 1),
	}
	if plan.Count == 0 {
		plan.Count = plan.Recommended
	}
	if limit := gw.SessionStartLimit; limit.Remaining < 2*plan.Count {
		reset := time.Duration(limit.ResetAfter) * time.Millisecond
		return ShardPlan{}, fmt.Errorf("discord allows %d more session starts until the limit resets in %s, fewer than twice the %d shards to start",
			limit.Remaining, reset.Round(time.Second), plan.Count)
	}
	return plan, nil
}

// Shards is one gateway session per shard. Every session shares a single
// REST rate limiter, since Discord's limits apply to the bot as a whole.
//
// Shards reconnects dropped sessions itself rather than leaving it to
// discordgo, so every reconnect waits for its identify bucket like the first
// connection did. discordgo resumes when it can, and when Discord refuses
// the resume, the identify that follows stays within the bucket's slot.
// The one identify outside the gate is discordgo's answer to an Invalid
// Session on a live connection, which it sends without telling handlers.
type Shards struct {
	sessions []*discordgo.Session
	gate     *identifyGate
	logger   *slog.Logger

	// open connects one shard; tests replace it.
	open               func(i int) error
	minRetry, maxRetry time.Duration

	// ctx ends when Close is called, stopping every reconnect.
	ctx          context.Context
	cancel       context.CancelFunc
	reconnecting []atomic.Bool
	failed       chan error
}

// NewShards asks Discord how to shard the bot and creates a session for each
// shard, without connecting them.
//
// Parameters:
//   - token (string): bot token, without the "Bot " prefix.
//   - count (int): shards to run, or 0 for Discord's recommendation.
//   - logger (*slog.Logger): receives shard warnings.
func NewShards(token string, count int, logger *slog.Logger) (*Shards, error) {
	first, err := newSession("Bot "+token, discordgo.NewRatelimiter())
	if err != nil {
		return nil, err
	}
	gateway, err := first.GatewayBot()
	if err != nil {
		return nil, fmt.Errorf("query discord gateway: %w", err)
	}
	plan, err := PlanShards(gateway, count)
	if err != nil {
		return nil, err
	}
	if plan.Count < plan.Recommended {
		logger.Warn("running fewer shards than Discord recommends",
			slog.Int("shards", plan.Count),
			slog.Int("recommended", plan.Recommended))
	}
	return newShards(first, plan, logger)
}

// newShards builds the shards for plan around first, which becomes shard 0
// and lends the others its token and rate limiter.
//
// Parameters:
//   - first (*discordgo.Session): session created by newSession.
//   - plan (ShardPlan): shards to create.
//   - logger (*slog.Logger): receives shard warnings.
func newShards(first *discordgo.Session, plan ShardPlan, logger *slog.Logger) (*Shards, error) {
	ctx, cancel := context.WithCancel(context.Background())
	sh := &Shards{
		sessions:     make([]*discordgo.Session, plan.Count),
		gate:         newIdentifyGate(plan.MaxConcurrency, identifyInterval),
		logger:       logger,
		minRetry:     minRetryDelay,
		maxRetry:     maxRetryDelay,
		ctx:          ctx,
		cancel:       cancel,
		reconnecting: make([]atomic.Bool, plan.Count),
		failed:       make(chan error, 1),
	}
	sh.open = func(i int) error { return sh.sessions[i].Open() }

	for i := range sh.sessions {
		s := first
		if i > 0 {
			var err error
			if s, err = newSession(first.Token, first.Ratelimiter); err != nil {
				cancel()
				return nil, err
			}
		}
		s.ShardID = i
		s.ShardCount = plan.Count
		s.AddHandler(func(*discordgo.Session, *discordgo.Disconnect) { sh.reconnect(i) })
		sh.sessions[i] = s
	}
	return sh, nil
}

// newSession creates a session that leaves reconnecting to Shards.
//
// Parameters:
//   - auth (string): authorization header value, "Bot " and the token.
//   - limiter (*discordgo.RateLimiter): REST rate limiter to share.
func newSession(auth string, limiter *discordgo.RateLimiter) (*discordgo.Session, error) {
	s, err := discordgo.New(auth)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	s.Identify.Intents = Intents
	s.Ratelimiter = limiter
	s.ShouldReconnectOnError = false
	return s, nil
}

// Session returns the session of shard 0. Its REST calls share the rate
// limiter of every shard, so it can act in any guild.
func (sh *Shards) Session() *discordgo.Session {
	return sh.sessions[0]
}

// AddHandler registers handler on every shard.
//
// Parameters:
//   - handler (any): a discordgo event handler.
func (sh *Shards) AddHandler(handler any) {
	for _, s := range sh.sessions {
		s.AddHandler(handler)
	}
}

// Failed receives an error when a shard that dropped can never reconnect,
// such as after Discord revokes the token.
func (sh *Shards) Failed() <-chan error {
	return sh.failed
}

// Open connects every shard, as fast as the identify buckets allow,
// retrying each one until it connects. It returns the first error that no
// retry can fix, or ctx's error when ctx ends first. Shards that connected
// stay open either way; call Close once Open returns to disconnect them.
//
// Parameters:
//   - ctx (context.Context): stops the startup.
func (sh *Shards) Open(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(sh.sessions))
	var wg sync.WaitGroup
	for i := range sh.sessions {
		wg.Go(func() {
			if errs[i] = sh.connect(ctx, i); errs[i] != nil {
				cancel()
			}
		})
	}
	wg.Wait()

	// Report the failure that stopped the startup rather than the
	// cancellations it caused in the other shards.
	var canceled error
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			canceled = err
		default:
			return err
		}
	}
	return canceled
}

// Close stops reconnecting and disconnects every shard at once, since
// discordgo pauses a second for each closing connection. Closing an unopened
// shard is a no-op.
func (sh *Shards) Close() error {
	sh.cancel()
	errs := make([]error, len(sh.sessions))
	var wg sync.WaitGroup
	for i, s := range sh.sessions {
		wg.Go(func() { errs[i] = s.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// reconnect connects shard i again after it dropped, reporting on Failed
// when it cannot. A drop while the shard is already reconnecting is ignored.
//
// Parameters:
//   - i (int): shard ID.
func (sh *Shards) reconnect(i int) {
	if sh.ctx.Err() != nil || !sh.reconnecting[i].CompareAndSwap(false, true) {
		return
	}
	defer sh.reconnecting[i].Store(false)

	sh.logger.Warn("shard disconnected, reconnecting", slog.Int("shard", i))
	if err := sh.connect(sh.ctx, i); err != nil && sh.ctx.Err() == nil {
		select {
		case sh.failed <- err:
		default:
		}
	}
}

// connect opens shard i once its identify bucket allows it, retrying with
// backoff until it connects, ctx ends or Discord rejects it for good.
//
// Parameters:
//   - ctx (context.Context): stops the retries.
//   - i (int): shard ID.
func (sh *Shards) connect(ctx context.Context, i int) error {
	delay := sh.minRetry
	for attempt := 1; ; attempt++ {
		if err := sh.gate.wait(ctx, i); err != nil {
			return err
		}
		err := sh.open(i)
		if err == nil || errors.Is(err, discordgo.ErrWSAlreadyOpen) {
			// A Close that ran while the shard was connecting found nothing
			// to close, so the new connection is closed here.
			if sh.ctx.Err() != nil {
				_ = sh.sessions[i].Close()
				return sh.ctx.Err()
			}
			return nil
		}

		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			if reason, ok := fatalCloseCodes[closeErr.Code]; ok {
				return fmt.Errorf("shard %d: %s: %w", i, reason, err)
			}
		}
		sh.logger.Warn("connect shard",
			slog.Int("shard", i),
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", delay),
			slog.Any("error", err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, sh.maxRetry)
	}
}

// identifyGate spaces session starts as Discord requires. Shards whose IDs
// are equal modulo max_concurrency share a bucket, and a bucket starts at
// most one session per interval.
type identifyGate struct {
	interval time.Duration
	// buckets each hold the earliest time the bucket may start its next
	// session; taking the value is what locks the bucket.
	buckets []chan time.Time
}

// newIdentifyGate returns a gate whose buckets may all start a session now.
//
// Parameters:
//   - maxConcurrency (int): number of buckets.
//   - interval (time.Duration): wait between two starts in one bucket.
func newIdentifyGate(maxConcurrency int, interval time.Duration) *identifyGate {
	g := &identifyGate{interval: interval, buckets: make([]chan time.Time, maxConcurrency)}
	for i := range g.buckets {
		g.buckets[i] = make(chan time.Time, 1)
		g.buckets[i] <- time.Time{}
	}
	return g
}

// wait blocks until shard may start a session, then claims the slot.
//
// Parameters:
//   - ctx (context.Context): stops the wait.
//   - shard (int): shard ID.
func (g *identifyGate) wait(ctx context.Context, shard int) error {
	bucket := g.buckets[shard%len(g.buckets)]
	var next time.Time
	select {
	case <-ctx.Done():
		return ctx.Err()
	case next = <-bucket:
	}

	if d := time.Until(next); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			bucket <- next
			return ctx.Err()
		case <-timer.C:
		}
	}
	bucket <- time.Now().Add(g.interval)
	return nil
}
