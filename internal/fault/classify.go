package fault

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"time"
)

// FromClientWrite classifies an error returned by writing to, or flushing,
// the downstream ResponseWriter. By construction of the copy loop these are
// always the client's doing — with one exception, noted below.
func FromClientWrite(err error) *Fault {
	if err == nil {
		return nil
	}

	// Not a client fault: the ResponseWriter chain does not support flushing,
	// which means a wrapper somewhere forgot Unwrap() http.ResponseWriter.
	// Reporting this as a client disconnect would be a lie, and a damaging one
	// — it also means the proxy is silently buffering and every latency number
	// it prints is wrong.
	if errors.Is(err, errNotSupported) {
		return New(SideProxy, KindProxyConfig, "flush", err).
			withDetail("the ResponseWriter does not support flushing, so responses are being buffered").
			withVerdict("llm-proxy bug: a ResponseWriter wrapper is missing Unwrap(). Timings from this request are not trustworthy.")
	}

	switch {
	case isBrokenPipe(err):
		return New(SideClient, KindClientEPIPE, "write", err).syscall("EPIPE").
			withDetail("write failed with broken pipe: the client had already closed the connection")
	case isConnReset(err):
		return New(SideClient, KindClientReset, "write", err).syscall("ECONNRESET").
			withDetail("write failed with connection reset: the client aborted the connection")
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return New(SideClient, KindClientStalled, "write", err).
			withDetail("write deadline exceeded: the client is still connected but has stopped reading")
	}

	if k, ok := classifyH2(err); ok && k == KindH2Stream && isH2Cancel(err) {
		return New(SideClient, KindClientDisconnect, "write", err).
			withDetail("the client cancelled the HTTP/2 stream")
	}

	return New(SideClient, KindClientWriteFailed, "write", err).
		withDetail("writing to the client failed: %v", err)
}

// FromRequestBodyRead classifies a failure while reading the client's request
// body — the client dying part-way through uploading a large prompt.
func FromRequestBodyRead(err error) *Fault {
	if err == nil {
		return nil
	}
	return New(SideClient, KindClientUpload, "body", err).
		withDetail("reading the request body failed after the client went away: %v", err)
}

// FromRoundTrip classifies a failure to obtain a response header at all: DNS,
// TCP, TLS, or the vendor never answering. Nothing has been written downstream
// at this point, so these faults are the only ones a retry can address.
//
// upCtx is the proxy-owned upstream context; its cause is the only reliable
// way to tell why a cancellation happened.
func FromRoundTrip(err error, upCtx context.Context) *Fault {
	if err == nil {
		return nil
	}
	if f := fromCancellation(err, upCtx, "dial"); f != nil {
		return f
	}
	err = unwrapURL(err)

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return New(SideUpstream, KindDNSNotFound, "dial", err).
				withDetail("DNS lookup for %s returned no records", dnsErr.Name)
		}
		return New(SideUpstream, KindDNS, "dial", err).retryable().
			withDetail("DNS lookup for %s failed: %v", dnsErr.Name, dnsErr.Err)
	}

	if f := classifyTLS(err); f != nil {
		return f
	}

	switch {
	case isConnRefused(err):
		return New(SideUpstream, KindConnRefused, "dial", err).syscall("ECONNREFUSED").retryable().
			withDetail("the upstream refused the TCP connection")
	case isUnreachable(err):
		return New(SideUpstream, KindUnreachable, "dial", err).syscall(syscallName(err)).retryable().
			withDetail("the upstream address is unreachable from this host")
	case isConnReset(err):
		return New(SideUpstream, KindUpstreamReset, "dial", err).syscall("ECONNRESET").retryable().
			withDetail("the upstream reset the connection before sending a response header")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return New(SideUpstream, KindHeaderTimeout, "dial", err).retryable().
			withDetail("the upstream did not send a response header before the deadline")
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return New(SideUpstream, KindDialTimeout, "dial", err).retryable().
				withDetail("timed out establishing a TCP connection to the upstream")
		}
		return New(SideUpstream, KindHeaderTimeout, "dial", err).retryable().
			withDetail("timed out waiting for the upstream's response header")
	}

	// An EOF before any response header, on a connection the transport took
	// from its idle pool, is the classic stale keep-alive race: the vendor had
	// already closed it. Calling that an outage would send someone hunting a
	// problem that does not exist.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return New(SideUpstream, KindIdleReuseEOF, "dial", err).retryable().
			withDetail("the upstream closed the connection before sending a response header")
	}

	if kind, ok := classifyH2(err); ok {
		return New(SideUpstream, kind, "dial", err).retryable().
			withDetail("HTTP/2 failure before a response header: %v", err)
	}

	var oe *net.OpError
	if errors.As(err, &oe) {
		return New(SideUpstream, KindUpstreamUnknown, oe.Op, err).retryable().
			withDetail("network %s to the upstream failed: %v", oe.Op, oe.Err)
	}

	return New(SideUpstream, KindUpstreamUnknown, "dial", err).
		withDetail("the request to the upstream failed: %v", err)
}

// FromRead classifies an error returned by reading the upstream response body.
// Because the copy loop reads before it writes, a non-cancellation error here
// is always the vendor's.
func FromRead(err error, upCtx context.Context, st ReadState) *Fault {
	if err == nil || errors.Is(err, io.EOF) {
		return FromCleanEOF(st)
	}
	if f := fromCancellation(err, upCtx, "read"); f != nil {
		// The client leaving after it already had the complete body is not a
		// failure — it is what a well-behaved client does, and it races our own
		// final read.
		if f.Side == SideClient && st.bodyComplete() {
			return FromCleanEOF(st)
		}
		f.Detail = stallDetail(f, st)
		return f
	}
	err = unwrapURL(err)

	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return New(SideUpstream, KindTruncatedBody, "read", err).
			withDetail("the upstream closed the connection mid-body after %s", describeProgress(st))
	case isConnReset(err):
		return New(SideUpstream, KindUpstreamReset, "read", err).syscall("ECONNRESET").
			withDetail("the upstream reset the connection after %s", describeProgress(st))
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return New(SideUpstream, KindReadTimeout, "read", err).
			withDetail("no data from the upstream before the read deadline, after %s", describeProgress(st))
	}

	if kind, ok := classifyH2(err); ok {
		return New(SideUpstream, kind, "read", err).
			withDetail("HTTP/2 failure mid-response after %s: %v", describeProgress(st), err)
	}

	if f := classifyTLS(err); f != nil {
		f.Op = "read"
		return f
	}

	return New(SideUpstream, KindUpstreamUnknown, "read", err).
		withDetail("reading the upstream response failed after %s: %v", describeProgress(st), err)
}

// FromCleanEOF decides whether a stream that ended without an I/O error
// actually finished. This is where a naive implementation goes wrong.
//
// `data: [DONE]` is an OpenAI convention, not part of the SSE specification.
// vLLM, llama.cpp's server and several gateways end a perfectly good stream
// with a terminal finish_reason and nothing else. Treating a missing sentinel
// as truncation would flag every healthy request against those backends, so
// finish_reason is the primary evidence and [DONE] only corroborates it.
//
// It returns nil when the response completed.
func FromCleanEOF(st ReadState) *Fault {
	// A partial trailing frame is unambiguous: the bytes stop mid-event.
	if st.PartialBytes > 0 {
		return New(SideUpstream, KindTruncatedBody, "read", io.ErrUnexpectedEOF).
			withDetail("the stream ended mid-frame with %d unterminated bytes, after %s",
				st.PartialBytes, describeProgress(st))
	}

	if !st.Streaming {
		// A HEAD or 204 response declares the length of a body it deliberately
		// does not send. Judging it as short would be a confidently wrong
		// verdict on a correct response.
		if !st.BodyExpected {
			return nil
		}
		if st.ContentLength >= 0 && st.BytesRead < st.ContentLength {
			return New(SideUpstream, KindTruncatedBody, "read", io.ErrUnexpectedEOF).
				withDetail("the response body stopped at %d of the %d bytes it declared",
					st.BytesRead, st.ContentLength)
		}
		return nil
	}

	// An EOF with nothing read at all, on a connection reused after an idle
	// period, is a stale keep-alive rather than a vendor failure.
	if st.BytesRead == 0 && st.Reused {
		return New(SideUpstream, KindIdleReuseEOF, "read", io.EOF).retryable().
			withDetail("the upstream closed a connection reused after %s idle, before sending anything",
				st.IdleTime.Round(100*time.Millisecond))
	}

	if st.DoneSeen {
		return nil
	}

	switch st.ExpectDone {
	case "false":
		// The operator has told us this backend never sends the sentinel.
		if st.FinishSeen {
			return nil
		}
	case "true":
		return New(SideUpstream, KindTruncatedStream, "read", io.EOF).
			withDetail("the stream ended without `data: [DONE]` after %s (expect_done is true for this route)",
				describeProgress(st))
	default: // "auto"
		if st.FinishSeen {
			return nil
		}
	}

	if st.Events == 0 && st.BytesRead == 0 {
		return New(SideUpstream, KindTruncatedStream, "read", io.EOF).
			withDetail("the upstream returned a streaming response and then sent no data at all")
	}

	return New(SideUpstream, KindTruncatedStream, "read", io.EOF).
		withDetail("the stream ended after %s with no finish_reason and no `data: [DONE]`",
			describeProgress(st))
}

// fromCancellation resolves a context cancellation through the cause the proxy
// stamped. Go's net/http cancels a server request context with a bare
// context.Canceled, so without an explicitly-owned upstream context there is
// no way to tell a client hangup from a stall from a shutdown.
func fromCancellation(err error, upCtx context.Context, op string) *Fault {
	// The sentinel can arrive two ways. A read interrupted by cancellation
	// usually surfaces as context.Canceled and the reason has to be recovered
	// from the context; but net/http also propagates the cancellation cause
	// directly, in which case the error *is* the sentinel. Checking only the
	// first form leaves the second falling through to "unknown upstream
	// error" — which is precisely the misattribution this package exists to
	// prevent, and it looks entirely plausible in the logs.
	cancelled := errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrClientGone) ||
		errors.Is(err, ErrUpstreamStall) ||
		errors.Is(err, ErrProxyShutdown) ||
		errors.Is(err, ErrHandlerReturn)
	if !cancelled {
		return nil
	}

	// Prefer the reason carried by the error itself; fall back to the context.
	var cause error
	switch {
	case errors.Is(err, ErrClientGone):
		cause = ErrClientGone
	case errors.Is(err, ErrUpstreamStall):
		cause = ErrUpstreamStall
	case errors.Is(err, ErrProxyShutdown):
		cause = ErrProxyShutdown
	case upCtx != nil:
		cause = context.Cause(upCtx)
	}

	switch {
	case errors.Is(cause, ErrClientGone):
		// The read failed only because we cancelled it once the client left,
		// so the client is the answer — this is the verdict, not a consequence
		// of one.
		return New(SideClient, KindClientDisconnect, op, err).
			withDetail("the client closed the connection; the proxy then aborted the upstream read")
	case errors.Is(cause, ErrUpstreamStall):
		// The proxy enforced the deadline, but the vendor going silent is the
		// cause. Attributing this to the proxy would be exactly the
		// misdiagnosis this tool exists to prevent.
		return New(SideUpstream, KindStallTimeout, op, err)
	case errors.Is(cause, ErrProxyShutdown):
		return New(SideProxy, KindProxyShutdown, op, err).
			withDetail("the proxy is shutting down and aborted the request")
	case errors.Is(err, context.DeadlineExceeded):
		return New(SideUpstream, KindHeaderTimeout, op, err).retryable().
			withDetail("the upstream exceeded the configured deadline")
	case cause == nil, errors.Is(cause, context.Canceled), errors.Is(cause, ErrHandlerReturn):
		// Cancelled with no cause of ours. The most likely explanation by far
		// is the client, but say so as an inference rather than a fact.
		return New(SideClient, KindClientDisconnect, op, err).
			withDetail("the request context was cancelled with no recorded cause; the client is the most likely source")
	default:
		return New(SideProxy, KindProxyInternal, op, err).
			withDetail("the upstream request was cancelled: %v", cause)
	}
}

func stallDetail(f *Fault, st ReadState) string {
	if f.Kind != KindStallTimeout {
		return f.Detail
	}
	idle := st.StreamIdle
	if idle == 0 {
		return "the upstream sent nothing for the configured stream_idle period — the proxy aborted the read"
	}
	return "no bytes from the upstream for " + idle.String() +
		" (stream_idle) — the proxy aborted the read, after " + describeProgress(st)
}

func classifyTLS(err error) *Fault {
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return New(SideUpstream, KindTLSCertInvalid, "tls", err).
			withDetail("the upstream's certificate did not verify: %v", cve.Err)
	}
	var rhe tls.RecordHeaderError
	if errors.As(err, &rhe) {
		return New(SideUpstream, KindTLSNotTLS, "tls", err).
			withDetail("the upstream did not answer with TLS: %s", rhe.Msg)
	}
	var ae tls.AlertError
	if errors.As(err, &ae) {
		return New(SideUpstream, KindTLSAlert, "tls", err).
			withDetail("the TLS handshake failed with alert: %v", ae)
	}
	return nil
}

// unwrapURL peels the *url.Error that http.Client.Do wraps around every
// failure. Without this the whole ladder below matches nothing.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func describeProgress(st ReadState) string {
	switch {
	case st.Streaming && st.Events > 0:
		return plural(st.Events, "event") + " and " + formatBytes(st.BytesRead)
	case st.BytesRead > 0:
		return formatBytes(st.BytesRead)
	default:
		return "no data"
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return itoa(n) + " " + word + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + " MiB"
	case n >= 1<<10:
		return trimFloat(float64(n)/(1<<10)) + " KiB"
	default:
		return itoa(int(n)) + " B"
	}
}

func trimFloat(f float64) string {
	whole := int(f)
	frac := int((f - float64(whole)) * 10)
	if frac == 0 {
		return itoa(whole)
	}
	return itoa(whole) + "." + itoa(frac)
}
