package bot

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
			want: ShardPlan{Count: 3, MaxConcurrency: 1},
		},
		{
			name:  "configured count wins",
			gw:    gateway(3, 1000, 16),
			count: 8,
			want:  ShardPlan{Count: 8, MaxConcurrency: 16},
		},
		{
			name: "missing values default to one",
			gw:   gateway(0, 1000, 0),
			want: ShardPlan{Count: 1, MaxConcurrency: 1},
		},
		{
			name:    "session starts exhausted",
			gw:      gateway(4, 3, 1),
			wantErr: "3 more session starts until the limit resets in 1m30s, fewer than the 4 shards",
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

func TestNewShards(t *testing.T) {
	t.Parallel()

	sh, err := NewShards("token", ShardPlan{Count: 3, MaxConcurrency: 1})
	if err != nil {
		t.Fatalf("NewShards() error = %v", err)
	}
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
	}
	if err := sh.Close(); err != nil {
		t.Errorf("Close() on unopened shards error = %v", err)
	}
}

func TestOpenBatchesPacesIdentifyBuckets(t *testing.T) {
	t.Parallel()

	const interval = 20 * time.Millisecond
	// Each call writes its own element, so the slice needs no lock.
	opened := make([]time.Time, 5)
	start := time.Now()
	err := openBatches(t.Context(), len(opened), 2, interval, func(i int) error {
		opened[i] = time.Now()
		return nil
	})
	if err != nil {
		t.Fatalf("openBatches() error = %v", err)
	}

	// Shards 0-1, 2-3 and 4 form three batches.
	for i, at := range opened {
		batch := i / 2
		if elapsed := at.Sub(start); elapsed < time.Duration(batch)*interval {
			t.Errorf("shard %d opened after %v, want at least %v", i, elapsed, time.Duration(batch)*interval)
		}
	}
}

func TestOpenBatchesStopsOnError(t *testing.T) {
	t.Parallel()

	fail := errors.New("boom")
	var calls []int
	err := openBatches(t.Context(), 4, 1, time.Millisecond, func(i int) error {
		calls = append(calls, i)
		if i == 1 {
			return fail
		}
		return nil
	})
	if !errors.Is(err, fail) {
		t.Fatalf("openBatches() error = %v, want %v", err, fail)
	}
	if !slices.Equal(calls, []int{0, 1}) {
		t.Errorf("opened shards %v, want [0 1]", calls)
	}
}

func TestOpenBatchesStopsWhenCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	var calls int
	err := openBatches(ctx, 3, 1, time.Hour, func(int) error {
		calls++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("openBatches() error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("opened %d shards, want 1", calls)
	}
}
