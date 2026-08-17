package fault

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// wrap simulates what http.Client.Do does to every transport error. If the
// ladder forgets to unwrap this, essentially nothing matches.
func wrap(err error) error {
	return &url.Error{Op: "Post", URL: "https://api.example.com/v1/chat/completions", Err: err}
}

func cancelledWith(cause error) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	return ctx
}

func TestFromClientWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantSide    Side
		wantKind    Kind
		wantSyscall string
	}{
		{"broken pipe", syscall.EPIPE, SideClient, KindClientEPIPE, "EPIPE"},
		{"wrapped broken pipe", fmt.Errorf("write tcp: %w", syscall.EPIPE), SideClient, KindClientEPIPE, "EPIPE"},
		{"connection reset", syscall.ECONNRESET, SideClient, KindClientReset, "ECONNRESET"},
		{
			"op error reset",
			&net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET},
			SideClient, KindClientReset, "ECONNRESET",
		},
		{"write deadline", os.ErrDeadlineExceeded, SideClient, KindClientStalled, ""},
		{"unknown", errors.New("something else"), SideClient, KindClientWriteFailed, ""},
		// A missing Unwrap() on a ResponseWriter wrapper is the proxy's bug.
		// Blaming the client would hide that responses are being buffered.
		{"flush unsupported", http.ErrNotSupported, SideProxy, KindProxyConfig, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := FromClientWrite(tc.err)
			if f == nil {
				t.Fatal("expected a fault")
			}
			checkFault(t, f, tc.wantSide, tc.wantKind)
			if f.Syscall != tc.wantSyscall {
				t.Errorf("Syscall = %q, want %q", f.Syscall, tc.wantSyscall)
			}
		})
	}
}

func TestFromClientWriteNilIsNoFault(t *testing.T) {
	t.Parallel()
	if f := FromClientWrite(nil); f != nil {
		t.Errorf("nil error produced a fault: %v", f)
	}
}

func TestFromRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		wantSide  Side
		wantKind  Kind
		retryable bool
	}{
		{
			"dns not found",
			wrap(&net.DNSError{Name: "api.example.com", IsNotFound: true}),
			SideUpstream, KindDNSNotFound, false,
		},
		{
			"dns timeout",
			wrap(&net.DNSError{Name: "api.example.com", IsTimeout: true, Err: "timeout"}),
			SideUpstream, KindDNS, true,
		},
		{"connection refused", wrap(syscall.ECONNREFUSED), SideUpstream, KindConnRefused, true},
		{"host unreachable", wrap(syscall.EHOSTUNREACH), SideUpstream, KindUnreachable, true},
		{
			"dial timeout",
			wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}),
			SideUpstream, KindDialTimeout, true,
		},
		{"response header timeout", wrap(context.DeadlineExceeded), SideUpstream, KindHeaderTimeout, true},
		{
			"certificate invalid",
			wrap(&tls.CertificateVerificationError{Err: errors.New("expired")}),
			SideUpstream, KindTLSCertInvalid, false,
		},
		{
			"not a tls server",
			wrap(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}),
			SideUpstream, KindTLSNotTLS, false,
		},
		{"tls alert", wrap(tls.AlertError(40)), SideUpstream, KindTLSAlert, false},
		// An EOF before any header on a pooled connection is a stale
		// keep-alive, not an outage. Getting this wrong sends someone hunting
		// a vendor problem that does not exist.
		{"eof before header", wrap(io.EOF), SideUpstream, KindIdleReuseEOF, true},
		{"goaway", wrap(http2.GoAwayError{ErrCode: http2.ErrCodeEnhanceYourCalm}), SideUpstream, KindH2GoAway, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := FromRoundTrip(tc.err, context.Background())
			if f == nil {
				t.Fatal("expected a fault")
			}
			checkFault(t, f, tc.wantSide, tc.wantKind)
			if f.Retryable != tc.retryable {
				t.Errorf("Retryable = %v, want %v", f.Retryable, tc.retryable)
			}
		})
	}
}

func TestFromRead(t *testing.T) {
	t.Parallel()
	st := ReadState{Streaming: true, Events: 12, BytesRead: 4096, ExpectDone: "auto"}
	tests := []struct {
		name     string
		err      error
		wantSide Side
		wantKind Kind
	}{
		{"truncated mid-body", io.ErrUnexpectedEOF, SideUpstream, KindTruncatedBody},
		{"reset mid-stream", syscall.ECONNRESET, SideUpstream, KindUpstreamReset},
		{"read deadline", os.ErrDeadlineExceeded, SideUpstream, KindReadTimeout},
		{"stream error", http2.StreamError{Code: http2.ErrCodeInternal}, SideUpstream, KindH2Stream},
		{"unknown", errors.New("boom"), SideUpstream, KindUpstreamUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := FromRead(tc.err, context.Background(), st)
			if f == nil {
				t.Fatal("expected a fault")
			}
			checkFault(t, f, tc.wantSide, tc.wantKind)
			if f.Detail == "" {
				t.Error("Detail must describe what was observed")
			}
		})
	}
}

// The four-way cancellation table. Every ambiguity in the product collapses to
// this: a context.Canceled from an upstream read means nothing on its own, and
// only the cause the proxy stamped can say which side is responsible.
func TestCancellationIsResolvedByCause(t *testing.T) {
	t.Parallel()
	st := ReadState{Streaming: true, Events: 3, BytesRead: 512, StreamIdle: 60 * time.Second, ExpectDone: "auto"}
	tests := []struct {
		name     string
		cause    error
		wantSide Side
		wantKind Kind
	}{
		{"client left", ErrClientGone, SideClient, KindClientDisconnect},
		// The proxy enforced the deadline, but the vendor going silent is the
		// cause. Reporting SideProxy here would be the exact misdiagnosis this
		// tool exists to prevent.
		{"vendor stalled", ErrUpstreamStall, SideUpstream, KindStallTimeout},
		{"operator stopped us", ErrProxyShutdown, SideProxy, KindProxyShutdown},
		{"no cause recorded", nil, SideClient, KindClientDisconnect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := cancelledWith(tc.cause)
			f := FromRead(context.Canceled, ctx, st)
			if f == nil {
				t.Fatal("expected a fault")
			}
			checkFault(t, f, tc.wantSide, tc.wantKind)
		})
	}
}

// net/http propagates a cancellation cause as the error itself as well as
// through the context. Only handling the context form leaves the other falling
// through to "unknown upstream error" — a plausible-looking misattribution
// that nothing else would catch.
func TestSentinelCarriedByTheErrorIsResolved(t *testing.T) {
	t.Parallel()
	st := ReadState{Streaming: true, Events: 2}
	tests := []struct {
		err      error
		wantSide Side
		wantKind Kind
	}{
		{fmt.Errorf("read: %w", ErrClientGone), SideClient, KindClientDisconnect},
		{fmt.Errorf("read: %w", ErrUpstreamStall), SideUpstream, KindStallTimeout},
		{fmt.Errorf("read: %w", ErrProxyShutdown), SideProxy, KindProxyShutdown},
	}
	for _, tc := range tests {
		t.Run(string(tc.wantKind), func(t *testing.T) {
			t.Parallel()
			// An empty context: the reason is only available from the error.
			f := FromRead(tc.err, context.Background(), st)
			checkFault(t, f, tc.wantSide, tc.wantKind)
		})
	}
}

// A vendor socket error that only happened because the client left is the
// client's fault, not the vendor's.
func TestAsInduced(t *testing.T) {
	t.Parallel()
	upstream := FromRead(syscall.ECONNRESET, context.Background(),
		ReadState{Streaming: true, Events: 5, BytesRead: 900})
	if upstream.Side != SideUpstream {
		t.Fatalf("precondition: Side = %s, want upstream", upstream.Side)
	}

	got := AsInduced(upstream)
	checkFault(t, got, SideClient, KindClientDisconnect)
	if !got.Induced {
		t.Error("the corrected fault should be marked induced")
	}
	if !contains(got.Detail, "reset the connection") {
		t.Errorf("Detail = %q, want the original upstream evidence kept", got.Detail)
	}
	if AsInduced(nil) != nil {
		t.Error("AsInduced(nil) must be nil")
	}
}

func TestStallDetailNamesTheTimeout(t *testing.T) {
	t.Parallel()
	st := ReadState{Streaming: true, Events: 8, BytesRead: 900, StreamIdle: 45 * time.Second, ExpectDone: "auto"}
	f := FromRead(context.Canceled, cancelledWith(ErrUpstreamStall), st)
	if want := "45s"; !contains(f.Detail, want) {
		t.Errorf("Detail = %q, want it to name the configured %s stream_idle", f.Detail, want)
	}
	if !contains(f.Detail, "stream_idle") {
		t.Errorf("Detail = %q, want it to name the setting that fired", f.Detail)
	}
}

// FromCleanEOF is where a naive implementation flags healthy backends as
// broken, so each branch is pinned.
func TestFromCleanEOF(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		st       ReadState
		wantNil  bool
		wantKind Kind
	}{
		{
			"done sentinel seen",
			ReadState{Streaming: true, Events: 40, BytesRead: 2048, DoneSeen: true, ExpectDone: "auto"},
			true, "",
		},
		{
			// vLLM, llama.cpp and several gateways never send [DONE]. Flagging
			// this as truncation would fire on every healthy request.
			"finish_reason but no done sentinel, auto",
			ReadState{Streaming: true, Events: 40, BytesRead: 2048, FinishSeen: true, ExpectDone: "auto"},
			true, "",
		},
		{
			"no finish_reason and no sentinel",
			ReadState{Streaming: true, Events: 40, BytesRead: 2048, ExpectDone: "auto"},
			false, KindTruncatedStream,
		},
		{
			"expect_done true is stricter",
			ReadState{Streaming: true, Events: 40, BytesRead: 2048, FinishSeen: true, ExpectDone: "true"},
			false, KindTruncatedStream,
		},
		{
			// A partial frame is the strongest evidence available and outranks
			// everything else.
			"partial trailing frame",
			ReadState{Streaming: true, Events: 40, BytesRead: 2048, FinishSeen: true, PartialBytes: 19, ExpectDone: "auto"},
			false, KindTruncatedBody,
		},
		{
			"nothing at all on a reused connection",
			ReadState{Streaming: true, Reused: true, IdleTime: 58 * time.Second, ExpectDone: "auto"},
			false, KindIdleReuseEOF,
		},
		{
			"streaming response with no data",
			ReadState{Streaming: true, ExpectDone: "auto"},
			false, KindTruncatedStream,
		},
		{
			"non-streaming complete",
			ReadState{ContentLength: 900, BytesRead: 900, BodyExpected: true},
			true, "",
		},
		{
			"non-streaming short body",
			ReadState{ContentLength: 9113, BytesRead: 4182, BodyExpected: true},
			false, KindTruncatedBody,
		},
		{
			"non-streaming unknown length",
			ReadState{ContentLength: -1, BytesRead: 4182, BodyExpected: true},
			true, "",
		},
		{
			// A HEAD or 204 declares the length of a body it deliberately does
			// not send. Comparing bytes read against it would invent a
			// truncation verdict for a correct response — and, because
			// truncation aborts the connection, break the client too.
			"bodyless response with a declared length",
			ReadState{ContentLength: 4096, BytesRead: 0, BodyExpected: false},
			true, "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := FromCleanEOF(tc.st)
			if tc.wantNil {
				if f != nil {
					t.Fatalf("expected a clean completion, got %s: %s", f.Kind, f.Detail)
				}
				return
			}
			if f == nil {
				t.Fatalf("expected %s", tc.wantKind)
			}
			checkFault(t, f, SideUpstream, tc.wantKind)
		})
	}
}

func TestShortBodyDetailReportsBothCounts(t *testing.T) {
	t.Parallel()
	f := FromCleanEOF(ReadState{ContentLength: 9113, BytesRead: 4182, BodyExpected: true})
	for _, want := range []string{"4182", "9113"} {
		if !contains(f.Detail, want) {
			t.Errorf("Detail = %q, want it to report %s", f.Detail, want)
		}
	}
}

// The stdlib bundles x/net/http2 as unexported types, so errors.As against the
// x/net package never matches errors the standard transport produced. These
// strings are the only handle, and pinning them means a Go upgrade that
// rewords them fails here instead of silently losing the classification.
func TestHTTP2StringFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		msg  string
		want Kind
	}{
		{"http2: server sent GOAWAY and closed the connection", KindH2GoAway},
		{"http2: stream error: stream ID 7; INTERNAL_ERROR", KindH2Stream},
		{"http2: response body closed", KindUpstreamProtocol},
	}
	for _, tc := range tests {
		t.Run(tc.want.String(), func(t *testing.T) {
			t.Parallel()
			got, ok := classifyH2ByString(errors.New(tc.msg))
			if !ok || got != tc.want {
				t.Errorf("classifyH2ByString(%q) = %q, %v; want %q, true", tc.msg, got, ok, tc.want)
			}
		})
	}
	if _, ok := classifyH2ByString(errors.New("connection reset by peer")); ok {
		t.Error("a non-http2 error must not match the http2 fallback")
	}
}

func TestH2CancelIsAClientFault(t *testing.T) {
	t.Parallel()
	f := FromClientWrite(http2.StreamError{StreamID: 3, Code: http2.ErrCodeCancel})
	checkFault(t, f, SideClient, KindClientDisconnect)
}

func TestEveryFaultCarriesAVerdict(t *testing.T) {
	t.Parallel()
	// The verdict is what the operator actually reads. A fault without one is
	// a rendering hole, so no constructor may produce a blank.
	samples := []*Fault{
		FromClientWrite(syscall.EPIPE),
		FromClientWrite(errors.New("mystery")),
		FromRoundTrip(wrap(syscall.ECONNREFUSED), context.Background()),
		FromRoundTrip(wrap(errors.New("mystery")), context.Background()),
		FromRead(io.ErrUnexpectedEOF, context.Background(), ReadState{Streaming: true}),
		FromRead(context.Canceled, cancelledWith(ErrClientGone), ReadState{Streaming: true}),
		FromCleanEOF(ReadState{Streaming: true, Events: 3, ExpectDone: "auto"}),
		FromRequestBodyRead(io.ErrUnexpectedEOF),
	}
	for i, f := range samples {
		if f == nil {
			t.Fatalf("sample %d produced no fault", i)
		}
		if f.Verdict == "" {
			t.Errorf("sample %d (%s/%s) has no verdict", i, f.Side, f.Kind)
		}
		if f.Side == SideNone {
			t.Errorf("sample %d (%s) was not attributed to a side", i, f.Kind)
		}
	}
}

func TestFaultErrorsIs(t *testing.T) {
	t.Parallel()
	f := FromRead(io.ErrUnexpectedEOF, context.Background(), ReadState{Streaming: true})
	if !errors.Is(f, &Fault{Kind: KindTruncatedBody}) {
		t.Error("errors.Is should match on Kind")
	}
	if !errors.Is(f, io.ErrUnexpectedEOF) {
		t.Error("errors.Is should reach the wrapped cause")
	}
	if errors.Is(f, &Fault{Kind: KindClientEPIPE}) {
		t.Error("errors.Is must not match a different Kind")
	}
}

func TestSideString(t *testing.T) {
	t.Parallel()
	for s, want := range map[Side]string{
		SideNone: "none", SideClient: "client", SideUpstream: "upstream", SideProxy: "proxy",
	} {
		if got := s.String(); got != want {
			t.Errorf("Side(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func checkFault(t *testing.T, f *Fault, wantSide Side, wantKind Kind) {
	t.Helper()
	if f.Side != wantSide {
		t.Errorf("Side = %s, want %s", f.Side, wantSide)
	}
	if f.Kind != wantKind {
		t.Errorf("Kind = %s, want %s", f.Kind, wantKind)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
