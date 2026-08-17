package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/obs"
)

func TestStripHopByHop(t *testing.T) {
	t.Parallel()
	h := http.Header{
		"Connection":       []string{"keep-alive, X-Internal-Token"},
		"Keep-Alive":       []string{"timeout=5"},
		"Upgrade":          []string{"websocket"},
		"Proxy-Connection": []string{"keep-alive"},
		"Te":               []string{"trailers"},
		"X-Internal-Token": []string{"secret"},
		"Authorization":    []string{"Bearer sk-keep-me"},
		"Content-Type":     []string{"application/json"},
	}

	stripHopByHop(h)

	// The Connection header names further headers to drop, and forwarding one
	// it named would leak something the sender explicitly asked us not to pass
	// on. Handling only the static list is the usual bug here.
	for _, gone := range []string{"Connection", "Keep-Alive", "Upgrade", "Proxy-Connection", "Te", "X-Internal-Token"} {
		if h.Get(gone) != "" {
			t.Errorf("%s survived the hop-by-hop strip", gone)
		}
	}
	if h.Get("Authorization") != "Bearer sk-keep-me" {
		t.Error("end-to-end headers must be preserved")
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("Content-Type must be preserved")
	}
}

func TestCopyHeadersDoesNotMutateTheOriginal(t *testing.T) {
	t.Parallel()
	src := http.Header{"Connection": []string{"keep-alive"}, "Accept": []string{"text/event-stream"}}
	dst := copyHeaders(src)

	if dst.Get("Connection") != "" {
		t.Error("the copy should have hop-by-hop headers removed")
	}
	// The record still needs to show what the client actually sent.
	if src.Get("Connection") == "" {
		t.Error("the source headers were mutated")
	}
	if dst.Get("Accept") != "text/event-stream" {
		t.Error("end-to-end headers should be copied")
	}
}

func TestApplyAuth(t *testing.T) {
	t.Parallel()
	route := &config.Route{}
	route.APIKeyHeader = "Authorization"
	route.APIKeyPrefix = "Bearer "

	t.Run("injects the configured key", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		if got := applyAuth(h, route, "sk-configured"); got != obs.KeyInjected {
			t.Errorf("source = %v, want injected", got)
		}
		if h.Get("Authorization") != "Bearer sk-configured" {
			t.Errorf("Authorization = %q", h.Get("Authorization"))
		}
	})

	t.Run("replaces a client key by default", func(t *testing.T) {
		t.Parallel()
		h := http.Header{"Authorization": []string{"Bearer sk-from-client"}}
		if got := applyAuth(h, route, "sk-configured"); got != obs.KeyInjected {
			t.Errorf("source = %v, want injected", got)
		}
	})

	t.Run("forwards the client key when asked", func(t *testing.T) {
		t.Parallel()
		fwd := *route
		yes := true
		fwd.ForwardClientAuth = &yes

		h := http.Header{"Authorization": []string{"Bearer sk-from-client"}}
		if got := applyAuth(h, &fwd, "sk-configured"); got != obs.KeyClientSupplied {
			t.Errorf("source = %v, want client-supplied", got)
		}
		if h.Get("Authorization") != "Bearer sk-from-client" {
			t.Error("the client's own key must survive when forwarding is enabled")
		}
	})

	t.Run("reports when there are no credentials at all", func(t *testing.T) {
		t.Parallel()
		h := http.Header{}
		if got := applyAuth(h, route, ""); got != obs.KeyNone {
			t.Errorf("source = %v, want none — this is what explains a 401", got)
		}
	})

	t.Run("keeps the client key when none is configured", func(t *testing.T) {
		t.Parallel()
		h := http.Header{"Authorization": []string{"Bearer sk-from-client"}}
		if got := applyAuth(h, route, ""); got != obs.KeyClientSupplied {
			t.Errorf("source = %v, want client-supplied", got)
		}
	})
}

func TestPrepareResponseHeaders(t *testing.T) {
	t.Parallel()
	src := http.Header{
		"Content-Type":     []string{"text/event-stream"},
		"Connection":       []string{"keep-alive"},
		"X-Ratelimit-Left": []string{"7"},
	}

	dst := http.Header{}
	prepareResponseHeaders(dst, src, false)

	if dst.Get("Connection") != "" {
		t.Error("upstream hop-by-hop headers must not be echoed downstream")
	}
	if dst.Get("X-Ratelimit-Left") != "7" {
		t.Error("vendor headers must reach the client verbatim")
	}
}

// When Go's transport decompressed the body, the declared length describes
// bytes the client will never see.
func TestPrepareResponseHeadersDropsLengthWhenDecompressed(t *testing.T) {
	t.Parallel()
	src := http.Header{
		"Content-Length":   []string{"120"},
		"Content-Encoding": []string{"gzip"},
	}
	dst := http.Header{}
	prepareResponseHeaders(dst, src, true)

	if dst.Get("Content-Length") != "" || dst.Get("Content-Encoding") != "" {
		t.Errorf("stale length/encoding survived: %+v", dst)
	}
}

// The regression guard for the single most damaging silent failure available
// here: a ResponseWriter wrapper without Unwrap makes every Flush fail, the
// proxy buffers the stream, and every timing it reports becomes fiction.
func TestResponseWriterWrapperIsFlushable(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: rec}

	if err := http.NewResponseController(cw).Flush(); err != nil {
		t.Fatalf("Flush through the wrapper failed: %v — countingWriter is missing "+
			"Unwrap() http.ResponseWriter, so responses are being buffered", err)
	}
}

func TestCountingWriterCountsAndRecordsStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: rec}

	cw.WriteHeader(http.StatusTeapot)
	n, err := io.WriteString(cw, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || cw.n != 5 {
		t.Errorf("wrote %d bytes, counter = %d, want 5 and 5", n, cw.n)
	}
	if cw.status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", cw.status, http.StatusTeapot)
	}
}

// Hop-by-hop stripping and auth injection have to hold end to end, not just in
// isolation.
func TestHeadersEndToEnd(t *testing.T) {
	t.Parallel()
	var seen http.Header
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Vendor-Detail", "kept")
		_, _ = io.WriteString(w, "{}")
	})
	h := newHarnessWithUpstream(t, upstream, func(r *config.Route) {
		r.APIKeyEnv = "" // no configured key; the client's own must pass through
	})

	req, _ := http.NewRequest("POST", "http://"+h.addr+"/vendor/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer sk-client-key")
	req.Header.Set("X-Custom", "kept")
	req.Header.Set("Connection", "keep-alive, X-Dropped")
	req.Header.Set("X-Dropped", "should not arrive")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	h.waitForRecord(t, 3*time.Second)

	if seen.Get("Authorization") != "Bearer sk-client-key" {
		t.Errorf("upstream Authorization = %q, want the client's key forwarded", seen.Get("Authorization"))
	}
	if seen.Get("X-Custom") != "kept" {
		t.Error("an end-to-end header was dropped")
	}
	if seen.Get("X-Dropped") != "" {
		t.Error("a header named by Connection reached the upstream")
	}
	if resp.Header.Get("X-Vendor-Detail") != "kept" {
		t.Error("a vendor response header did not reach the client")
	}
	if resp.Header.Get("X-LLM-Proxy-Request-Id") == "" {
		t.Error("the request id must be returned so harness logs can be correlated")
	}
}
