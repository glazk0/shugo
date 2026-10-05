package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

func TestPlanShards(t *testing.T) {
	t.Parallel()

	gateway := func(shards, remaining, maxConcurrency int) *discordgo.GatewayBotResponse {
		return &discordgo.GatewayBotResponse{
			Shards: shards,
			SessionStartLimit: discordgo.SessionInformation{
				Total:          1000,
				Remaining:      remaining,
				ResetAfter:     90_000,
				MaxConcurrency: maxConcurrency,
			},
		}
	}

	tests := []struct {
		name    string
		gw      *discordgo.GatewayBotResponse
		count   int
		want    ShardPlan
		wantErr string
	}{
		{
			name: "recommended count",
			gw:   gateway(3, 1000, 1),
			want: ShardPlan{Count: 3, Recommended: 3, MaxConcurrency: 1},
		},
		{
			name:  "configured count wins",
			gw:    gateway(3, 1000, 16),
			count: 8,
			want:  ShardPlan{Count: 8, Recommended: 3, MaxConcurrency: 16},
		},
		{
			name: "missing values default to one",
			gw:   gateway(0, 1000, 0),
			want: ShardPlan{Count: 1, Recommended: 1, MaxConcurrency: 1},
		},
		{
			name: "budget covers every shard twice",
			gw:   gateway(4, 8, 1),
			want: ShardPlan{Count: 4, Recommended: 4, MaxConcurrency: 1},
		},
		{
			name:    "budget leaves no reserve",
			gw:      gateway(4, 7, 1),
			wantErr: "7 more session starts until the limit resets in 1m30s, fewer than twice the 4 shards",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := PlanShards(tt.gw, tt.count)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PlanShards() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanShards() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("PlanShards() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// testShards builds n unopened shards whose identify slots and retries take
// a millisecond, with open replaced by fn when it is not nil.
//
// Parameters:
//   - t (*testing.T): closes the shards when the test ends.
//   - n (int): number of shards.
//   - fn (func(int) error): stands in for connecting a shard.
func testShards(t *testing.T, n int, fn func(int) error) *Shards {
	t.Helper()
	first, err := newSession("Bot token", discordgo.NewRatelimiter())
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	sh, err := newShards(first, ShardPlan{Count: n, Recommended: n, MaxConcurrency: n}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newShards() error = %v", err)
	}
	sh.gate = newIdentifyGate(n, time.Millisecond)
	sh.minRetry, sh.maxRetry = time.Millisecond, time.Millisecond
	if fn != nil {
		sh.open = fn
	}
	t.Cleanup(func() { _ = sh.Close() })
	return sh
}

func TestNewShards(t *testing.T) {
	t.Parallel()

	sh := testShards(t, 3, nil)
	if len(sh.sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(sh.sessions))
	}
	for i, s := range sh.sessions {
		if s.ShardID != i || s.ShardCount != 3 {
			t.Errorf("session %d shard = %d/%d", i, s.ShardID, s.ShardCount)
		}
		if s.Identify.Intents != Intents || s.Token != "Bot token" {
			t.Errorf("session %d intents/token = %v/%q", i, s.Identify.Intents, s.Token)
		}
		if s.Ratelimiter != sh.Session().Ratelimiter {
			t.Errorf("session %d has its own rate limiter, want a shared one", i)
		}
		if s.ShouldReconnectOnError {
			t.Errorf("session %d reconnects on its own, want Shards to do it", i)
		}
	}
}

func TestOpenRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	var attempts [2]atomic.Int32
	sh := testShards(t, 2, func(i int) error {
		if attempts[i].Add(1) < 3 && i == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err := sh.Open(t.Context()); err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := attempts[1].Load(); got != 3 {
		t.Errorf("shard 1 attempts = %d, want 3", got)
	}
	if got := attempts[0].Load(); got != 1 {
		t.Errorf("shard 0 attempts = %d, want 1", got)
	}
}

func TestOpenStopsOnFatalCloseCode(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	sh := testShards(t, 2, func(i int) error {
		// Shard 0 keeps retrying, so only shard 1's failure can end the
		// startup.
		if i == 0 {
			return io.ErrUnexpectedEOF
		}
		attempts.Add(1)
		return &websocket.CloseError{Code: 4011, Text: "Sharding required."}
	})

	err := sh.Open(t.Context())
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != 4011 {
		t.Fatalf("Open() error = %v, want close 4011", err)
	}
	if !strings.Contains(err.Error(), "shard 1: sharding required") {
		t.Errorf("Open() error = %q, want it to name the shard and reason", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a fatal close must not be retried", got)
	}
}

func TestOpenReturnsCanceledWhenStopped(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	sh := testShards(t, 1, func(int) error {
		cancel()
		return io.ErrUnexpectedEOF
	})
	if err := sh.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open() error = %v, want context.Canceled", err)
	}
}

func TestReconnectReportsFatalFailure(t *testing.T) {
	t.Parallel()

	sh := testShards(t, 1, func(int) error {
		return &websocket.CloseError{Code: 4004, Text: "Authentication failed."}
	})
	sh.reconnect(0)

	select {
	case err := <-sh.Failed():
		if !strings.Contains(err.Error(), "authentication failed") {
			t.Errorf("Failed() = %v, want authentication failure", err)
		}
	default:
		t.Fatal("Failed() received nothing")
	}
}

func TestReconnectStopsAfterClose(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	sh := testShards(t, 1, func(int) error {
		attempts.Add(1)
		return nil
	})
	if err := sh.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sh.reconnect(0)
	if got := attempts.Load(); got != 0 {
		t.Errorf("attempts = %d after Close, want 0", got)
	}
}

func TestIdentifyGateSpacesBucket(t *testing.T) {
	t.Parallel()

	const interval = 30 * time.Millisecond
	g := newIdentifyGate(2, interval)

	start := time.Now()
	// Shards 0 and 1 use different buckets, so neither waits.
	for _, shard := range []int{0, 1} {
		if err := g.wait(t.Context(), shard); err != nil {
			t.Fatalf("wait(%d) error = %v", shard, err)
		}
	}
	if elapsed := time.Since(start); elapsed >= interval {
		t.Errorf("first starts took %v, want no wait", elapsed)
	}

	// Shard 2 shares shard 0's bucket.
	if err := g.wait(t.Context(), 2); err != nil {
		t.Fatalf("wait(2) error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < interval {
		t.Errorf("shard 2 started after %v, want at least %v", elapsed, interval)
	}
}

func TestIdentifyGateWaitStopsWithContext(t *testing.T) {
	t.Parallel()

	g := newIdentifyGate(1, time.Hour)
	if err := g.wait(t.Context(), 0); err != nil {
		t.Fatalf("wait() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := g.wait(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait() error = %v, want context.DeadlineExceeded", err)
	}

	// The canceled wait must hand the slot back rather than lose it.
	select {
	case next := <-g.buckets[0]:
		if time.Until(next) < 50*time.Minute {
			t.Errorf("bucket reopens at %v, want about an hour from now", next)
		}
	default:
		t.Fatal("canceled wait kept the bucket locked")
	}
}
