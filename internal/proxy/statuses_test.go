package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

const contextLengthBody = `{"error":{"message":"This model's maximum context length is 128000 tokens. ` +
	`However, your messages resulted in 131204 tokens.","type":"invalid_request_error",` +
	`"param":"messages","code":"context_length_exceeded"}}`

func TestContextLengthExceeded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Status: http.StatusBadRequest,
		Body:   contextLengthBody,
	})

	snap := h.chat(t, `{"model":"m","max_tokens":8192,"messages":[{"role":"user","content":"..."}]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindContextLength)

	// The vendor's own wording is the most useful thing on the screen, so it
	// has to survive into the report intact.
	if !strings.Contains(snap.Fault.Detail, "maximum context length is 128000") {
		t.Errorf("Detail = %q, want the vendor's message", snap.Fault.Detail)
	}
	if !strings.Contains(snap.Fault.Detail, "code=context_length_exceeded") {
		t.Errorf("Detail = %q, want the vendor's error code", snap.Fault.Detail)
	}
	// Naming the pattern that fired is what makes a false positive diagnosable
	// rather than mysterious.
	if !strings.Contains(snap.Fault.Detail, "matched") {
		t.Errorf("Detail = %q, want the matching rule named", snap.Fault.Detail)
	}
	if snap.MaxTokens == nil || *snap.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %v, want the request's own limit recorded alongside", snap.MaxTokens)
	}
}

// An ordinary 4xx must not be swept up by the context-length matcher.
func TestOtherClientErrorsAreNotContextLength(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Status: http.StatusUnauthorized,
		Body:   `{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`,
	})

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindHTTPStatus)
	if !strings.Contains(snap.Fault.Detail, "Incorrect API key") {
		t.Errorf("Detail = %q, want the vendor's message verbatim", snap.Fault.Detail)
	}
}

// The protocol behaved correctly, so this is a warning rather than a fault —
// but the answer is still cut off and saying nothing would be worse.
func TestOutputTruncatedByMaxTokens(t *testing.T) {
	t.Parallel()
	s := testutil.StreamOf(3, testutil.EndDone)
	s.FinishReason = "length"
	h := newHarness(t, s)

	snap := h.chat(t, `{"model":"m","stream":true,"max_tokens":4096,"messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("a completed stream must not be a fault: %s", snap.Fault.Kind)
	}
	if !hasWarningKind(snap.Warnings, fault.KindOutputTruncated) {
		t.Errorf("warnings = %+v, want an output-truncated warning", snap.Warnings)
	}
	if len(snap.Stream.FinishReasons) == 0 || snap.Stream.FinishReasons[0] != "length" {
		t.Errorf("FinishReasons = %v, want [length]", snap.Stream.FinishReasons)
	}
}

func TestContentFilterWarns(t *testing.T) {
	t.Parallel()
	s := testutil.StreamOf(1, testutil.EndDone)
	s.FinishReason = "content_filter"
	h := newHarness(t, s)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	if !hasWarningKind(snap.Warnings, fault.KindContentFilter) {
		t.Errorf("warnings = %+v, want a content-filter warning", snap.Warnings)
	}
}

// Streams omit usage unless the client opts in. Silently reporting nothing
// would read as a proxy failure, so the hint says what to change.
func TestMissingUsageIsExplained(t *testing.T) {
	t.Parallel()
	s := testutil.StreamOf(2, testutil.EndDone)
	s.Usage = false
	h := newHarness(t, s)

	snap := h.chat(t, `{"model":"m","stream":true,"messages":[]}`)

	var explained bool
	for _, w := range snap.Warnings {
		if strings.Contains(w.Text, "include_usage") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("warnings = %+v, want the missing-usage hint", snap.Warnings)
	}
}

func TestUsageNotFlaggedWhenClientOptedIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(2, testutil.EndDone))

	snap := h.chat(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)

	for _, w := range snap.Warnings {
		if strings.Contains(w.Text, "include_usage") {
			t.Errorf("the hint should not fire when usage was requested and reported: %+v", snap.Warnings)
		}
	}
	if snap.Stream.Usage == nil {
		t.Error("usage should have been captured")
	}
}

// A non-streaming body cut short mid-JSON is the same failure as a cut stream,
// and just as easy to miss.
func TestNonStreamingTruncatedBody(t *testing.T) {
	t.Parallel()
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-9","choices":[{"index":0,"message":{"content":"cut o`)
	})
	h := newHarnessWithUpstream(t, upstream)

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindTruncatedBody)
}

// The same truncation, but with the client hanging up the instant it has the
// body — which is what every client without keep-alives does on every request.
//
// The client watcher stays armed until the handler returns, so its stamp can
// land while the verdict is still being drawn. It must not change the answer:
// the client took every byte the upstream sent, so it interrupted nothing, and
// the vendor's body still ended mid-JSON. Attribution here is decided by
// undelivered bytes, not by which goroutine got there first.
func TestClientHangUpAfterTruncatedBodyStillBlamesUpstream(t *testing.T) {
	t.Parallel()
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-9","choices":[{"index":0,"message":{"content":"cut o`)
	})
	h := newHarnessWithUpstream(t, upstream)

	c := testutil.Dial(t, h.addr)
	c.Send("POST", "/vendor/v1/chat/completions", "localhost", `{"model":"m","messages":[]}`)
	c.ReadStatusLine()
	c.ReadHeaders()
	c.ReadSome(time.Second)
	c.HangUp()

	snap := h.waitForRecord(t, 3*time.Second)
	requireSide(t, snap, fault.SideUpstream)
	requireKind(t, snap, fault.KindTruncatedBody)
	if snap.Fault.Induced {
		t.Error("a client that received the whole body cannot have induced the truncation")
	}
}

func TestNonStreamingCompleteResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{
		Status: http.StatusOK,
		Body: `{"id":"cmpl-1","model":"m","choices":[{"index":0,"finish_reason":"stop",` +
			`"message":{"role":"assistant","content":"hello"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	})

	snap := h.chat(t, `{"model":"m","messages":[]}`)

	if snap.Fault != nil {
		t.Fatalf("unexpected fault: %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}
	if snap.Stream == nil || snap.Stream.Usage == nil || snap.Stream.Usage.TotalTokens != 15 {
		t.Errorf("usage not captured from a non-streaming response: %+v", snap.Stream)
	}
}

func hasWarningKind(warnings []record.Warning, kind fault.Kind) bool {
	for _, w := range warnings {
		if w.Kind == kind {
			return true
		}
	}
	return false
}
