package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/obs"
	"github.com/peterhoneder/llm-proxy/internal/record"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

// harness runs a real proxy against a fake vendor over real TCP.
//
// Nothing here is mocked below the HTTP layer. The failures this tool exists to
// diagnose — a reset, a truncated chunk, a client that stops reading — only
// exist at the socket, so testing anything less would test the wrong thing.
type harness struct {
	srv   *Server
	route *config.Route
	addr  string

	upstream *testutil.Upstream

	// onGapWarn, when set, fires each time the proxy reports that it is still
	// waiting. Used to prove a long wait reports progress repeatedly.
	onGapWarn func()

	mu      sync.Mutex
	records []record.Snapshot
	got     chan struct{}
}

func newHarness(t *testing.T, scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewUpstream(t, scripts...)
	h := newHarnessWithUpstream(t, up.URL)
	h.upstream = up
	return h
}

// newHarnessTuned is newHarness with a chance to adjust the route before the
// server is built, for tests that need retry enabled or a different timeout.
func newHarnessTuned(t *testing.T, tune func(*config.Route), scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewUpstream(t, scripts...)
	h := newHarnessWithUpstream(t, up.URL, tune)
	h.upstream = up
	return h
}

// newRawUpstream starts a plain httptest server for cases that need a handler
// rather than a script.
func newRawUpstream(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newHarnessWithAuth builds a harness whose listener requires a proxy token.
func newHarnessWithAuth(t *testing.T, cfgTune func(*config.Config), scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewUpstream(t, scripts...)
	h := newHarnessCfg(t, up.URL, cfgTune)
	h.upstream = up
	return h
}

func newHarnessWithUpstreamAndAuth(
	t *testing.T, upstream string, cfgTune func(*config.Config), tune ...func(*config.Route),
) *harness {
	t.Helper()
	return newHarnessCfg(t, upstream, cfgTune, tune...)
}

func newHarnessWithUpstream(t *testing.T, upstream string, tune ...func(*config.Route)) *harness {
	t.Helper()
	return newHarnessCfg(t, upstream, nil, tune...)
}

// newHarnessCfg is the single construction path. cfgTune adjusts the whole
// config (auth, for instance); tune adjusts the one route.
func newHarnessCfg(
	t *testing.T, upstream string, cfgTune func(*config.Config), tune ...func(*config.Route),
) *harness {
	t.Helper()

	// DefaultConfig rather than Load: the embedded defaults declare no routes,
	// so Load would (correctly) refuse them as an unusable configuration.
	cfg, err := config.DefaultConfig()
	if err != nil {
		t.Fatalf("loading defaults: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	// Hermetic: a developer with LLM_PROXY_TOKENS exported must still be able
	// to run the suite.
	cfg.Auth.Enabled = "false"
	cfg.Log.Level = "debug"
	cfg.Log.Color = "never"
	cfg.Routes = []config.Route{{
		Name:     "vendor",
		Upstream: upstream,
	}}
	// Timeouts are shortened so a hang fails a test in under a second rather
	// than after a minute.
	cfg.Defaults.Timeouts.StreamIdle = dur(500 * time.Millisecond)
	cfg.Defaults.Timeouts.ClientWrite = dur(2 * time.Second)
	cfg.Defaults.Timeouts.ResponseHeader = dur(5 * time.Second)
	cfg.Defaults.Timeouts.GapWarn = dur(200 * time.Millisecond)
	// Routes are built in Go here, so they start with nothing set and inherit
	// everything above.
	cfg.Routes[0].Timeouts = config.Timeouts{}
	applyDefaultsForTest(cfg)
	if cfgTune != nil {
		cfgTune(cfg)
	}
	for _, fn := range tune {
		fn(&cfg.Routes[0])
	}

	h := &harness{got: make(chan struct{}, 16)}

	log := obs.NewLogger(obs.Options{Cfg: cfg.Log, Out: io.Discard})
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatalf("building server: %v", err)
	}
	h.srv = srv
	h.route = &cfg.Routes[0]

	// Capture every completed record so tests can assert on the verdict the
	// operator would have seen.
	srv.onGapWarn = func() {
		if h.onGapWarn != nil {
			h.onGapWarn()
		}
	}

	srv.onRecord = func(snap record.Snapshot) {
		h.mu.Lock()
		h.records = append(h.records, snap)
		h.mu.Unlock()
		select {
		case h.got <- struct{}{}:
		default:
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	h.addr = ln.Addr().String()

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ServeListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})

	return h
}

// authedChat issues a request carrying a proxy credential.
func (h *harness) authedChat(t *testing.T, authorization, body string) record.Snapshot {
	t.Helper()

	req, err := http.NewRequest("POST", "http://"+h.addr+"/vendor/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)

	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return h.waitForRecord(t, 5*time.Second)
}

// chat issues a request with an ordinary http.Client and returns the record.
func (h *harness) chat(t *testing.T, body string) record.Snapshot {
	t.Helper()
	return h.do(t, "POST", "/vendor/v1/chat/completions", body)
}

func (h *harness) do(t *testing.T, method, path, body string) record.Snapshot {
	t.Helper()

	req, err := http.NewRequest(method, "http://"+h.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	// A dedicated client so a pooled connection from another test cannot
	// influence the connection-reuse fields under assertion.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return h.waitForRecord(t, 5*time.Second)
}

// waitForRecord blocks until the proxy finishes a request and reports it.
func (h *harness) waitForRecord(t *testing.T, timeout time.Duration) record.Snapshot {
	t.Helper()
	deadline := time.After(timeout)
	for {
		h.mu.Lock()
		n := len(h.records)
		var snap record.Snapshot
		if n > 0 {
			snap = h.records[n-1]
		}
		h.mu.Unlock()
		if n > 0 {
			return snap
		}
		select {
		case <-h.got:
		case <-deadline:
			t.Fatal("timed out waiting for the proxy to report a request")
		}
	}
}

// waitForRecords blocks until at least n requests have been reported.
func (h *harness) waitForRecords(t *testing.T, n int, timeout time.Duration) []record.Snapshot {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if got := h.all(); len(got) >= n {
			return got
		}
		select {
		case <-h.got:
		case <-deadline:
			t.Fatalf("timed out waiting for %d reported requests, got %d", n, len(h.all()))
		}
	}
}

// records returns every captured record.
func (h *harness) all() []record.Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]record.Snapshot, len(h.records))
	copy(out, h.records)
	return out
}

func dur(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}

// applyDefaultsForTest mirrors what config.Load does after decoding, which the
// harness bypasses by building routes in Go.
func applyDefaultsForTest(cfg *config.Config) {
	cfg.ApplyRouteDefaultsForTest()
}
