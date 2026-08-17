package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// A reasoning model can take ten minutes to its first token. The proxy must not
// be the side that gives up — if it is, it has replaced the observation with
// its own impatience and there is nothing left to diagnose.
//
// The delays here are seconds rather than minutes, but they exceed every
// deadline the proxy previously shipped with (response_header 120s applied to
// the header wait, stream_idle 60s to the body wait), so the assertions fail
// against those defaults and pass against "no limit".

func TestSlowFirstTokenIsNotCutOffByTheProxy(t *testing.T) {
	t.Parallel()

	const think = 1200 * time.Millisecond
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		// Headers are out; now the model thinks for a long time before saying
		// anything. This is the case that matters.
		time.Sleep(think)

		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"finally"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})

	h := newHarnessWithUpstream(t, upstream, patientRoute(200*time.Millisecond))

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("the proxy gave up on a slow first token: %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
	if !snap.Stream.DoneSeen {
		t.Error("the stream did not complete")
	}
	if snap.Duration < think {
		t.Errorf("duration = %v, want at least the %v the upstream spent thinking",
			snap.Duration, think)
	}

	// The wait must be visible while it happens, or the operator cannot tell a
	// thinking model from a hung one.
	if !hasWarningText(snap.Warnings, "no data from the upstream") {
		t.Errorf("warnings = %+v, want the wait reported", snap.Warnings)
	}
}

// The same, but the vendor withholds the response header itself — which is what
// a non-streaming request to a slow model looks like.
func TestSlowResponseHeaderIsNotCutOffByTheProxy(t *testing.T) {
	t.Parallel()

	const think = 1200 * time.Millisecond
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(think)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","choices":[{"index":0,"finish_reason":"stop"}]}`)
	})

	h := newHarnessWithUpstream(t, upstream, patientRoute(200*time.Millisecond))

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("the proxy gave up waiting for the response header: %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
	if snap.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", snap.Status)
	}
	if !hasWarningText(snap.Warnings, "first byte") {
		t.Errorf("warnings = %+v, want the wait for the first byte reported", snap.Warnings)
	}
}

// The inter-chunk clock must start when the body starts, not when the request
// arrived. Otherwise a slow first token silently consumes the idle budget and
// the stream dies the moment it begins.
func TestIdleClockStartsAtTheBodyNotTheRequest(t *testing.T) {
	t.Parallel()

	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		// Longer than stream_idle below: if the idle clock had been running
		// since the request arrived, the very first chunk would arrive already
		// over budget.
		time.Sleep(700 * time.Millisecond)

		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})

	h := newHarnessWithUpstream(t, upstream, func(r *config.Route) {
		r.Timeouts.ResponseHeader = dur(0) // no limit on the header
		r.Timeouts.StreamIdle = dur(1500 * time.Millisecond)
		r.Timeouts.GapWarn = dur(300 * time.Millisecond)
	})

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("the idle deadline fired on the first chunk: %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
}

// Patience is the default, not an absence of supervision: when a deadline *is*
// configured it still fires, and the vendor still gets the blame for going
// silent.
func TestConfiguredIdleDeadlineStillFires(t *testing.T) {
	t.Parallel()

	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(3 * time.Second)
	})

	h := newHarnessWithUpstream(t, upstream, func(r *config.Route) {
		r.Timeouts.StreamIdle = dur(250 * time.Millisecond)
		r.Timeouts.GapWarn = dur(100 * time.Millisecond)
	})

	start := time.Now()
	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindStallTimeout, fault.KindReadTimeout)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the configured deadline took %v to fire", elapsed)
	}
	if !strings.Contains(snap.Fault.Detail, "stream_idle") {
		t.Errorf("Detail = %q, want the setting that fired named so the operator "+
			"knows the proxy ended it", snap.Fault.Detail)
	}
}

// The progress report repeats: a single line at the 30s mark tells you nothing
// about whether a ten-minute wait is still going.
func TestWaitIsReportedRepeatedly(t *testing.T) {
	t.Parallel()

	var ticks atomic.Int64
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(700 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})

	h := newHarnessWithUpstream(t, upstream, func(r *config.Route) {
		r.Timeouts.StreamIdle = dur(0)
		r.Timeouts.GapWarn = dur(150 * time.Millisecond)
	})
	h.onGapWarn = func() { ticks.Add(1) }

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)
	if snap.Fault != nil {
		t.Fatalf("unexpected fault: %s", snap.Fault.Kind)
	}
	// ~700ms of silence at a 150ms interval should report several times, not once.
	if n := ticks.Load(); n < 2 {
		t.Errorf("the wait was reported %d time(s); a long wait must report progress "+
			"repeatedly or it is indistinguishable from a hang", n)
	}
}

// patientRoute mirrors the shipped defaults — no deadline after the connection
// — with a short heartbeat so the test does not have to wait 30 seconds to see
// one.
func patientRoute(gapWarn time.Duration) func(*config.Route) {
	return func(r *config.Route) {
		r.Timeouts.ResponseHeader = dur(0)
		r.Timeouts.StreamIdle = dur(0)
		r.Timeouts.Total = dur(0)
		r.Timeouts.GapWarn = dur(gapWarn)
	}
}

func hasWarningText(warnings []record.Warning, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w.Text, substr) {
			return true
		}
	}
	return false
}
