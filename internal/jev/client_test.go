package jev_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/jev"
)

const sampleResponse = `{
  "model": "jev-1.13.0",
  "answers": {
    "category": {
      "type": "choice",
      "choice": "spam",
      "confidence": 0.9,
      "probabilities": {"none": 0.05, "spam": 0.95}
    },
    "suspicious": {"type": "noul", "noul": 0.82},
    "severity": {
      "type": "score",
      "score": 1.4,
      "confidence": 0.7,
      "probabilities": {"0": 0.1, "1": 0.4, "2": 0.5},
      "legend": {"0": "none", "1": "minor", "2": "severe"}
    }
  },
  "usage": {"input_tokens": 328, "output_tokens": 34}
}`

// newClient returns a client pointed at srv with fast, deterministic retries.
//
// Parameters:
//   - srv (*httptest.Server): fake API server.
//   - retries (int): retries after the first attempt.
func newClient(srv *httptest.Server, retries int) *jev.Client {
	return jev.NewClient("test-key",
		jev.WithEndpoint(srv.URL),
		jev.WithHTTPClient(srv.Client()),
		jev.WithRetry(retries, time.Millisecond, 2*time.Millisecond),
	)
}

func TestEvaluateSendsRequestAndDecodesAnswers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}

		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got := string(body["model"]); got != `"jev-latest"` {
			t.Errorf("model = %s, want default model", got)
		}
		var questions map[string]map[string]any
		if err := json.Unmarshal(body["questions"], &questions); err != nil {
			t.Fatalf("decode questions: %v", err)
		}
		if questions["suspicious"]["type"] != "noul" {
			t.Errorf("suspicious type = %v", questions["suspicious"]["type"])
		}
		if levels, ok := questions["severity"]["criteria"].([]any); !ok || len(levels) != 3 {
			t.Errorf("severity criteria = %v, want 3 ordered levels", questions["severity"]["criteria"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleResponse))
	}))
	t.Cleanup(srv.Close)

	resp, err := newClient(srv, 0).Evaluate(t.Context(), jev.Request{
		State: map[string]string{"message": "buy cheap nitro"},
		Questions: map[string]jev.Question{
			"category":   jev.Choice("What is it?", map[string]string{"none": "fine", "spam": "spam"}),
			"suspicious": jev.Noul("Is the author suspicious?", "yes", "no"),
			"severity":   jev.Score("How severe?", "none", "minor", "severe"),
		},
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	if resp.Model != "jev-1.13.0" {
		t.Errorf("Model = %q", resp.Model)
	}
	if got := resp.Answers["category"]; got.Choice != "spam" || got.Probabilities["none"] != 0.05 {
		t.Errorf("category = %+v", got)
	}
	if got := resp.Answers["suspicious"].Noul; got != 0.82 {
		t.Errorf("suspicious.noul = %v", got)
	}
	if got := resp.Answers["severity"]; got.Score != 1.4 || got.Legend["2"] != "severe" {
		t.Errorf("severity = %+v", got)
	}
	if resp.Usage.InputTokens != 328 || resp.Usage.OutputTokens != 34 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestEvaluateKeepsExplicitModel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body jev.Request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Model != "jev-1.13.0" {
			t.Errorf("model = %q, want pinned model", body.Model)
		}
		_, _ = w.Write([]byte(sampleResponse))
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(srv, 0).Evaluate(t.Context(), jev.Request{Model: "jev-1.13.0", State: "hi"})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
}

func TestEvaluateRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statuses  []int
		retries   int
		wantCalls int32
		wantErr   int // expected APIError status, 0 for success
	}{
		{name: "rate limited then ok", statuses: []int{429, 200}, retries: 3, wantCalls: 2},
		{name: "overloaded then ok", statuses: []int{529, 529, 200}, retries: 3, wantCalls: 3},
		{name: "server error then ok", statuses: []int{503, 200}, retries: 3, wantCalls: 2},
		{name: "gives up after max retries", statuses: []int{429, 429, 429}, retries: 2, wantCalls: 3, wantErr: 429},
		{name: "unauthorized is not retried", statuses: []int{401}, retries: 3, wantCalls: 1, wantErr: 401},
		{name: "unprocessable is not retried", statuses: []int{422}, retries: 3, wantCalls: 1, wantErr: 422},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := calls.Add(1)
				status := tt.statuses[min(int(n), len(tt.statuses))-1]
				if status != http.StatusOK {
					http.Error(w, `{"error":"nope"}`, status)
					return
				}
				_, _ = w.Write([]byte(sampleResponse))
			}))
			t.Cleanup(srv.Close)

			_, err := newClient(srv, tt.retries).Evaluate(t.Context(), jev.Request{State: "hi"})

			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("calls = %d, want %d", got, tt.wantCalls)
			}
			if tt.wantErr == 0 {
				if err != nil {
					t.Fatalf("Evaluate() error = %v", err)
				}
				return
			}
			apiErr, ok := errors.AsType[*jev.APIError](err)
			if !ok {
				t.Fatalf("Evaluate() error = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tt.wantErr {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.wantErr)
			}
		})
	}
}

func TestEvaluateDoesNotRetryMalformedBody(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("{not json"))
	}))
	t.Cleanup(srv.Close)

	if _, err := newClient(srv, 3).Evaluate(t.Context(), jev.Request{State: "hi"}); err == nil {
		t.Fatal("Evaluate() error = nil, want decode error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestEvaluateStopsWhenContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := newClient(srv, 5).Evaluate(ctx, jev.Request{State: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Evaluate() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Evaluate() honoured Retry-After past the deadline (%s)", elapsed)
	}
}

func TestQuestionConstructors(t *testing.T) {
	t.Parallel()

	got, err := json.Marshal(map[string]jev.Question{
		"a": jev.Noul("Q?", "yes", "no"),
		"b": jev.Choice("Q?", map[string]string{"x": "X"}),
		"c": jev.Score("Q?", "low", "high"),
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	const want = `{"a":{"type":"noul","instructions":"Q?","criteria":{"false":"no","true":"yes"}},` +
		`"b":{"type":"choice","instructions":"Q?","criteria":{"x":"X"}},` +
		`"c":{"type":"score","instructions":"Q?","criteria":["low","high"]}}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}
