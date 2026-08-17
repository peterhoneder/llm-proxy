// Package fault classifies a failed request by the side responsible for it.
//
// This is the point of llm-proxy. Everything else — the logging, the traces,
// the rate-limit parsing — exists to support one judgement: when an LLM stream
// dies, was it the client, the vendor, or the proxy itself?
//
// The classification is structural rather than heuristic. The proxy's copy loop
// reads from the upstream and writes to the client in that order, so an error
// returned by the read is the vendor's and an error returned by the write is
// the client's. The exception is cancellation, which can originate anywhere;
// see Cause and Classify.
package fault

import (
	"errors"
	"fmt"
	"time"
)

// Side names who is responsible for a fault.
type Side uint8

const (
	SideNone Side = iota
	SideClient
	SideUpstream
	SideProxy
)

func (s Side) String() string {
	switch s {
	case SideClient:
		return "client"
	case SideUpstream:
		return "upstream"
	case SideProxy:
		return "proxy"
	default:
		return "none"
	}
}

// Kind is a stable, machine-readable fault identifier. It is a string so it can
// be used directly as an OpenTelemetry error.type and llmproxy.fault.kind value
// without a translation table.
type Kind string

const (
	// Upstream, transport level.
	KindDNS              Kind = "dns_failure"
	KindDNSNotFound      Kind = "dns_not_found"
	KindConnRefused      Kind = "connection_refused"
	KindUnreachable      Kind = "network_unreachable"
	KindDialTimeout      Kind = "dial_timeout"
	KindTLSHandshake     Kind = "tls_handshake_failed"
	KindTLSCertInvalid   Kind = "tls_certificate_invalid"
	KindTLSAlert         Kind = "tls_alert"
	KindTLSNotTLS        Kind = "tls_not_a_tls_server"
	KindUpstreamReset    Kind = "upstream_connection_reset"
	KindTruncatedBody    Kind = "upstream_truncated_body"
	KindTruncatedStream  Kind = "upstream_truncated_stream"
	KindErrorInStream    Kind = "upstream_error_in_stream"
	KindIdleReuseEOF     Kind = "idle_reuse_eof"
	KindReadTimeout      Kind = "upstream_read_timeout"
	KindStallTimeout     Kind = "upstream_stall_timeout"
	KindHeaderTimeout    Kind = "upstream_response_header_timeout"
	KindH2Stream         Kind = "http2_stream_error"
	KindH2GoAway         Kind = "http2_goaway"
	KindUpstreamProtocol Kind = "upstream_protocol_error"
	KindUpstreamEOF      Kind = "upstream_unexpected_eof"
	KindUpstreamUnknown  Kind = "upstream_unknown"

	// Upstream, HTTP level.
	KindHTTPStatus      Kind = "http_status"
	KindRateLimited     Kind = "rate_limited"
	KindContextLength   Kind = "context_length_exceeded"
	KindOutputTruncated Kind = "output_truncated"
	KindContentFilter   Kind = "content_filter"

	// Client.
	KindClientDisconnect  Kind = "client_disconnect_ctx"
	KindClientEPIPE       Kind = "client_disconnect_epipe"
	KindClientReset       Kind = "client_disconnect_reset"
	KindClientStalled     Kind = "client_stalled"
	KindClientUpload      Kind = "client_disconnect_upload"
	KindClientWriteFailed Kind = "client_write_failed"

	// Proxy.
	KindProxyShutdown Kind = "proxy_shutdown"
	KindProxyConfig   Kind = "proxy_misconfigured"
	KindProxyInternal Kind = "proxy_internal"
	KindBodyTooLarge  Kind = "request_body_too_large"
)

func (k Kind) String() string { return string(k) }

// Cancellation sentinels. The proxy owns the upstream request's context and
// stamps one of these as the cancellation cause, because Go's net/http cancels
// a server request context with a bare context.Canceled and no cause — so the
// cause is the only reliable way to learn why a read was interrupted.
var (
	ErrClientGone    = errors.New("client connection closed")
	ErrUpstreamStall = errors.New("upstream stopped sending")
	ErrProxyShutdown = errors.New("proxy shutting down")
	ErrHandlerReturn = errors.New("handler returned")
)

// Fault is a classified failure. A zero Fault is never valid; use the
// constructors in classify.go.
type Fault struct {
	Side    Side
	Kind    Kind
	Op      string // dial | tls | write-request | read | write | flush | status | watchdog | body
	Err     error
	Syscall string // "EPIPE", "ECONNRESET", ... when the cause reached a syscall

	// Detail is one sentence of evidence: what was observed.
	Detail string
	// Verdict is one sentence of judgement: who broke it, in plain English.
	// Every fault has one; the renderer always prints it.
	Verdict string

	// HTTPStatus is set for faults derived from a response status.
	HTTPStatus int

	// Retryable marks faults that a retry could plausibly fix, and only
	// matters before any byte has been written downstream.
	Retryable bool

	// Induced marks an upstream error that is a consequence of a client fault
	// — typically the read failing because we cancelled it after the client
	// left. An induced fault must never become the verdict, or every client
	// disconnect would be misreported as a vendor outage.
	Induced bool
}

func (f *Fault) Error() string {
	if f == nil {
		return "<nil fault>"
	}
	if f.Err != nil {
		return fmt.Sprintf("%s/%s: %v", f.Side, f.Kind, f.Err)
	}
	return fmt.Sprintf("%s/%s: %s", f.Side, f.Kind, f.Detail)
}

func (f *Fault) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// Is lets errors.Is match on Kind, so callers can write
// errors.Is(err, &Fault{Kind: KindTruncatedStream}).
func (f *Fault) Is(target error) bool {
	t, ok := target.(*Fault)
	if !ok {
		return false
	}
	return t.Kind == "" || t.Kind == f.Kind
}

// AsInduced converts an upstream fault that a client disconnect caused into the
// client fault it really is, keeping the original error as evidence.
//
// Without this, a client pressing ^C mid-stream can surface as whatever the
// vendor's socket happened to do in response, and the report would blame the
// vendor for a disconnect the client initiated.
func AsInduced(f *Fault) *Fault {
	if f == nil {
		return nil
	}
	out := New(SideClient, KindClientDisconnect, f.Op, f.Err)
	out.Induced = true
	out.Syscall = f.Syscall
	out.Detail = "the client closed the connection first; the upstream then reported: " + f.Detail
	return out
}

// ReadState is what the copy loop knows about a stream at the moment an
// upstream read failed or ended. It is what turns one ambiguous io.EOF into
// three very different conclusions.
type ReadState struct {
	BytesRead int64
	Events    int  // complete SSE events parsed
	DoneSeen  bool // `data: [DONE]` observed
	// FinishSeen reports a terminal finish_reason on every choice, which many
	// OpenAI-compatible backends send instead of a [DONE] sentinel.
	FinishSeen bool
	// PartialBytes is an unterminated trailing fragment at EOF: the strongest
	// possible evidence that a response was cut mid-frame.
	PartialBytes int

	// Reused and IdleTime describe the upstream connection. An EOF with no
	// bytes read on a connection reused after a long idle period is a stale
	// keep-alive race, not a vendor outage, and must not be reported as one.
	Reused   bool
	IdleTime time.Duration

	// StreamIdle is the configured inter-chunk deadline, used in messages.
	StreamIdle time.Duration

	// Streaming distinguishes SSE from a plain JSON response.
	Streaming bool
	// ExpectDone is the route's expect_done setting: "auto", "true", "false".
	ExpectDone string
	// ContentLength is the declared body length for non-streaming responses,
	// or -1 when unknown.
	ContentLength int64

	// BodyExpected is false for responses that carry no body at all — HEAD,
	// 204, 304, 1xx. Those still declare a Content-Length describing the body
	// a GET *would* have returned, so comparing bytes read against it would
	// manufacture a truncation verdict for a perfectly correct response.
	BodyExpected bool
}

// bodyComplete reports whether the evidence already shows the whole response
// had been read before the interruption.
//
// A client that closes as soon as it has the full body is behaving correctly,
// and it races the proxy's own final read. Without this check that ordinary
// completion surfaces as a client disconnect on perfectly healthy requests.
func (st ReadState) bodyComplete() bool {
	if st.Streaming {
		return st.DoneSeen || st.FinishSeen
	}
	if !st.BodyExpected {
		return true
	}
	return st.ContentLength >= 0 && st.BytesRead >= st.ContentLength
}

// New builds a fault, filling in a default verdict for the kind when the
// caller has nothing more specific to say.
func New(side Side, kind Kind, op string, err error) *Fault {
	f := &Fault{Side: side, Kind: kind, Op: op, Err: err}
	f.Verdict = defaultVerdict(kind)
	return f
}

// WithDetail sets the evidence line.
func (f *Fault) WithDetail(format string, args ...any) *Fault {
	f.Detail = fmt.Sprintf(format, args...)
	return f
}

// WithVerdict overrides the plain-English judgement.
func (f *Fault) WithVerdict(format string, args ...any) *Fault {
	f.Verdict = fmt.Sprintf(format, args...)
	return f
}

func (f *Fault) withDetail(format string, args ...any) *Fault  { return f.WithDetail(format, args...) }
func (f *Fault) withVerdict(format string, args ...any) *Fault { return f.WithVerdict(format, args...) }

func (f *Fault) retryable() *Fault {
	f.Retryable = true
	return f
}

func (f *Fault) syscall(name string) *Fault {
	f.Syscall = name
	return f
}

// defaultVerdict is the plain-English judgement rendered when nothing more
// specific applies. These are written for someone reading a terminal at 2am.
func defaultVerdict(k Kind) string {
	switch k {
	case KindDNS, KindDNSNotFound:
		return "the upstream hostname could not be resolved. Check the route's upstream and your DNS."
	case KindConnRefused:
		return "nothing is listening at the upstream address. The request never reached the vendor."
	case KindUnreachable:
		return "the network could not reach the vendor. The request never left your machine's network."
	case KindDialTimeout:
		return "the vendor did not accept a TCP connection in time. The request never reached it."
	case KindTLSHandshake, KindTLSAlert:
		return "the TLS handshake with the vendor failed. Nothing was sent."
	case KindTLSCertInvalid:
		return "the vendor's TLS certificate did not verify — check for an intercepting proxy or an expired certificate."
	case KindTLSNotTLS:
		return "the upstream did not answer with TLS. Check whether the URL should be http:// instead of https://."
	case KindUpstreamReset:
		return "the vendor reset the connection. Your tool will see an incomplete reply."
	case KindTruncatedBody:
		return "the vendor cut the response mid-frame. Your tool will see an incomplete reply."
	case KindTruncatedStream:
		return "the vendor ended the stream without finishing the answer. Your tool will see an incomplete reply."
	case KindErrorInStream:
		return "the vendor returned 200 and then reported an error mid-stream. The answer is incomplete."
	case KindIdleReuseEOF:
		return "the vendor closed an idle keep-alive connection. This is a stale-connection race, not an outage — retrying usually succeeds immediately."
	case KindReadTimeout, KindStallTimeout:
		return "the vendor stopped sending mid-answer and never resumed."
	case KindHeaderTimeout:
		return "the vendor accepted the request but never sent a response header."
	case KindH2Stream, KindH2GoAway:
		return "the vendor's HTTP/2 connection failed mid-request."
	case KindUpstreamProtocol, KindUpstreamEOF:
		return "the vendor sent a malformed or truncated HTTP response."
	case KindRateLimited:
		return "the vendor is rate limiting you."
	case KindContextLength:
		return "the prompt is too big for this model. Nothing was streamed."
	case KindOutputTruncated:
		return "the model hit the output token cap — the answer is cut off, but the protocol behaved correctly. Raise max_tokens."
	case KindContentFilter:
		return "the vendor's content filter stopped the response."
	case KindHTTPStatus:
		return "the vendor rejected the request. The full response body is above."
	case KindClientDisconnect, KindClientEPIPE, KindClientReset:
		return "your LLM tool hung up first. The vendor did not interrupt anything."
	case KindClientStalled:
		return "your LLM tool stayed connected but stopped reading, so the proxy could not write to it."
	case KindClientUpload:
		return "your LLM tool disconnected while it was still uploading the request."
	case KindClientWriteFailed:
		return "the proxy could not write to your LLM tool."
	case KindProxyShutdown:
		return "the proxy was shut down by the operator. This is not a vendor fault."
	case KindProxyConfig:
		return "the proxy is misconfigured — this is llm-proxy's own fault, not the vendor's."
	case KindBodyTooLarge:
		return "the request body exceeded max_request_body."
	case KindProxyInternal:
		return "an internal proxy error. This is llm-proxy's own fault, not the vendor's."
	default:
		return "the cause could not be attributed to a side; the raw error is above."
	}
}
