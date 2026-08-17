// Package conntrace wires net/http/httptrace into a record.ConnTimeline.
//
// This is where "the request was slow" becomes "DNS took 12ms, the TLS
// handshake took 31ms, and then the vendor sat on it for 4 seconds". Without
// it, every connection-level failure looks the same from the outside.
package conntrace

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/record"
)

// WithTrace attaches a ClientTrace for the given attempt to ctx.
func WithTrace(ctx context.Context, a *record.Attempt, now func() time.Time, captureHeaders bool) context.Context {
	return httptrace.WithClientTrace(ctx, New(a, now, captureHeaders))
}

// New returns a ClientTrace that fills in a.Conn, plus the mutex-protected
// writer it uses.
//
// The hooks do not all run on the caller's goroutine — WroteRequest fires on
// the transport's write goroutine and GotConn can fire on a dial goroutine —
// so every field write is guarded.
func New(a *record.Attempt, now func() time.Time, captureHeaders bool) *httptrace.ClientTrace {
	if now == nil {
		now = time.Now
	}
	// Every hook mutates through the attempt's own lock: these callbacks run on
	// the transport's goroutines, while the renderer may be reading the same
	// timeline.
	set := func(f func(c *record.ConnTimeline)) { a.WithConn(f) }

	tr := &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			set(func(c *record.ConnTimeline) { c.HostPort = hostPort })
		},
		DNSStart: func(i httptrace.DNSStartInfo) {
			set(func(c *record.ConnTimeline) { c.DNSStart = now() })
		},
		DNSDone: func(i httptrace.DNSDoneInfo) {
			set(func(c *record.ConnTimeline) {
				c.DNSDone = now()
				c.DNSAddrs = i.Addrs
				c.DNSErr = i.Err
				c.DNSCoalesced = i.Coalesced
			})
		},
		ConnectStart: func(network, addr string) {
			set(func(c *record.ConnTimeline) { c.ConnectStart = now() })
		},
		ConnectDone: func(network, addr string, err error) {
			set(func(c *record.ConnTimeline) {
				c.ConnectDone = now()
				c.RemoteAddr = addr
				c.DialErr = err
			})
		},
		TLSHandshakeStart: func() {
			set(func(c *record.ConnTimeline) { c.TLSStart = now() })
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			set(func(c *record.ConnTimeline) {
				c.TLSDone = now()
				c.TLSErr = err
				c.TLSVersion = cs.Version
				c.TLSCipher = cs.CipherSuite
				c.ALPN = cs.NegotiatedProtocol
				// Certificate expiry and an unexpected issuer are ordinary
				// causes of the failures this proxy is pointed at, and an
				// intercepting corporate proxy shows up here first.
				if len(cs.PeerCertificates) > 0 {
					c.CertNotAfter = cs.PeerCertificates[0].NotAfter
					c.CertSubject = cs.PeerCertificates[0].Subject.CommonName
				}
			})
		},
		GotConn: func(i httptrace.GotConnInfo) {
			set(func(c *record.ConnTimeline) {
				c.GotConn = now()
				c.Reused = i.Reused
				c.WasIdle = i.WasIdle
				c.IdleTime = i.IdleTime
				// Counting acquisitions inside one RoundTrip is how a silent
				// transport-level replay becomes visible; otherwise the
				// timings simply stop making sense with no explanation.
				c.GotConnCount++
				if i.Conn != nil && i.Conn.LocalAddr() != nil {
					c.LocalAddr = i.Conn.LocalAddr().String()
				}
			})
		},
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			set(func(c *record.ConnTimeline) {
				c.WroteRequest = now()
				c.WriteErr = i.Err
			})
		},
		GotFirstResponseByte: func() {
			set(func(c *record.ConnTimeline) { c.FirstByte = now() })
		},
	}

	// Capturing the outbound header block is only wanted in full-trace mode:
	// it allocates per header and is the one place the real Authorization
	// value is visible, so it stays off by default.
	if captureHeaders {
		tr.WroteHeaderField = func(key string, values []string) {
			a.AddSentHeader(key, values)
		}
	}

	return tr
}
