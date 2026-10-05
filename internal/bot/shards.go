package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// identifyInterval is how long Discord makes each identify bucket wait
// between two session starts.
const identifyInterval = 5 * time.Second

// ShardPlan is how many shards to run and how many of them may identify at
// once.
type ShardPlan struct {
	Count          int
	MaxConcurrency int
}

// PlanShards picks the shard count, falling back to Discord's recommendation
// when count is 0. It fails when Discord would not let every shard start a
// session: going over the daily limit makes Discord reset the bot token.
//
// Parameters:
//   - gw (*discordgo.GatewayBotResponse): the GET /gateway/bot response.
//   - count (int): shards to run, or 0 for Discord's recommendation.
func PlanShards(gw *discordgo.GatewayBotResponse, count int) (ShardPlan, error) {
	plan := ShardPlan{Count: count, MaxConcurrency: max(gw.SessionStartLimit.MaxConcurrency, 1)}
	if plan.Count == 0 {
		plan.Count = max(gw.Shards, 1)
	}
	if limit := gw.SessionStartLimit; limit.Remaining < plan.Count {
		reset := time.Duration(limit.ResetAfter) * time.Millisecond
		return ShardPlan{}, fmt.Errorf("discord allows %d more session starts until the limit resets in %s, fewer than the %d shards to start",
			limit.Remaining, reset.Round(time.Second), plan.Count)
	}
	return plan, nil
}

// Shards is one gateway session per shard. Every session shares a single
// REST rate limiter, since Discord's limits apply to the bot as a whole.
type Shards struct {
	sessions       []*discordgo.Session
	maxConcurrency int
}

// NewShards creates a session for each shard in plan, without connecting.
//
// Parameters:
//   - token (string): bot token, without the "Bot " prefix.
//   - plan (ShardPlan): shards to create.
func NewShards(token string, plan ShardPlan) (*Shards, error) {
	limiter := discordgo.NewRatelimiter()
	sessions := make([]*discordgo.Session, plan.Count)
	for i := range sessions {
		s, err := discordgo.New("Bot " + token)
		if err != nil {
			return nil, fmt.Errorf("create discord session: %w", err)
		}
		s.ShardID = i
		s.ShardCount = plan.Count
		s.Identify.Intents = Intents
		s.Ratelimiter = limiter
		sessions[i] = s
	}
	return &Shards{sessions: sessions, maxConcurrency: plan.MaxConcurrency}, nil
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

// Open connects every shard, starting at most MaxConcurrency of them per
// identify interval. On failure it closes the shards it opened.
//
// Parameters:
//   - ctx (context.Context): stops the startup between two batches.
func (sh *Shards) Open(ctx context.Context) error {
	err := openBatches(ctx, len(sh.sessions), sh.maxConcurrency, identifyInterval, func(i int) error {
		if err := sh.sessions[i].Open(); err != nil {
			return fmt.Errorf("shard %d: %w", i, err)
		}
		return nil
	})
	if err != nil {
		return errors.Join(err, sh.Close())
	}
	return nil
}

// Close disconnects every shard at once, since discordgo pauses a second
// for each closing connection. Closing an unopened shard is a no-op.
func (sh *Shards) Close() error {
	errs := make([]error, len(sh.sessions))
	var wg sync.WaitGroup
	for i, s := range sh.sessions {
		wg.Go(func() { errs[i] = s.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// openBatches calls open for shards 0 to n-1 in batches of maxConcurrency,
// pausing for interval after each batch. A batch holds one shard from each identify bucket
// (shard ID modulo maxConcurrency), so its shards open concurrently.
//
// Parameters:
//   - ctx (context.Context): stops the wait between two batches.
//   - n (int): number of shards.
//   - maxConcurrency (int): shards that may identify at once.
//   - interval (time.Duration): pause between two batches.
//   - open (func(int) error): connects one shard.
func openBatches(ctx context.Context, n, maxConcurrency int, interval time.Duration, open func(int) error) error {
	for start := 0; start < n; start += maxConcurrency {
		if start > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}

		end := min(start+maxConcurrency, n)
		errs := make([]error, end-start)
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			wg.Go(func() { errs[i-start] = open(i) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	return nil
}
