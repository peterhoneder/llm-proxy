package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

// These tests are the product thesis. Everything else in llm-proxy exists so
// that when a stream dies, the report names the right side — so each case here
// reproduces one real failure and asserts the attribution.
//
// They assert on Side and on a set of acceptable Kinds, never on one exact
// Kind. Whether a TCP reset surfaces as ECONNRESET or as a plain EOF depends on
// kernel timing and differs between darwin and linux; pinning the exact value
// would buy nothing and flake in CI.

func TestCleanStreamHasNoFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(5, testutil.EndDone))

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if snap.Fault != nil {
		t.Fatalf("clean stream produced a fault: %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}
	if snap.Status != 200 {
		t.Errorf("Status = %d, want 200", snap.Status)
	}
	if !snap.Stream.DoneSeen {
		t.Error("the [DONE] sentinel was not observed")
	}
	if snap.Stream.Usage == nil || snap.Stream.Usage.InputTokens != 1042 {
		t.Errorf("usage not captured: %+v", snap.Stream.Usage)
	}
	if snap.BytesToClient == 0 || snap.BytesToClient != snap.BytesFromUpstream {
		t.Errorf("bytes read=%d written=%d, want them equal and non-zero",
			snap.BytesFromUpstream, snap.BytesToClient)
	}
}

func TestUpstreamTruncatesMidFrame(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(4, testutil.EndTruncateMidFrame))

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindTruncatedBody, fault.KindTruncatedStream, fault.KindUpstreamReset)
	if snap.BytesToClient == 0 {
		t.Error("the bytes that did arrive should still have been forwarded")
	}
}

// A backend that ends the body correctly but never says the answer finished.
func TestUpstreamCleanEOFWithoutFinish(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending: testutil.EndCleanNoDone,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`},
		},
	})

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindTruncatedStream)
}

// The false-positive guard. vLLM, llama.cpp and several gateways never send
// `data: [DONE]`; treating that as truncation would flag every healthy request
// against them, and a tool that cries wolf stops being consulted.
func TestNoDoneSentinelButFinishReasonIsClean(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending:       testutil.EndCleanNoDone,
		FinishReason: "stop",
		Usage:        true,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`},
		},
	})

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("a backend that omits [DONE] but sends finish_reason must not be reported "+
			"as truncated, got %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}
	if !snap.Stream.FinishSeen {
		t.Error("finish_reason was not recorded")
	}
}

func TestUpstreamResetsConnection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending: testutil.EndReset,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`},
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"b"}}]}`},
		},
	})

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindUpstreamReset, fault.KindTruncatedBody, fault.KindTruncatedStream)
}

// The vendor goes silent mid-answer. The proxy enforces the deadline, but the
// fault is the vendor's — reporting SideProxy here would be precisely the
// misdiagnosis this tool exists to prevent.
func TestUpstreamStallIsBlamedOnTheVendor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending:  testutil.EndHang,
		HangFor: 3 * time.Second,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`},
		},
	})
	h.route.Timeouts.StreamIdle = dur(150 * time.Millisecond)
	h.route.Timeouts.GapWarn = dur(50 * time.Millisecond)

	start := time.Now()
	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)
	elapsed := time.Since(start)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindStallTimeout, fault.KindReadTimeout)
	if elapsed > 2*time.Second {
		t.Errorf("the watchdog took %v to fire; it should resolve the hang promptly", elapsed)
	}
	if snap.BytesToClient == 0 {
		t.Error("the event delivered before the stall should still have reached the client")
	}
	if !strings.Contains(snap.Fault.Detail, "stream_idle") {
		t.Errorf("Detail = %q, want it to name the setting that fired", snap.Fault.Detail)
	}
}

// The single most important case in the tool, and the one a naive
// implementation gets wrong: the client leaves while the proxy is blocked in a
// read, so no write ever fails and there is nothing to classify from.
func TestClientDisconnectsWhileUpstreamIsSilent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending:  testutil.EndHang,
		HangFor: 3 * time.Second,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`},
		},
	})
	h.route.Timeouts.StreamIdle = dur(5 * time.Second) // long enough that the watchdog cannot be the cause

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost",
		`{"model":"m","stream":true,"messages":[]}`)
	c.ReadStatusLine()
	c.ReadHeaders()
	c.ReadSome(time.Second) // the first event

	c.HangUp()

	snap := h.waitForRecord(t, 3*time.Second)
	requireSide(t, snap, fault.SideClient)
	if snap.Fault.Induced {
		t.Error("the verdict must not be an induced fault")
	}
	if snap.ClientGoneAt.IsZero() {
		t.Error("the client watcher did not record when the connection went away")
	}
}

func TestClientAbortsMidStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, slowStream(20, 40*time.Millisecond))

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost",
		`{"model":"m","stream":true,"messages":[]}`)
	c.ReadStatusLine()
	c.ReadHeaders()
	c.ReadSome(time.Second)
	c.Abort()

	snap := h.waitForRecord(t, 5*time.Second)
	requireSide(t, snap, fault.SideClient)
	requireKind(t, snap,
		fault.KindClientDisconnect, fault.KindClientEPIPE, fault.KindClientReset, fault.KindClientWriteFailed)
}

// The upstream completed normally, but the client had already gone: every write
// succeeded into the kernel buffer and the answer was never actually received.
// Reporting this as a success would be wrong in the way that matters most.
func TestClientGoneButUpstreamCompleted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(2, testutil.EndDone))

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost",
		`{"model":"m","stream":true,"messages":[]}`)
	c.HangUp()

	snap := h.waitForRecord(t, 3*time.Second)
	if snap.Fault == nil {
		t.Fatal("a completed response the client never received must not be reported as clean")
	}
	requireSide(t, snap, fault.SideClient)
}

func TestConnectionRefusedIsAnUpstreamFault(t *testing.T) {
	t.Parallel()
	// A port with nothing behind it: bind then immediately release.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()

	h := newHarnessWithUpstream(t, "http://"+dead)
	snap := h.chat(t, `{"model":"m","messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindConnRefused, fault.KindUnreachable, fault.KindUpstreamUnknown)
	if snap.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502 reported to the client", snap.Status)
	}
}

// HTTP 200, then an error frame. The status line already said everything was
// fine, so nothing else in the pipeline would notice.
func TestErrorInsideStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending: testutil.EndDone,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`},
			{Raw: `data: {"error":{"message":"upstream model crashed","type":"server_error"}}`},
		},
	})

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindErrorInStream)
	if !strings.Contains(snap.Fault.Detail, "upstream model crashed") {
		t.Errorf("Detail = %q, want the vendor's message verbatim", snap.Fault.Detail)
	}
}

// Shutting the proxy down cuts in-flight streams. That is the operator's doing
// and must never be attributed to the vendor.
func TestShutdownIsAProxyFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Ending:  testutil.EndHang,
		HangFor: 5 * time.Second,
		SSE: []testutil.Event{
			{Raw: `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`},
		},
	})
	h.route.Timeouts.StreamIdle = dur(10 * time.Second)

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost",
		`{"model":"m","stream":true,"messages":[]}`)
	c.ReadStatusLine()
	c.ReadHeaders()
	c.ReadSome(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _ = h.srv.Shutdown(ctx) }()

	snap := h.waitForRecord(t, 5*time.Second)
	requireSide(t, snap, fault.SideProxy)
	requireKind(t, snap, fault.KindProxyShutdown)
}

// A gzip-encoded stream, which is what a Python client gets by default because
// httpx and requests send Accept-Encoding: gzip. Without decoding a copy for
// analysis, the analyzer would see compressed bytes, find no finish_reason and
// report every healthy request as truncated.
func TestGzipEncodedStreamIsStillUnderstood(t *testing.T) {
	t.Parallel()
	s := testutil.StreamOf(3, testutil.EndDone)
	s.Gzip = true
	h := newHarness(t, s)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("a gzip-encoded healthy stream was reported as broken: %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
	if !snap.Stream.DoneSeen {
		t.Error("the analyzer did not see through the gzip encoding")
	}
}

// An encoding we cannot decode must produce no verdict at all rather than a
// confident wrong one.
func TestUndecodableEncodingMakesNoTruncationClaim(t *testing.T) {
	t.Parallel()
	s := testutil.StreamOf(2, testutil.EndCleanNoDone)
	s.Header = http.Header{"Content-Encoding": []string{"br"}}
	h := newHarness(t, s)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("an undecodable body must not yield a truncation verdict, got %s — %s",
			snap.Fault.Kind, snap.Fault.Detail)
	}
	if snap.Stream.AnalysisUnavailable == "" {
		t.Error("the report should say analysis was not possible")
	}
}

// Proves the proxy is not buffering. It is gated on a channel rather than timed,
// so it fails only on a real regression and never on a slow machine.
func TestNoBuffering(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once

	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)

		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		fl.Flush()

		<-release // the second event is withheld until the test says so

		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	h := newHarnessWithUpstream(t, upstream)

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost",
		`{"model":"m","stream":true,"messages":[]}`)
	c.ReadStatusLine()
	c.ReadHeaders()

	// The first event must reach the client while the upstream is still
	// holding the second. If the proxy buffered, this read would block until
	// the test released the upstream, which never happens first.
	got, ok := c.ReadUntil("first", 2*time.Second)
	once.Do(func() { close(release) })
	if !ok {
		t.Fatalf("the first event did not reach the client before the second was sent — "+
			"the proxy is buffering. Read so far: %q", got)
	}

	h.waitForRecord(t, 3*time.Second)
}

func requireSide(t *testing.T, snap record.Snapshot, want fault.Side) {
	t.Helper()
	if snap.Fault == nil {
		t.Fatalf("expected a %s fault, got a clean request", want)
	}
	if snap.Fault.Side != want {
		t.Fatalf("Side = %s (kind=%s), want %s\n  detail: %s\n  verdict: %s",
			snap.Fault.Side, snap.Fault.Kind, want, snap.Fault.Detail, snap.Fault.Verdict)
	}
	if snap.Fault.Verdict == "" {
		t.Error("every fault must carry a verdict; that line is what the operator reads")
	}
}

// requireKind accepts a set, because the exact errno a torn connection produces
// depends on kernel timing and differs across platforms.
func requireKind(t *testing.T, snap record.Snapshot, want ...fault.Kind) {
	t.Helper()
	if snap.Fault == nil {
		t.Fatalf("expected one of %v, got a clean request", want)
	}
	for _, k := range want {
		if snap.Fault.Kind == k {
			return
		}
	}
	t.Fatalf("Kind = %s, want one of %v\n  detail: %s", snap.Fault.Kind, want, snap.Fault.Detail)
}

func slowStream(n int, gap time.Duration) testutil.Script {
	s := testutil.StreamOf(n, testutil.EndDone)
	for i := range s.SSE {
		s.SSE[i].Delay = gap
	}
	return s
}
