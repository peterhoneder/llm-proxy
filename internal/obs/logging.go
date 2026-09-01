package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// Logger is llm-proxy's logging front end.
//
// Ordinary messages fan out through a multiHandler to the console and, when
// configured, to OpenTelemetry. Completed requests take a different path: the
// console gets a rendered multi-line block written as a single unit, while
// OpenTelemetry gets the same facts as flat attributes. Trying to force a
// seven-line fault report through a one-line-per-record handler produces
// something neither humans nor collectors can use.
type Logger struct {
	slog *slog.Logger // console + otel, for ordinary messages
	otel *slog.Logger // otel only, for request records rendered separately

	console  io.Writer
	pretty   bool
	renderer *Renderer
	level    slog.Level
}

// Options configure the logger.
type Options struct {
	Cfg config.Log
	// Out defaults to stderr. Tests pass a buffer.
	Out io.Writer
	// OtelHandler is nil when OpenTelemetry is disabled, which is the default.
	OtelHandler slog.Handler
	// Now is injectable so golden output is stable.
	Now func() time.Time
	// IsTerminal decides colour under "auto"; nil means detect from Out.
	IsTerminal func(io.Writer) bool
}

// NewLogger builds the logger described by cfg.
func NewLogger(o Options) *Logger {
	out := o.Out
	if out == nil {
		out = os.Stderr
	}
	out = NewSyncWriter(out)

	level := parseLevel(o.Cfg.Level)
	pretty := o.Cfg.Format != "json"
	color := wantColor(o.Cfg.Color, out, o.IsTerminal)

	var consoleHandler slog.Handler
	if pretty {
		consoleHandler = tint.NewTextHandler(out, &tint.Options{
			Level:      level,
			TimeFormat: "15:04:05.000",
			NoColor:    !color,
		})
	} else {
		consoleHandler = slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})
	}

	l := &Logger{
		console: out,
		pretty:  pretty,
		level:   level,
		renderer: NewRenderer(RendererOptions{
			Color:     color,
			Symbols:   o.Cfg.Symbols,
			Redactor:  NewRedactor(o.Cfg.RedactHeaders, o.Cfg.UnsafeRevealSecrets),
			FullTrace: o.Cfg.FullTrace,
			MaxBody:   o.Cfg.MaxBodyBytes.Int64(),
			Level:     level,
		}),
	}

	l.slog = slog.New(NewMultiHandler(consoleHandler, o.OtelHandler))
	if o.OtelHandler != nil {
		l.otel = slog.New(o.OtelHandler)
	} else {
		l.otel = slog.New(discardHandler{})
	}
	return l
}

// Slog exposes the underlying logger for ordinary messages.
func (l *Logger) Slog() *slog.Logger { return l.slog }

// Renderer exposes the console renderer, mainly for tests.
func (l *Logger) Renderer() *Renderer { return l.renderer }

// Enabled reports whether a level would be logged.
func (l *Logger) Enabled(level slog.Level) bool { return level >= l.level }

// Request emits a completed request: a rendered block on the console and flat
// attributes to OpenTelemetry.
func (l *Logger) Request(ctx context.Context, snap record.Snapshot) {
	level := levelFor(snap)

	if l.pretty {
		if level >= l.level {
			// One Write for the whole block; syncWriter keeps concurrent
			// requests from shredding each other's reports.
			_, _ = l.console.Write([]byte(l.renderer.Render(snap)))
		}
		// The console has the human version; OpenTelemetry gets the data.
		l.otel.LogAttrs(ctx, level, summaryLine(snap), Attrs(snap)...)
		return
	}

	// In JSON mode there is no block to render, so one structured record goes
	// everywhere.
	l.slog.LogAttrs(ctx, level, summaryLine(snap), Attrs(snap)...)
}

// Startup prints the banner and route table. It goes straight to the console
// because it is a table, not an event.
func (l *Logger) Startup(text string) {
	if l.pretty {
		_, _ = l.console.Write([]byte(text))
		return
	}
	l.slog.Info(strings.TrimSpace(text))
}

func levelFor(snap record.Snapshot) slog.Level {
	if f := snap.Fault; f != nil {
		switch f.Side {
		case fault.SideNone:
			return slog.LevelInfo
		case fault.SideClient:
			// A client hanging up is ordinary operation for an interactive
			// tool — someone pressed ^C. It is worth seeing, but it is not an
			// error and should not read like one.
			return slog.LevelWarn
		default:
			return slog.LevelError
		}
	}
	if len(snap.Warnings) > 0 {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func wantColor(mode string, out io.Writer, isTerminal func(io.Writer) bool) bool {
	switch strings.ToLower(mode) {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if isTerminal != nil {
		return isTerminal(out)
	}
	return isTerminalWriter(out)
}

// isTerminalWriter reports whether w is a character device. Piping the output
// to a file or through grep must produce clean text.
func isTerminalWriter(w io.Writer) bool {
	type unwrapper interface{ Unwrap() io.Writer }
	for {
		switch v := w.(type) {
		case *syncWriter:
			w = v.w
		case unwrapper:
			w = v.Unwrap()
		case *os.File:
			st, err := v.Stat()
			if err != nil {
				return false
			}
			return st.Mode()&os.ModeCharDevice != 0
		default:
			return false
		}
	}
}

// discardHandler stands in for the OpenTelemetry bridge when export is off, so
// call sites never need to check.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
