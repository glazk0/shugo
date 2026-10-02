// Package config loads the bot configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/glazk0/shugo/internal/jev"
	"github.com/glazk0/shugo/internal/moderation"
)

// Config holds every runtime setting.
type Config struct {
	// DiscordToken is the bot token, without the "Bot " prefix.
	DiscordToken string
	// TypeSafeAPIKey authenticates against the Jev API.
	TypeSafeAPIKey string
	// JevEndpoint is the System One endpoint URL.
	JevEndpoint string
	// JevModel is the Jev model alias or pinned version.
	JevModel string

	// LogChannelID receives moderation reports; empty disables reports.
	LogChannelID string
	// DryRun reports what would happen without deleting or timing out.
	DryRun bool
	// TimeoutDuration is how long offending members are timed out for.
	TimeoutDuration time.Duration
	// Policy holds the moderation thresholds.
	Policy moderation.Policy

	// HistorySize is how many recent messages are kept per member.
	HistorySize int
	// HistoryTTL is how long a message stays in a member's history.
	HistoryTTL time.Duration

	// MaxConcurrency caps in-flight Jev evaluations.
	MaxConcurrency int
	// QueueTimeout is how long a message may wait for a free evaluation slot
	// before it is dropped.
	QueueTimeout time.Duration
	// EvaluationTimeout bounds a single message evaluation, retries included.
	EvaluationTimeout time.Duration
	// EnforcementTimeout bounds the Discord calls that apply a verdict, and
	// separately the report sent afterwards.
	EnforcementTimeout time.Duration
	// ShutdownTimeout is how long shutdown waits for in-flight messages.
	ShutdownTimeout time.Duration

	// LogLevel is the minimum level written by the logger.
	LogLevel slog.Level
}

// LookupFunc reads an environment variable; os.LookupEnv satisfies it.
type LookupFunc func(key string) (string, bool)

// Load reads the configuration through lookup, applying defaults for unset
// optional variables. It reports every invalid variable at once.
//
// Parameters:
//   - lookup (LookupFunc): environment accessor, typically os.LookupEnv.
func Load(lookup LookupFunc) (Config, error) {
	p := parser{lookup: lookup}

	cfg := Config{
		DiscordToken:   p.required("DISCORD_TOKEN"),
		TypeSafeAPIKey: p.required("TYPESAFE_API_KEY"),
		JevEndpoint:    p.string("JEV_ENDPOINT", jev.DefaultEndpoint),
		JevModel:       p.string("JEV_MODEL", jev.DefaultModel),

		LogChannelID:    p.string("LOG_CHANNEL_ID", ""),
		DryRun:          p.bool("DRY_RUN", false),
		TimeoutDuration: p.duration("TIMEOUT_DURATION", 10*time.Minute),
		Policy: moderation.Policy{
			FlagRisk:        p.float("FLAG_RISK", moderation.DefaultPolicy.FlagRisk),
			DeleteRisk:      p.float("DELETE_RISK", moderation.DefaultPolicy.DeleteRisk),
			TimeoutSeverity: p.float("TIMEOUT_SEVERITY", moderation.DefaultPolicy.TimeoutSeverity),
			Suspicion:       p.float("SUSPICION_THRESHOLD", moderation.DefaultPolicy.Suspicion),
		},

		HistorySize: p.int("HISTORY_SIZE", 10),
		HistoryTTL:  p.duration("HISTORY_TTL", 15*time.Minute),

		MaxConcurrency:     p.int("MAX_CONCURRENCY", 16),
		QueueTimeout:       p.duration("QUEUE_TIMEOUT", 5*time.Second),
		EvaluationTimeout:  p.duration("EVALUATION_TIMEOUT", 20*time.Second),
		EnforcementTimeout: p.duration("ENFORCEMENT_TIMEOUT", 10*time.Second),
		ShutdownTimeout:    p.duration("SHUTDOWN_TIMEOUT", 30*time.Second),

		LogLevel: p.level("LOG_LEVEL", slog.LevelInfo),
	}

	cfg.DiscordToken = strings.TrimPrefix(cfg.DiscordToken, "Bot ")

	p.check(cfg.Policy.Validate())
	// Discord caps member timeouts at 28 days.
	if cfg.TimeoutDuration <= 0 || cfg.TimeoutDuration > 28*24*time.Hour {
		p.fail("TIMEOUT_DURATION must be between 1s and 28 days")
	}
	if cfg.HistorySize < 1 {
		p.fail("HISTORY_SIZE must be at least 1")
	}
	if cfg.HistoryTTL <= 0 {
		p.fail("HISTORY_TTL must be positive")
	}
	if cfg.MaxConcurrency < 1 {
		p.fail("MAX_CONCURRENCY must be at least 1")
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"QUEUE_TIMEOUT", cfg.QueueTimeout},
		{"EVALUATION_TIMEOUT", cfg.EvaluationTimeout},
		{"ENFORCEMENT_TIMEOUT", cfg.EnforcementTimeout},
		{"SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout},
	} {
		if d.value <= 0 {
			p.fail(d.name + " must be positive")
		}
	}

	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// parser accumulates errors while reading variables so Load can report them
// all together.
type parser struct {
	lookup LookupFunc
	errs   []error
}

// fail records a validation error.
//
// Parameters:
//   - msg (string): human-readable description.
func (p *parser) fail(msg string) {
	p.errs = append(p.errs, errors.New(msg))
}

// check records err if it is non-nil.
//
// Parameters:
//   - err (error): error to record.
func (p *parser) check(err error) {
	if err != nil {
		p.errs = append(p.errs, err)
	}
}

// value returns the trimmed variable and whether it is set to a non-empty
// value.
//
// Parameters:
//   - key (string): variable name.
func (p *parser) value(key string) (string, bool) {
	v, ok := p.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// required returns key's value, recording an error when it is unset.
//
// Parameters:
//   - key (string): variable name.
func (p *parser) required(key string) string {
	v, ok := p.value(key)
	if !ok {
		p.fail(key + " is required")
	}
	return v
}

// string returns key's value or def.
//
// Parameters:
//   - key (string): variable name.
//   - def (string): default when unset.
func (p *parser) string(key, def string) string {
	if v, ok := p.value(key); ok {
		return v
	}
	return def
}

// bool parses key with strconv.ParseBool, or returns def.
//
// Parameters:
//   - key (string): variable name.
//   - def (bool): default when unset.
func (p *parser) bool(key string, def bool) bool {
	return parse(p, key, def, strconv.ParseBool)
}

// int parses key as a base-10 integer, or returns def.
//
// Parameters:
//   - key (string): variable name.
//   - def (int): default when unset.
func (p *parser) int(key string, def int) int {
	return parse(p, key, def, strconv.Atoi)
}

// float parses key as a float64, or returns def.
//
// Parameters:
//   - key (string): variable name.
//   - def (float64): default when unset.
func (p *parser) float(key string, def float64) float64 {
	return parse(p, key, def, func(s string) (float64, error) { return strconv.ParseFloat(s, 64) })
}

// duration parses key with time.ParseDuration, or returns def.
//
// Parameters:
//   - key (string): variable name.
//   - def (time.Duration): default when unset.
func (p *parser) duration(key string, def time.Duration) time.Duration {
	return parse(p, key, def, time.ParseDuration)
}

// level parses key as a slog level name (debug, info, warn, error), or
// returns def.
//
// Parameters:
//   - key (string): variable name.
//   - def (slog.Level): default when unset.
func (p *parser) level(key string, def slog.Level) slog.Level {
	return parse(p, key, def, func(s string) (slog.Level, error) {
		var l slog.Level
		err := l.UnmarshalText([]byte(s))
		return l, err
	})
}

// parse reads key with fn, recording a descriptive error on failure.
//
// Parameters:
//   - p (*parser): parser that records errors.
//   - key (string): variable name.
//   - def (T): default when unset or invalid.
//   - fn (func(string) (T, error)): converts the raw value.
func parse[T any](p *parser, key string, def T, fn func(string) (T, error)) T {
	raw, ok := p.value(key)
	if !ok {
		return def
	}
	v, err := fn(raw)
	if err != nil {
		p.fail(fmt.Sprintf("%s: invalid value %q", key, raw))
		return def
	}
	return v
}
