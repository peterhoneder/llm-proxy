package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/peterhoneder/llm-proxy/internal/config"
)

// SetupOtel configures OpenTelemetry export and returns a shutdown function
// plus the slog handler that bridges log records into OTLP.
//
// Export is off unless an endpoint is configured. The whole tool has to be
// usable with nothing running alongside it, so "auto" means "only if the
// standard environment variables say where to send things" — and when they do
// not, the returned handler is nil and no provider, batcher or network
// connection is created at all.
func SetupOtel(ctx context.Context, cfg config.Otel) (shutdown func(), handler slog.Handler, err error) {
	if !otelEnabled(cfg) {
		return func() {}, nil, nil
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "llm-proxy"
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
	))
	if err != nil {
		return nil, nil, err
	}

	// A misconfigured endpoint must not drown the console this tool exists to
	// produce, so exporter errors are logged once a minute at most.
	otel.SetErrorHandler(newThrottledErrorHandler(time.Minute))

	var shutdowns []func(context.Context) error

	if enabledSignal(cfg.Traces) {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	if enabledSignal(cfg.Logs) {
		exp, err := otlploghttp.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		)
		otellog.SetLoggerProvider(lp)
		shutdowns = append(shutdowns, lp.Shutdown)

		// otelslog attaches the active trace and span IDs from the record's
		// context, which is why every call site uses the ...Context variants:
		// without them the logs and the traces cannot be correlated.
		handler = &levelGate{
			min:  parseLevel(cfg.LogLevel),
			next: otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp)),
		}
	}

	shutdown = func() {
		// Flushing gets its own budget: the traces from the last few requests
		// before a shutdown are usually the interesting ones.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, fn := range shutdowns {
			_ = fn(ctx)
		}
	}
	return shutdown, handler, nil
}

// otelEnabled implements the "auto" default: on only when an OTLP endpoint is
// configured somewhere.
func otelEnabled(cfg config.Otel) bool {
	switch strings.ToLower(cfg.Enabled) {
	case "true":
		return true
	case "false":
		return false
	}
	for _, env := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		if os.Getenv(env) != "" {
			return true
		}
	}
	return false
}

func enabledSignal(p *bool) bool { return p == nil || *p }

// levelGate lets the OTLP side run at a different verbosity from the console —
// full detail to the collector, a readable stream in the terminal.
type levelGate struct {
	min  slog.Level
	next slog.Handler
}

func (g *levelGate) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= g.min && g.next.Enabled(ctx, l)
}

func (g *levelGate) Handle(ctx context.Context, r slog.Record) error {
	return g.next.Handle(ctx, r)
}

func (g *levelGate) WithAttrs(a []slog.Attr) slog.Handler {
	return &levelGate{min: g.min, next: g.next.WithAttrs(a)}
}

func (g *levelGate) WithGroup(name string) slog.Handler {
	return &levelGate{min: g.min, next: g.next.WithGroup(name)}
}

// throttledErrorHandler collapses repeated exporter failures.
type throttledErrorHandler struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	dropped  int
}

func newThrottledErrorHandler(interval time.Duration) otel.ErrorHandler {
	return &throttledErrorHandler{interval: interval}
}

func (h *throttledErrorHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	if now.Sub(h.last) < h.interval {
		h.dropped++
		return
	}
	dropped := h.dropped
	h.dropped = 0
	h.last = now

	if dropped > 0 {
		slog.Warn("opentelemetry export error", "error", err, "suppressed_since_last", dropped)
		return
	}
	slog.Warn("opentelemetry export error", "error", err)
}
