package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

// These exercise settings end to end rather than against a hand-built
// ReadState. A unit test that constructs the classifier's input directly cannot
// fail when the plumbing that produces that input is missing — which is exactly
// how expect_done ended up decoded, defaulted, validated, documented and
// entirely inert.

func TestExpectDoneStrictIsWired(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t,
		func(r *config.Route) { r.ExpectDone = "true" },
		testutil.Script{
			Ending:       testutil.EndCleanNoDone,
			FinishReason: "stop",
			Usage:        true,
			SSE:          []testutil.Event{{Raw: `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`}},
		},
	)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindTruncatedStream)
	if !strings.Contains(snap.Fault.Detail, "expect_done") {
		t.Errorf("Detail = %q, want it to name the setting that made this strict", snap.Fault.Detail)
	}
}

func TestExpectDoneAutoAcceptsFinishReason(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t,
		func(r *config.Route) { r.ExpectDone = "auto" },
		testutil.Script{
			Ending:       testutil.EndCleanNoDone,
			FinishReason: "stop",
			Usage:        true,
			SSE:          []testutil.Event{{Raw: `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`}},
		},
	)

	if snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`); snap.Fault != nil {
		t.Fatalf("auto should accept a terminal finish_reason, got %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
}

// A HEAD response declares the length of a body it deliberately does not send.
// Judging it short would invent a truncation verdict — and, because truncation
// aborts the connection, break the client at the same time.
func TestHeadResponseIsNotReportedAsTruncated(t *testing.T) {
	t.Parallel()
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
	})
	h := newHarnessWithUpstream(t, upstream)

	snap := h.do(t, "HEAD", "/vendor/v1/models", "")

	if snap.Fault != nil {
		t.Fatalf("a HEAD response was reported as broken: %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
}

func TestNoContentResponseIsNotReportedAsTruncated(t *testing.T) {
	t.Parallel()
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusNoContent)
	})
	h := newHarnessWithUpstream(t, upstream)

	if snap := h.do(t, "GET", "/vendor/v1/thing", ""); snap.Fault != nil {
		t.Fatalf("a 204 was reported as broken: %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}
}

// The proxy's contract is to pass bytes through unaltered. A body larger than
// the retained copy must still reach the client in full, under the
// Content-Length the upstream declared.
func TestLargeErrorBodyIsForwardedInFull(t *testing.T) {
	t.Parallel()
	const size = 200 << 10 // comfortably over the 64 KiB retained cap
	payload := strings.Repeat("E", size)

	h := newHarnessTuned(t, withRetry(2, time.Minute), testutil.Script{
		Status: http.StatusServiceUnavailable,
		Body:   payload,
	})

	resp, err := http.Post("http://"+h.addr+"/vendor/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if readErr != nil {
		t.Fatalf("reading the forwarded body failed: %v", readErr)
	}
	if len(body) != size {
		t.Errorf("client received %d bytes, want %d — the proxy truncated the body "+
			"while still advertising the upstream's Content-Length", len(body), size)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != "204800" {
		t.Errorf("Content-Length = %q, want it to match what was actually sent", cl)
	}

	snap := h.waitForRecord(t, 5*time.Second)
	if snap.BytesToClient != int64(size) {
		t.Errorf("report says %d bytes delivered, want %d", snap.BytesToClient, size)
	}
}

// The vendor's words must survive into the report even when the client asked
// for gzip — which Python's httpx, and so the OpenAI SDK, does by default.
func TestGzippedErrorBodyIsReadableInTheReport(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withRetry(2, time.Minute), testutil.Script{
		Status: http.StatusTooManyRequests,
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
		Body:   gzipString(t, `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`),
	})

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	requireKind(t, snap, fault.KindRateLimited)
	if !strings.Contains(snap.Fault.Detail, "code=rate_limit_exceeded") {
		t.Errorf("Detail = %q, want the vendor's code decoded rather than left as gzip bytes",
			snap.Fault.Detail)
	}
	last := snap.Attempts[len(snap.Attempts)-1]
	if strings.Contains(string(last.ErrorBody), "\x1f\x8b") {
		t.Error("the retained error body is still gzip-compressed, so the console would print binary")
	}
}

// An over-cap request body disables retry but must still reach the upstream
// intact — and must not be buffered in memory, which is what the cap is for.
func TestOverCapRequestBodyIsForwardedAndDisablesRetry(t *testing.T) {
	t.Parallel()
	const size = 256 << 10
	payload := `{"model":"m","pad":"` + strings.Repeat("p", size) + `"}`

	var received int
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = int(n)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	})

	h := newHarnessWithUpstream(t, upstream, func(r *config.Route) {
		r.MaxRequestBody = config.Bytes(64 << 10)
		withRetry(3, time.Minute)(r)
	})

	snap := h.chat(t, payload)

	if received != len(payload) {
		t.Errorf("upstream received %d bytes, want the full %d", received, len(payload))
	}
	if snap.BodyReplayable {
		t.Error("an over-cap body cannot be replayed, so retry must be marked unavailable")
	}
	if !hasWarningText(snap.Warnings, "max_request_body") {
		t.Errorf("warnings = %+v, want the cap explained rather than silently applied", snap.Warnings)
	}
}

// The plan requires this on any 401/403: it is the line that resolves most
// authentication mysteries in one read.
func TestUnauthorisedExplainsWhereCredentialsCameFrom(t *testing.T) {
	t.Parallel()
	h := newHarnessWithUpstream(t,
		newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
		}),
		func(r *config.Route) { r.APIKeyEnv = "" },
	)

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if snap.KeySource != "none" {
		t.Errorf("KeySource = %q, want none recorded", snap.KeySource)
	}
	out := h.srv.log.Renderer().Render(snap)
	if !strings.Contains(out, "auth") || !strings.Contains(out, "no Authorization") {
		t.Errorf("a 401 report must say where the credentials came from:\n%s", out)
	}
}

func TestClientRequestIDIsEchoed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{Status: http.StatusOK, Body: "{}"})

	req, _ := http.NewRequest("POST", "http://"+h.addr+"/vendor/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("X-Request-Id", "harness-abc-123")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := resp.Header.Get("X-Request-Id"); got != "harness-abc-123" {
		t.Errorf("X-Request-Id = %q, want it echoed so the harness can correlate", got)
	}
	if snap := h.waitForRecord(t, 3*time.Second); snap.ClientRequestID != "harness-abc-123" {
		t.Errorf("ClientRequestID = %q, want it recorded", snap.ClientRequestID)
	}
}

func gzipString(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The connection-reuse plumbing runs from httptrace through readState into the
// classifier; only an end-to-end request proves it is connected.
func TestConnectionReuseIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		testutil.Script{Status: http.StatusOK, Body: `{"id":"1"}`},
		testutil.Script{Status: http.StatusOK, Body: `{"id":"2"}`},
	)

	client := &http.Client{}
	for i := 0; i < 2; i++ {
		resp, err := client.Post("http://"+h.addr+"/vendor/v1/chat/completions",
			"application/json", strings.NewReader(`{"model":"m","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	records := h.waitForRecords(t, 2, 5*time.Second)
	second := records[1]
	if len(second.Attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	if !second.Attempts[0].Conn.Reused {
		t.Error("the second request did not reuse the pooled connection, so the " +
			"reuse plumbing that distinguishes a stale keep-alive from an outage is unverified")
	}
}
