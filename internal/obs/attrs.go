package obs

import (
	"log/slog"
	"strconv"

	"github.com/peterhoneder/llm-proxy/internal/record"
)

// Attribute keys.
//
// The GenAI semantic conventions are still at "Development" stability and have
// already renamed one attribute (gen_ai.system became gen_ai.provider.name in
// semconv v1.37.0), so the keys are declared here as plain constants rather
// than taken from a versioned package. That keeps a convention change to a
// one-line edit in this file.
//
// Network facts deliberately live in a private llmproxy.* namespace instead of
// being forced into gen_ai.*: "which side dropped the connection" is not an
// LLM concept and there is no standard attribute for it.
const (
	AttrGenAIOperation    = "gen_ai.operation.name"
	AttrGenAIProvider     = "gen_ai.provider.name"
	AttrGenAIRequestModel = "gen_ai.request.model"
	AttrGenAIMaxTokens    = "gen_ai.request.max_tokens"
	AttrGenAITemperature  = "gen_ai.request.temperature"
	AttrGenAITopP         = "gen_ai.request.top_p"
	AttrGenAIResponseID   = "gen_ai.response.id"
	AttrGenAIResponseModl = "gen_ai.response.model"
	AttrGenAIFinishReason = "gen_ai.response.finish_reasons"
	AttrGenAIInputTokens  = "gen_ai.usage.input_tokens"
	AttrGenAIOutputTokens = "gen_ai.usage.output_tokens"

	// Stable, shared conventions.
	AttrErrorType     = "error.type"
	AttrServerAddress = "server.address"
	AttrHTTPStatus    = "http.response.status_code"
	AttrHTTPMethod    = "http.request.method"
	AttrURLPath       = "url.path"

	// llm-proxy's own namespace.
	AttrRoute            = "llmproxy.route"
	AttrRequestID        = "llmproxy.request.id"
	AttrConnID           = "llmproxy.connection.id"
	AttrSide             = "llmproxy.fault.side"
	AttrFaultKind        = "llmproxy.fault.kind"
	AttrFaultOp          = "llmproxy.fault.op"
	AttrFaultSyscall     = "llmproxy.fault.syscall"
	AttrFaultDetail      = "llmproxy.fault.detail"
	AttrVerdict          = "llmproxy.verdict"
	AttrAttempts         = "llmproxy.attempts"
	AttrAttempt          = "llmproxy.attempt"
	AttrKeySource        = "llmproxy.auth.source"
	AttrAuthName         = "llmproxy.auth.client"
	AttrStreaming        = "llmproxy.stream.enabled"
	AttrStreamEvents     = "llmproxy.stream.events"
	AttrStreamDoneSeen   = "llmproxy.stream.done_seen"
	AttrStreamFinishSeen = "llmproxy.stream.finish_seen"
	AttrStreamMaxGapMs   = "llmproxy.stream.max_gap_ms"
	AttrStreamTruncated  = "llmproxy.stream.truncated"
	AttrBytesRead        = "llmproxy.bytes.from_upstream"
	AttrBytesWritten     = "llmproxy.bytes.to_client"
	AttrTTFBMs           = "llmproxy.ttfb_ms"
	AttrDurationMs       = "llmproxy.duration_ms"
	AttrConnReused       = "llmproxy.conn.reused"
	AttrConnIdleMs       = "llmproxy.conn.idle_ms"
	AttrConnALPN         = "llmproxy.conn.alpn"
	AttrDNSMs            = "llmproxy.conn.dns_ms"
	AttrConnectMs        = "llmproxy.conn.connect_ms"
	AttrTLSMs            = "llmproxy.conn.tls_ms"
	AttrClientGone       = "llmproxy.client.gone"
	AttrRetryWaitMs      = "llmproxy.retry.wait_ms"
	AttrRetryReason      = "llmproxy.retry.reason"
	AttrTransportRetries = "llmproxy.transport.silent_retries"
)

// Attrs flattens a record for structured logging and for span attributes.
// Nothing sensitive is included: bodies and headers stay on the console, where
// the operator chose to look at them.
func Attrs(snap record.Snapshot) []slog.Attr {
	a := []slog.Attr{
		slog.String(AttrRequestID, snap.ID),
		slog.String(AttrRoute, snap.Route),
		slog.String(AttrHTTPMethod, snap.Method),
		slog.String(AttrURLPath, snap.ClientPath),
		slog.Int64(AttrDurationMs, snap.Duration.Milliseconds()),
	}
	if snap.ConnID != "" {
		a = append(a, slog.String(AttrConnID, snap.ConnID))
	}
	if snap.KeySource != "" {
		a = append(a, slog.String(AttrKeySource, snap.KeySource))
	}
	if snap.AuthName != "" {
		a = append(a, slog.String(AttrAuthName, snap.AuthName))
	}
	if snap.Provider != "" {
		a = append(a, slog.String(AttrGenAIProvider, snap.Provider))
	}
	if snap.Status != 0 {
		a = append(a, slog.Int(AttrHTTPStatus, snap.Status))
	}
	if snap.TTFB > 0 {
		a = append(a, slog.Int64(AttrTTFBMs, snap.TTFB.Milliseconds()))
	}
	if n := len(snap.Attempts); n > 0 {
		a = append(a, slog.Int(AttrAttempts, n))
	}

	if snap.Chat {
		a = append(a,
			slog.String(AttrGenAIOperation, "chat"),
			slog.Bool(AttrStreaming, snap.Streaming),
		)
		if snap.Model != "" {
			a = append(a, slog.String(AttrGenAIRequestModel, snap.Model))
		}
		if snap.MaxTokens != nil {
			a = append(a, slog.Int64(AttrGenAIMaxTokens, *snap.MaxTokens))
		}
		if snap.Temperature != nil {
			a = append(a, slog.Float64(AttrGenAITemperature, *snap.Temperature))
		}
		if snap.TopP != nil {
			a = append(a, slog.Float64(AttrGenAITopP, *snap.TopP))
		}
	}

	a = append(a,
		slog.Int64(AttrBytesRead, snap.BytesFromUpstream),
		slog.Int64(AttrBytesWritten, snap.BytesToClient),
	)

	if s := snap.Stream; s != nil {
		a = append(a,
			slog.Int(AttrStreamEvents, s.DataEvents),
			slog.Bool(AttrStreamDoneSeen, s.DoneSeen),
			slog.Bool(AttrStreamFinishSeen, s.FinishSeen),
		)
		if s.MaxGap > 0 {
			a = append(a, slog.Int64(AttrStreamMaxGapMs, s.MaxGap.Milliseconds()))
		}
		if len(s.FinishReasons) > 0 {
			// The convention specifies a string array; slog carries it as an
			// any so the OTel bridge can map it to a slice attribute.
			a = append(a, slog.Any(AttrGenAIFinishReason, s.FinishReasons))
		}
		if s.ResponseID != "" {
			a = append(a, slog.String(AttrGenAIResponseID, s.ResponseID))
		}
		if s.ResponseModel != "" {
			a = append(a, slog.String(AttrGenAIResponseModl, s.ResponseModel))
		}
		if s.Usage != nil {
			a = append(a,
				slog.Int64(AttrGenAIInputTokens, s.Usage.InputTokens),
				slog.Int64(AttrGenAIOutputTokens, s.Usage.OutputTokens),
			)
		}
	}

	if !snap.ClientGoneAt.IsZero() {
		a = append(a, slog.Bool(AttrClientGone, true))
	}

	if f := snap.Fault; f != nil {
		a = append(a,
			slog.String(AttrSide, f.Side.String()),
			slog.String(AttrFaultKind, string(f.Kind)),
			slog.String(AttrVerdict, f.Verdict),
		)
		if f.Op != "" {
			a = append(a, slog.String(AttrFaultOp, f.Op))
		}
		if f.Syscall != "" {
			a = append(a, slog.String(AttrFaultSyscall, f.Syscall))
		}
		if f.Detail != "" {
			a = append(a, slog.String(AttrFaultDetail, f.Detail))
		}
		// error.type is the one stable attribute here, and backends group on
		// it, so the fault kind is reused verbatim rather than invented.
		errType := string(f.Kind)
		if snap.Status >= 400 {
			errType = strconv.Itoa(snap.Status)
		}
		a = append(a, slog.String(AttrErrorType, errType))
		if f.Kind == "upstream_truncated_stream" || f.Kind == "upstream_truncated_body" {
			a = append(a, slog.Bool(AttrStreamTruncated, true))
		}
	}

	if last, ok := lastAttempt(snap); ok {
		c := &last.Conn
		a = append(a, slog.Bool(AttrConnReused, c.Reused))
		if c.IdleTime > 0 {
			a = append(a, slog.Int64(AttrConnIdleMs, c.IdleTime.Milliseconds()))
		}
		if c.ALPN != "" {
			a = append(a, slog.String(AttrConnALPN, c.ALPN))
		}
		if c.HostPort != "" {
			a = append(a, slog.String(AttrServerAddress, c.HostPort))
		}
		if d := c.DNSTime(); d > 0 {
			a = append(a, slog.Int64(AttrDNSMs, d.Milliseconds()))
		}
		if d := c.ConnectTime(); d > 0 {
			a = append(a, slog.Int64(AttrConnectMs, d.Milliseconds()))
		}
		if d := c.TLSTime(); d > 0 {
			a = append(a, slog.Int64(AttrTLSMs, d.Milliseconds()))
		}
		if c.GotConnCount > 1 {
			a = append(a, slog.Int(AttrTransportRetries, c.GotConnCount-1))
		}
		if last.Waited > 0 {
			a = append(a,
				slog.Int64(AttrRetryWaitMs, last.Waited.Milliseconds()),
				slog.String(AttrRetryReason, last.WaitReason),
			)
		}
	}

	return a
}
