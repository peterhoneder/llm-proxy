package proxy

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/ratelimit"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

func withRetry(maxAttempts int, maxWait time.Duration) func(*config.Route) {
	return func(r *config.Route) {
		r.Retry = &config.Retry{
			MaxAttempts: maxAttempts,
			On:          []int{429, 500, 502, 503, 504},
			MaxWait:     config.Duration(maxWait),
			BaseBackoff: config.Duration(10 * time.Millisecond),
			MaxBackoff:  config.Duration(50 * time.Millisecond),
		}
		r.Retry.RespectResetHeaders = ptr(true)
	}
}

func ptr[T any](v T) *T { return &v }

func TestRetryHonoursRetryAfter(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(3, time.Minute),
		testutil.Script{
			Status: http.StatusTooManyRequests,
			Header: http.Header{"Retry-After": []string{"1"}},
			Body:   `{"message":"Requests rate limit exceeded"}`,
		},
		testutil.StreamOf(2, testutil.EndDone),
	)

	start := time.Now()
	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)
	elapsed := time.Since(start)

	if snap.Fault != nil {
		t.Fatalf("the retry should have succeeded, got %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}
	if len(snap.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(snap.Attempts))
	}
	// The vendor said one second, so waiting materially less would mean the
	// header was ignored.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want the full Retry-After of 1s to be honoured", elapsed)
	}
	first := snap.Attempts[0]
	if first.WaitReason == "" {
		t.Error("the retry arithmetic must be recorded so the console can explain the delay")
	}
	if len(first.ErrorBody) == 0 {
		t.Error("the 429 body should be kept verbatim for the report")
	}
}

// Absorbing a six-minute rate limit silently would be worse than the problem it
// hides, so a wait beyond max_wait forwards the 429 instead.
func TestRetryRefusedWhenWaitExceedsMaxWait(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(3, 2*time.Second),
		testutil.Script{
			Status: http.StatusTooManyRequests,
			Header: http.Header{"X-Ratelimit-Reset-Requests": []string{"6m0s"},
				"X-Ratelimit-Remaining-Requests": []string{"0"}},
			Body: `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`,
		},
	)

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if len(snap.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1: the wait was too long to honour", len(snap.Attempts))
	}
	if snap.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want the 429 forwarded to the client", snap.Status)
	}
	requireKind(t, snap, fault.KindRateLimited)

	var explained bool
	for _, w := range snap.Warnings {
		if bytes.Contains([]byte(w.Text), []byte("max_wait")) {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the refusal must be explained in the report, got warnings %+v", snap.Warnings)
	}
}

// The invariant the whole retry design rests on: once a status line is on the
// client's socket, a transparent proxy cannot start over.
func TestNoRetryAfterHeadersSent(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(3, time.Minute),
		testutil.Script{
			Ending: testutil.EndReset,
			SSE: []testutil.Event{
				{Raw: `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`},
			},
		},
	)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if len(snap.Attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly 1 — the response was already committed", len(snap.Attempts))
	}
	if snap.HeadersSentAt.IsZero() {
		t.Error("the headers-sent sentinel was not stamped")
	}
	requireSide(t, snap, fault.SideUpstream)
}

// A retried request must be byte-identical, or the second attempt is not the
// same request and any conclusion drawn from it is unsound.
func TestRetryReplaysTheBodyExactly(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(3, time.Minute),
		testutil.Script{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`},
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`},
	)

	body := `{"model":"m","messages":[{"role":"user","content":"exactly this"}]}`
	snap := h.chat(t, body)

	if len(snap.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(snap.Attempts))
	}
	seen := h.upstream.Seen()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	if !bytes.Equal(seen[0].Body, seen[1].Body) {
		t.Errorf("the replayed body differs:\n first: %s\nsecond: %s", seen[0].Body, seen[1].Body)
	}
	if string(seen[1].Body) != body {
		t.Errorf("replayed body = %s, want the client's bytes unmodified", seen[1].Body)
	}
	// An explicit Content-Length keeps the wire form identical to what the
	// client sent; without it net/http would switch to chunked encoding.
	if seen[1].Header.Get("Transfer-Encoding") != "" {
		t.Error("the replayed request went out chunked, changing the wire form")
	}
}

func TestNoRetryWithoutConfiguration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`})

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if len(snap.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1: passthrough is the default", len(snap.Attempts))
	}
	if snap.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want the 500 passed through untouched", snap.Status)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(2, time.Minute),
		testutil.Script{Status: http.StatusBadGateway, Body: `{"error":"nope"}`},
	)

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if len(snap.Attempts) != 2 {
		t.Errorf("attempts = %d, want max_attempts of 2", len(snap.Attempts))
	}
	if snap.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want the last response forwarded", snap.Status)
	}
}

func TestComputeWaitTakesTheLongestTerm(t *testing.T) {
	t.Parallel()
	cfg := &config.Retry{
		MaxAttempts: 3,
		BaseBackoff: config.Duration(500 * time.Millisecond),
		MaxBackoff:  config.Duration(20 * time.Second),
		MaxWait:     config.Duration(time.Minute),
	}
	cfg.RespectResetHeaders = ptr(true)

	retryAfter := 5 * time.Second
	reset := 2 * time.Second
	limit, remaining := int64(10), int64(0)

	rl := &ratelimit.Snapshot{
		RetryAfter: &retryAfter,
		Buckets: []ratelimit.Bucket{
			{Name: "requests", Limit: &limit, Remaining: &remaining, Reset: &reset},
		},
	}

	wait, reason := computeWait(cfg, 1, rl)
	if wait < retryAfter {
		t.Errorf("wait = %v, want at least the Retry-After of %v", wait, retryAfter)
	}
	for _, want := range []string{"retry-after", "reset", "backoff"} {
		if !bytes.Contains([]byte(reason), []byte(want)) {
			t.Errorf("reason = %q, want it to show the %s term so the delay is checkable", reason, want)
		}
	}
}

// A bucket with capacity left says nothing about when this request may
// proceed, so it must not inflate the wait.
func TestComputeWaitIgnoresBucketsWithCapacity(t *testing.T) {
	t.Parallel()
	cfg := &config.Retry{BaseBackoff: config.Duration(10 * time.Millisecond)}
	cfg.RespectResetHeaders = ptr(true)

	long := time.Hour
	limit, remaining := int64(100), int64(99)
	rl := &ratelimit.Snapshot{
		Buckets: []ratelimit.Bucket{
			{Name: "tokens", Limit: &limit, Remaining: &remaining, Reset: &long},
		},
	}

	wait, _ := computeWait(cfg, 1, rl)
	if wait > time.Minute {
		t.Errorf("wait = %v — a bucket that is not exhausted must not delay a retry", wait)
	}
}

func TestBackoffIsExponentialAndCapped(t *testing.T) {
	t.Parallel()
	cfg := &config.Retry{
		BaseBackoff: config.Duration(100 * time.Millisecond),
		MaxBackoff:  config.Duration(250 * time.Millisecond),
	}
	got := []time.Duration{backoff(cfg, 1), backoff(cfg, 2), backoff(cfg, 3), backoff(cfg, 9)}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff(attempt %d) = %v, want %v", i+1, got[i], want[i])
		}
	}
}
