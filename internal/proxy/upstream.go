package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http2"

	"github.com/peterhoneder/llm-proxy/internal/config"
)

// newTransport builds the upstream transport for one route.
//
// Several of these settings are the difference between measuring a problem and
// causing one, so each is deliberate rather than inherited.
func newTransport(route *config.Route) (*http.Transport, error) {
	t := route.Timeouts

	dialer := &net.Dialer{
		Timeout:   config.Dur(t.Connect),
		KeepAlive: 30 * time.Second,
	}

	tr := &http.Transport{
		DialContext: dialer.DialContext,

		// Without this, a corporate HTTPS_PROXY user gets connection-refused
		// and blames the vendor. http.DefaultTransport sets it; a hand-built
		// transport does not, and forgetting it is easy.
		Proxy: http.ProxyFromEnvironment,

		TLSHandshakeTimeout: config.Dur(t.TLSHandshake),
		// Zero means no limit, which is the default. Ten minutes to a first
		// token is normal for a reasoning model, and a proxy that gives up
		// first would be reporting its own impatience as a vendor failure.
		ResponseHeaderTimeout: config.Dur(t.ResponseHeader),
		ExpectContinueTimeout: time.Second,

		// Go's default is 2, which an eight-way-concurrent agent harness blows
		// through instantly — producing fresh handshakes, new-connection TTFB
		// and occasional stale-connection EOFs. The instrument would be
		// generating exactly the noise it is meant to measure.
		MaxIdleConnsPerHost: config.Int(route.MaxIdleConnsPerHost),
		MaxIdleConns:        config.Int(route.MaxIdleConnsPerHost) * 4,

		// Deliberately below the ~60s idle close common to LLM vendors, so we
		// retire a connection before the vendor does. That directly reduces
		// the stale-keep-alive EOFs this proxy would otherwise have to explain.
		IdleConnTimeout: route.IdleConnTimeout.D(),

		DisableKeepAlives: config.Bool(route.DisableKeepalives),

		// The proxy forwards the client's Accept-Encoding untouched and passes
		// response bytes through verbatim. Letting the transport add its own
		// gzip negotiation would rewrite the client's request and silently
		// strip Content-Encoding and Content-Length from the response, so
		// "verbatim" would stop being true.
		DisableCompression: true,
	}

	if route.InsecureSkipVerify != nil && *route.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, warned about at startup
	}

	// HTTP/1.1 by default. Its failures are unambiguous — io.ErrUnexpectedEOF
	// means the body was cut — whereas HTTP/2 surfaces the same condition as a
	// stream error or a GOAWAY, which is harder to attribute. No mainstream
	// OpenAI-compatible REST endpoint requires h2, and ALPN falls back
	// automatically, so this costs nothing.
	protocols := new(http.Protocols)
	if config.Bool(route.HTTP2) {
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		// Configuring the transport through x/net/http2 is what puts the x/net
		// error types in play. Without it the standard library uses its own
		// bundled copy, whose types are unexported and unmatchable — see
		// fault.classifyH2.
		if _, err := http2.ConfigureTransports(tr); err != nil {
			return nil, err
		}
	} else {
		protocols.SetHTTP1(true)
	}
	tr.Protocols = protocols

	return tr, nil
}

// target is a resolved route: its parsed upstream, its client and the
// configuration the handler needs on the hot path.
type target struct {
	route  *config.Route
	base   *url.URL
	client *http.Client
	apiKey string
}

func newTarget(route *config.Route, apiKey string) (*target, error) {
	base, err := url.Parse(route.Upstream)
	if err != nil {
		return nil, err
	}
	tr, err := newTransport(route)
	if err != nil {
		return nil, err
	}
	return &target{
		route:  route,
		base:   base,
		apiKey: apiKey,
		client: &http.Client{
			Transport: tr,
			// Redirects are not followed: this proxy reports what the vendor
			// said, and a 302 is part of that answer.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// No client-level timeout. It would cap the whole response
			// including a legitimately long stream; the per-phase timeouts
			// above are what actually want enforcing.
		},
	}, nil
}

// CloseIdleConnections releases pooled connections at shutdown.
func (t *target) CloseIdleConnections() {
	if tr, ok := t.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}
