package proxy

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/ratelimit"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// retryDecision is the outcome of considering a retry.
type retryDecision struct {
	Retry  bool
	Wait   time.Duration
	Reason string
	// Refused explains why a retry that looked applicable was not taken —
	// always printed, because silently absorbing a six-minute rate limit
	// would be worse than the problem it hides.
	Refused string
}

// shouldRetryStatus decides whether to retry after a response status.
//
// The precondition that matters is not in this function's arguments by
// accident: retry is only possible while nothing has been written downstream.
// The caller asserts that, and a test pins it, because once a status line is on
// the client's socket a transparent proxy cannot start over.
func shouldRetryStatus(
	cfg *config.Retry,
	attempt, status int,
	replayable bool,
	rl *ratelimit.Snapshot,
) retryDecision {
	if cfg == nil {
		return retryDecision{}
	}
	if !statusIn(cfg.On, status) {
		return retryDecision{}
	}
	if !replayable {
		return retryDecision{Refused: "the request body was too large to buffer, so it cannot be replayed"}
	}
	if attempt >= cfg.MaxAttempts {
		return retryDecision{Refused: fmt.Sprintf("already used all %d attempts", cfg.MaxAttempts)}
	}

	wait, reason := computeWait(cfg, attempt, rl)
	if max := cfg.MaxWait.D(); max > 0 && wait > max {
		return retryDecision{Refused: fmt.Sprintf(
			"%s exceeds max_wait %s — forwarding the %d to your tool rather than absorbing it",
			fmtShort(wait), fmtShort(max), status)}
	}
	return retryDecision{Retry: true, Wait: wait, Reason: reason}
}

// shouldRetryConnect decides whether to retry a failure that happened before
// any response header arrived.
func shouldRetryConnect(cfg *config.Retry, attempt int, replayable, faultRetryable bool) retryDecision {
	if cfg == nil || !config.Bool(cfg.OnConnectError) || !faultRetryable {
		return retryDecision{}
	}
	if !replayable {
		return retryDecision{Refused: "the request body was too large to buffer, so it cannot be replayed"}
	}
	if attempt >= cfg.MaxAttempts {
		return retryDecision{Refused: fmt.Sprintf("already used all %d attempts", cfg.MaxAttempts)}
	}
	wait := backoff(cfg, attempt)
	return retryDecision{
		Retry:  true,
		Wait:   wait,
		Reason: fmt.Sprintf("backoff %s", fmtShort(wait)),
	}
}

// computeWait takes the longest of the vendor's instructions and our own
// backoff. Retry-After is authoritative when present; an exhausted bucket's
// reset is the next best thing; backoff is the floor.
//
// The arithmetic is returned as text so the console can show why it waited what
// it waited, rather than presenting a number nobody can check.
func computeWait(cfg *config.Retry, attempt int, rl *ratelimit.Snapshot) (time.Duration, string) {
	var terms []string
	wait := backoff(cfg, attempt)
	terms = append(terms, "backoff "+fmtShort(wait))

	if rl != nil && config.Bool(cfg.RespectResetHeaders) {
		if rl.RetryAfter != nil {
			terms = append([]string{"retry-after " + fmtShort(*rl.RetryAfter)}, terms...)
			if *rl.RetryAfter > wait {
				wait = *rl.RetryAfter
			}
		}
		// Only exhausted buckets can block: one with capacity left says
		// nothing about when this request may proceed.
		if reset := rl.EarliestReset(); reset != nil {
			terms = append(terms, "reset "+fmtShort(*reset))
			if *reset > wait {
				wait = *reset
			}
		}
	}

	reason := "max(" + strings.Join(terms, ", ") + ")"
	if j := jitter(cfg, wait); j != 0 {
		wait += j
		if wait < 0 {
			wait = 0
		}
		sign := "+"
		if j < 0 {
			sign, j = "-", -j
		}
		reason += fmt.Sprintf(" %s jitter %s", sign, fmtShort(j))
	}
	// The ceiling is a refusal threshold, not a clamp: exceeding it forwards
	// the response instead of quietly waiting less than the vendor asked for.
	if max := cfg.MaxWait.D(); max > 0 {
		reason += "  [refuse above " + fmtShort(max) + "]"
	}
	return wait, reason
}

// backoff is exponential from BaseBackoff, capped at MaxBackoff.
func backoff(cfg *config.Retry, attempt int) time.Duration {
	base := cfg.BaseBackoff.D()
	if base <= 0 {
		return 0
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if max := cfg.MaxBackoff.D(); max > 0 && d > max {
		d = max
	}
	return d
}

// jitter spreads retries so several stalled requests do not resume in lockstep.
//
// It is symmetric: only ever adding would bias every wait upward, and a wait
// the vendor asked for should be honoured on average, not systematically
// overshot.
func jitter(cfg *config.Retry, wait time.Duration) time.Duration {
	if cfg.Jitter <= 0 || wait <= 0 {
		return 0
	}
	span := float64(wait) * cfg.Jitter
	return time.Duration((rand.Float64()*2 - 1) * span)
}

func statusIn(list []int, status int) bool {
	for _, s := range list {
		if s == status {
			return true
		}
	}
	return false
}

// recordWait stamps the decision onto the attempt so the console and the span
// can both explain the delay. It goes through Set because every Attempt field
// is guarded — the httptrace hooks write from the transport's goroutines.
func recordWait(a *record.Attempt, d retryDecision) {
	a.Set(func(at *record.Attempt) {
		at.WaitReason = d.Reason
		at.Waited = d.Wait
	})
}

// fmtShort renders a duration compactly for log lines.
func fmtShort(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.3gs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}
