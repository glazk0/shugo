package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/config"
	"github.com/glazk0/shugo/internal/jev"
	"github.com/glazk0/shugo/internal/moderation"
)

// env returns a LookupFunc backed by vars.
//
// Parameters:
//   - vars (map[string]string): fake environment.
func env(vars map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// required returns the minimal valid environment merged with extra.
//
// Parameters:
//   - extra (map[string]string): variables to add or override.
func required(extra map[string]string) map[string]string {
	vars := map[string]string{"DISCORD_TOKEN": "token", "TYPESAFE_API_KEY": "key"}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(required(nil)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DiscordToken != "token" || cfg.TypeSafeAPIKey != "key" {
		t.Errorf("credentials = %q/%q", cfg.DiscordToken, cfg.TypeSafeAPIKey)
	}
	if cfg.JevEndpoint != jev.DefaultEndpoint || cfg.JevModel != jev.DefaultModel {
		t.Errorf("jev = %q/%q", cfg.JevEndpoint, cfg.JevModel)
	}
	if cfg.Policy != moderation.DefaultPolicy {
		t.Errorf("Policy = %+v, want default", cfg.Policy)
	}
	if cfg.DryRun || cfg.LogChannelID != "" {
		t.Errorf("DryRun/LogChannelID = %v/%q", cfg.DryRun, cfg.LogChannelID)
	}
	if cfg.TimeoutDuration != 10*time.Minute || cfg.HistorySize != 10 || cfg.HistoryTTL != 15*time.Minute {
		t.Errorf("durations/sizes = %v/%d/%v", cfg.TimeoutDuration, cfg.HistorySize, cfg.HistoryTTL)
	}
	if cfg.MaxConcurrency != 16 || cfg.EvaluationTimeout != 20*time.Second {
		t.Errorf("concurrency = %d/%v", cfg.MaxConcurrency, cfg.EvaluationTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(required(map[string]string{
		"DISCORD_TOKEN":       "Bot abc",
		"JEV_MODEL":           "jev-1.13.0",
		"LOG_CHANNEL_ID":      "123",
		"DRY_RUN":             "true",
		"TIMEOUT_DURATION":    "1h",
		"FLAG_RISK":           "0.4",
		"DELETE_RISK":         "0.9",
		"TIMEOUT_SEVERITY":    "0.6",
		"SUSPICION_THRESHOLD": "0.7",
		"HISTORY_SIZE":        "25",
		"HISTORY_TTL":         " 30m ",
		"MAX_CONCURRENCY":     "4",
		"LOG_LEVEL":           "debug",
	})))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DiscordToken != "abc" {
		t.Errorf("DiscordToken = %q, want Bot prefix stripped", cfg.DiscordToken)
	}
	want := moderation.Policy{FlagRisk: 0.4, DeleteRisk: 0.9, TimeoutSeverity: 0.6, Suspicion: 0.7}
	if cfg.Policy != want {
		t.Errorf("Policy = %+v, want %+v", cfg.Policy, want)
	}
	if cfg.JevModel != "jev-1.13.0" || cfg.LogChannelID != "123" || !cfg.DryRun {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.TimeoutDuration != time.Hour || cfg.HistorySize != 25 || cfg.HistoryTTL != 30*time.Minute {
		t.Errorf("durations/sizes = %v/%d/%v", cfg.TimeoutDuration, cfg.HistorySize, cfg.HistoryTTL)
	}
	if cfg.MaxConcurrency != 4 || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("MaxConcurrency/LogLevel = %d/%v", cfg.MaxConcurrency, cfg.LogLevel)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"TYPESAFE_API_KEY": "   ",
		"DRY_RUN":          "maybe",
		"HISTORY_SIZE":     "0",
		"TIMEOUT_DURATION": "29d",
		"FLAG_RISK":        "0.95",
		"LOG_LEVEL":        "loud",
	}))
	if err == nil {
		t.Fatal("Load() error = nil")
	}

	for _, want := range []string{
		"DISCORD_TOKEN is required",
		"TYPESAFE_API_KEY is required",
		`DRY_RUN: invalid value "maybe"`,
		"HISTORY_SIZE must be at least 1",
		`TIMEOUT_DURATION: invalid value "29d"`,
		"flag risk 0.95 exceeds delete risk",
		`LOG_LEVEL: invalid value "loud"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadRejectsTimeoutAboveDiscordLimit(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(required(map[string]string{"TIMEOUT_DURATION": "700h"})))
	if err == nil || !strings.Contains(err.Error(), "TIMEOUT_DURATION") {
		t.Errorf("Load() error = %v, want TIMEOUT_DURATION error", err)
	}
}
