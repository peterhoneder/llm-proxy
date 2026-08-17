package obs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/analyze"
	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

const secretKey = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"

func TestFingerprintNeverRevealsTheSecret(t *testing.T) {
	t.Parallel()
	got := Fingerprint("Bearer " + secretKey)

	if strings.Contains(got, secretKey) {
		t.Fatalf("Fingerprint leaked the key: %q", got)
	}
	// Enough to answer "is it the key I think it is" without exposing it.
	for _, want := range []string{"Bearer", "sha256:", "len="} {
		if !strings.Contains(got, want) {
			t.Errorf("Fingerprint = %q, want it to include %q", got, want)
		}
	}
	if Fingerprint("") != "(empty)" {
		t.Errorf("an empty value should be reported as empty, got %q", Fingerprint(""))
	}
	// Different keys must be distinguishable across runs.
	if Fingerprint("sk-a-different-key-entirely") == got {
		t.Error("two different keys produced the same fingerprint")
	}
}

func TestShortSecretsAreNotPartiallyRevealed(t *testing.T) {
	t.Parallel()
	got := Fingerprint("tiny")
	if strings.Contains(got, "tiny") {
		t.Errorf("Fingerprint = %q, want a short secret fully hidden", got)
	}
}

func TestRedactorHidesConfiguredHeaders(t *testing.T) {
	t.Parallel()
	r := NewRedactor([]string{"authorization", "x-api-key", "cookie"}, false)

	if got := r.Value("Authorization", "Bearer "+secretKey); strings.Contains(got, secretKey) {
		t.Errorf("Authorization was not redacted: %q", got)
	}
	if got := r.Value("X-Api-Key", secretKey); strings.Contains(got, secretKey) {
		t.Errorf("X-Api-Key was not redacted: %q", got)
	}
	if got := r.Value("Content-Type", "application/json"); got != "application/json" {
		t.Errorf("an ordinary header was altered: %q", got)
	}
}

func TestUnsafeRevealIsOptIn(t *testing.T) {
	t.Parallel()
	r := NewRedactor([]string{"authorization"}, true)
	if got := r.Value("Authorization", "Bearer "+secretKey); !strings.Contains(got, secretKey) {
		t.Error("unsafe_reveal_secrets should show the value in full when explicitly enabled")
	}
}

// The requirement pulls both ways: nothing may be hidden from the operator, and
// the API key must not reach a third-party collector. This asserts the second
// half across every rendering path and every level.
func TestKeyNeverAppearsInRenderedOutput(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"debug", "info", "warn", "error"} {
		for _, full := range []bool{false, true} {
			var buf bytes.Buffer
			cfg := config.Log{
				Level: level, Format: "pretty", Color: "never", Symbols: "ascii",
				FullTrace:     full,
				MaxBodyBytes:  config.Bytes(64 << 10),
				RedactHeaders: []string{"authorization", "x-api-key"},
			}
			log := NewLogger(Options{Cfg: cfg, Out: &buf})

			snap := sampleSnapshot()
			snap.Attempts[0].SentHeaders = map[string][]string{
				"Authorization": {"Bearer " + secretKey},
			}
			snap.Attempts[0].RespHeaders = map[string][]string{
				"X-Api-Key": {secretKey},
			}
			snap.Fault = fault.New(fault.SideUpstream, fault.KindTruncatedStream, "read", nil)

			log.Request(context.Background(), snap)

			if strings.Contains(buf.String(), secretKey) {
				t.Fatalf("the API key reached the console at level=%s full_trace=%v:\n%s",
					level, full, buf.String())
			}
		}
	}
}

func TestAttrsCarryNoSecrets(t *testing.T) {
	t.Parallel()
	snap := sampleSnapshot()
	snap.Attempts[0].SentHeaders = map[string][]string{"Authorization": {"Bearer " + secretKey}}

	for _, a := range Attrs(snap) {
		if strings.Contains(a.Value.String(), secretKey) {
			t.Fatalf("attribute %s carries the API key — this would be exported to OTLP", a.Key)
		}
	}
}

func TestCleanRequestRendersOneLine(t *testing.T) {
	t.Parallel()
	r := NewRenderer(RendererOptions{Symbols: "ascii", Level: slog.LevelInfo})
	out := r.Render(sampleSnapshot())

	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 0 {
		t.Errorf("a clean request should be one line, got %d:\n%s", n+1, out)
	}
	for _, want := range []string{"vendor", "200 OK", "r-00042"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Every fault block must carry a verdict, the read/write byte split and the
// two "last byte" clocks: that trio is what makes the responsible side obvious
// without reading carefully.
func TestFaultBlockCarriesTheEssentials(t *testing.T) {
	t.Parallel()
	r := NewRenderer(RendererOptions{Symbols: "ascii", Level: slog.LevelInfo})

	snap := sampleSnapshot()
	snap.Fault = fault.New(fault.SideClient, fault.KindClientEPIPE, "write", nil).
		WithDetail("write failed with broken pipe")
	snap.Stream.DoneSeen = false
	snap.BytesToClient = 900

	out := r.Render(snap)

	for _, want := range []string{
		"side=client",
		"kind=client_disconnect_epipe",
		"verdict",
		"last upstream byte",
		"last downstream write",
		"read /",
		"written",
		"CLIENT DISCONNECTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fault block missing %q:\n%s", want, out)
		}
	}
}

func TestVerdictIsAlwaysPresent(t *testing.T) {
	t.Parallel()
	r := NewRenderer(RendererOptions{Symbols: "ascii"})

	for _, kind := range []fault.Kind{
		fault.KindTruncatedStream, fault.KindClientDisconnect, fault.KindStallTimeout,
		fault.KindContextLength, fault.KindRateLimited, fault.KindProxyShutdown,
		fault.KindIdleReuseEOF, fault.KindConnRefused,
	} {
		snap := sampleSnapshot()
		snap.Fault = fault.New(fault.SideUpstream, kind, "read", nil)

		out := r.Render(snap)
		if !strings.Contains(out, "verdict") {
			t.Errorf("%s rendered without a verdict:\n%s", kind, out)
		}
	}
}

// A mangled glyph in a CI log or a terminal with the wrong locale is worse than
// a plain arrow.
func TestAsciiSymbolsAvoidUnicode(t *testing.T) {
	t.Parallel()
	r := NewRenderer(RendererOptions{Symbols: "ascii"})
	snap := sampleSnapshot()
	snap.Fault = fault.New(fault.SideUpstream, fault.KindTruncatedStream, "read", nil)

	out := r.Render(snap)
	for _, glyph := range []string{"→", "←", "✓", "✗", "⚠", "·", "⟳"} {
		if strings.Contains(out, glyph) {
			t.Errorf("ascii mode emitted %q:\n%s", glyph, out)
		}
	}
}

func TestNoColourWhenDisabled(t *testing.T) {
	t.Parallel()
	r := NewRenderer(RendererOptions{Color: false, Symbols: "ascii"})
	snap := sampleSnapshot()
	snap.Fault = fault.New(fault.SideUpstream, fault.KindTruncatedStream, "read", nil)

	if out := r.Render(snap); strings.Contains(out, "\x1b[") {
		t.Errorf("escape sequences leaked into non-colour output:\n%q", out)
	}
}

// Two requests finishing together must not shred each other's multi-line
// reports.
func TestConcurrentBlocksDoNotInterleave(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(Options{
		Cfg: config.Log{Level: "info", Format: "pretty", Color: "never", Symbols: "ascii",
			MaxBodyBytes: config.Bytes(64 << 10)},
		Out: &buf,
	})

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snap := sampleSnapshot()
			snap.Fault = fault.New(fault.SideUpstream, fault.KindTruncatedStream, "read", nil).
				WithDetail("detail line for request %d", i)
			log.Request(context.Background(), snap)
		}(i)
	}
	wg.Wait()

	// Every fault block ends with a verdict; a torn block would leave a
	// header line without one.
	got := buf.String()
	headers := strings.Count(got, "UPSTREAM TRUNCATED")
	verdicts := strings.Count(got, "verdict")
	if headers != 24 || verdicts != 24 {
		t.Errorf("blocks were interleaved: %d headers, %d verdicts, want 24 each\n%s",
			headers, verdicts, got)
	}
}

func TestMultiHandlerFansOutAndIsolatesRecords(t *testing.T) {
	t.Parallel()
	a, b := &captureHandler{}, &captureHandler{}
	logger := slog.New(NewMultiHandler(a, b))

	logger.Info("hello", "k", "v")

	if len(a.records) != 1 || len(b.records) != 1 {
		t.Fatalf("fan-out reached %d and %d handlers, want 1 each", len(a.records), len(b.records))
	}
	// Each child gets its own clone, so one appending attributes cannot
	// corrupt what a sibling sees.
	if a.records[0].NumAttrs() != 1 || b.records[0].NumAttrs() != 1 {
		t.Error("records were not cloned per handler")
	}
}

func TestMultiHandlerRespectsPerHandlerLevels(t *testing.T) {
	t.Parallel()
	quiet := &captureHandler{min: slog.LevelError}
	verbose := &captureHandler{min: slog.LevelDebug}
	logger := slog.New(NewMultiHandler(quiet, verbose))

	logger.Debug("detail")

	if len(quiet.records) != 0 {
		t.Error("a handler above the record's level should not receive it")
	}
	if len(verbose.records) != 1 {
		t.Error("a handler below the record's level should receive it")
	}
}

func TestMultiHandlerNilChildrenAreSkipped(t *testing.T) {
	t.Parallel()
	c := &captureHandler{}
	logger := slog.New(NewMultiHandler(c, nil))
	logger.Info("still works")
	if len(c.records) != 1 {
		t.Error("a nil child must be skipped rather than panic")
	}
}

// The whole tool has to be usable with nothing running alongside it.
func TestOtelDisabledByDefault(t *testing.T) {
	for _, env := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		t.Setenv(env, "")
	}

	shutdown, handler, err := SetupOtel(context.Background(), config.Otel{Enabled: "auto"})
	if err != nil {
		t.Fatalf("SetupOtel: %v", err)
	}
	defer shutdown()

	if handler != nil {
		t.Error("no OTLP endpoint is configured, so no exporter should be created")
	}
}

func TestOtelAutoEnabledByEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	if !otelEnabled(config.Otel{Enabled: "auto"}) {
		t.Error("auto should enable export once an endpoint is set")
	}
	if otelEnabled(config.Otel{Enabled: "false"}) {
		t.Error("an explicit false must win over the environment")
	}
}

func TestSyncWriterSerialisesWrites(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewSyncWriter(&buf)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.Write([]byte("0123456789\n"))
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line != "0123456789" {
			t.Fatalf("interleaved write: %q", line)
		}
	}
}

// sampleSnapshot is a clean, complete streaming request.
func sampleSnapshot() record.Snapshot {
	start := time.Date(2026, 8, 15, 10, 14, 22, 31_000_000, time.UTC)
	maxTokens := int64(4096)

	return record.Snapshot{
		ID:       "r-00042",
		Route:    "vendor",
		Provider: "vendor",
		Method:   "POST", ClientPath: "/vendor/v1/chat/completions",
		Chat: true, Model: "m-large", Streaming: true, NMessages: 12,
		MaxTokens: &maxTokens,
		Start:     start,
		End:       start.Add(4850 * time.Millisecond),
		Duration:  4850 * time.Millisecond,
		TTFB:      612 * time.Millisecond,

		Status:        200,
		HeadersSentAt: start.Add(612 * time.Millisecond),

		BytesFromUpstream: 1024, BytesToClient: 1024,
		ChunksParsed: 118, ChunksDelivered: 118,
		LastUpstreamByte: start.Add(4800 * time.Millisecond),
		LastClientWrite:  start.Add(4800 * time.Millisecond),

		Stream: &analyze.Postmortem{
			DataEvents: 118, DoneSeen: true, FinishSeen: true,
			FinishReasons: []string{"stop"},
			Usage:         &analyze.Usage{InputTokens: 1042, OutputTokens: 387, TotalTokens: 1429},
		},
		Attempts: []record.AttemptView{{N: 1, Status: 200, TTFB: 612 * time.Millisecond}},
	}
}

// captureHandler records what it was given.
type captureHandler struct {
	mu      sync.Mutex
	min     slog.Level
	records []slog.Record
}

func (c *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= c.min }

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }
