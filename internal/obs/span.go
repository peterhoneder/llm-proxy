package obs

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// Tracer returns llm-proxy's tracer. When OpenTelemetry is not configured the
// global provider is a no-op, so callers never need to check.
func Tracer() trace.Tracer { return otel.Tracer("llm-proxy") }

// StartRequestSpan opens the span for one proxied request.
//
// The name is corrected once the model is known; the GenAI conventions want
// "{operation} {model}", and the model only appears after the request body has
// been peeked at.
func StartRequestSpan(ctx context.Context, route string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "chat",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String(AttrRoute, route)),
	)
}

// StartAttemptSpan opens a child span for one upstream attempt, so a retried
// request shows each try separately rather than as one blurred total.
func StartAttemptSpan(ctx context.Context, n int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "upstream attempt",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.Int(AttrAttempt, n)),
	)
}

// TraceID returns the current trace id, or "" when tracing is off. It is
// returned to the client as a header so a harness log line can be matched to
// the span.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// ApplySpan copies the finished record onto the span and sets its status.
func ApplySpan(span trace.Span, snap record.Snapshot) {
	if span == nil || !span.IsRecording() {
		return
	}

	if snap.Chat && snap.Model != "" {
		span.SetName("chat " + snap.Model)
	}
	span.SetAttributes(toKeyValues(Attrs(snap))...)

	// The connection timeline becomes span events, which is what turns "it was
	// slow" into "the TLS handshake took 4 seconds".
	for _, a := range snap.Attempts {
		addConnEvents(span, a)
	}

	if f := snap.Fault; f != nil {
		span.RecordError(f)
		// The verdict is the one sentence worth reading in a trace viewer.
		span.SetStatus(codes.Error, f.Side.String()+": "+f.Verdict)
		return
	}
	span.SetStatus(codes.Ok, "")
}

func addConnEvents(span trace.Span, a record.AttemptView) {
	c := a.Conn
	events := []struct {
		name string
		at   time.Time
	}{
		{"dns.start", c.DNSStart},
		{"dns.done", c.DNSDone},
		{"connect.start", c.ConnectStart},
		{"connect.done", c.ConnectDone},
		{"tls.start", c.TLSStart},
		{"tls.done", c.TLSDone},
		{"conn.acquired", c.GotConn},
		{"request.written", c.WroteRequest},
		{"response.first_byte", c.FirstByte},
	}
	for _, e := range events {
		if e.at.IsZero() {
			continue
		}
		span.AddEvent(e.name,
			trace.WithTimestamp(e.at),
			trace.WithAttributes(attribute.Int(AttrAttempt, a.N)),
		)
	}
}

// toKeyValues converts the flat slog attributes into OTel ones, so the console,
// the log record and the span all derive from the same source rather than
// drifting apart.
func toKeyValues(attrs []slog.Attr) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch a.Value.Kind() {
		case slog.KindString:
			out = append(out, attribute.String(a.Key, a.Value.String()))
		case slog.KindInt64:
			out = append(out, attribute.Int64(a.Key, a.Value.Int64()))
		case slog.KindFloat64:
			out = append(out, attribute.Float64(a.Key, a.Value.Float64()))
		case slog.KindBool:
			out = append(out, attribute.Bool(a.Key, a.Value.Bool()))
		case slog.KindAny:
			// finish_reasons is specified as a string array; anything else
			// falls back to its rendered form rather than being dropped.
			if ss, ok := a.Value.Any().([]string); ok {
				out = append(out, attribute.StringSlice(a.Key, ss))
				continue
			}
			out = append(out, attribute.String(a.Key, a.Value.String()))
		default:
			out = append(out, attribute.String(a.Key, a.Value.String()))
		}
	}
	return out
}

// SetAttemptOutcome stamps the result of one attempt onto its child span.
func SetAttemptOutcome(span trace.Span, status int, f *fault.Fault) {
	if span == nil || !span.IsRecording() {
		return
	}
	if status != 0 {
		span.SetAttributes(attribute.Int(AttrHTTPStatus, status))
	}
	if f != nil {
		span.RecordError(f)
		span.SetStatus(codes.Error, string(f.Kind))
		span.SetAttributes(
			attribute.String(AttrSide, f.Side.String()),
			attribute.String(AttrFaultKind, string(f.Kind)),
		)
		return
	}
	span.SetStatus(codes.Ok, "")
}
