package proxy

import (
	"net/http"
	"strings"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/obs"
)

// hopByHop are the connection-scoped headers RFC 9110 §7.6.1 forbids
// forwarding. They are stripped in both directions: echoing an upstream's
// `Connection: keep-alive` downstream is just as wrong as forwarding the
// client's.
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop removes the standard hop-by-hop headers and, critically, every
// header the Connection field itself names. Skipping that second part is the
// usual bug: `Connection: X-Internal-Token` is a real instruction to drop
// X-Internal-Token, and forwarding it leaks a header the sender asked us not to.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// copyHeaders duplicates src into a fresh header map with hop-by-hop entries
// removed. The original is never mutated, so the record can still show what the
// client actually sent.
func copyHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
	stripHopByHop(dst)
	return dst
}

// applyAuth sets the route's credentials on an outbound request and reports
// where they came from.
//
// A client that sent its own Authorization keeps it: someone testing a second
// key through the proxy should not silently have it replaced. Reporting the
// source is what turns most 401 mysteries into one line of output.
func applyAuth(h http.Header, route *config.Route, key string) obs.KeySource {
	name := route.APIKeyHeader
	if name == "" {
		name = "Authorization"
	}

	clientSupplied := h.Get(name) != ""
	if clientSupplied && (config.Bool(route.ForwardClientAuth) || key == "") {
		return obs.KeyClientSupplied
	}
	if key == "" {
		if clientSupplied {
			return obs.KeyClientSupplied
		}
		return obs.KeyNone
	}

	h.Set(name, route.APIKeyPrefix+key)
	return obs.KeyInjected
}

// applyExtraHeaders adds the route's configured static headers, without
// overriding anything the client set deliberately.
func applyExtraHeaders(h http.Header, route *config.Route) {
	for k, v := range route.Headers {
		if h.Get(k) == "" {
			h.Set(k, v)
		}
	}
}

// prepareResponseHeaders copies the upstream's headers downstream.
//
// Content-Length is dropped when Go's transport transparently decompressed the
// body, because the declared length then describes bytes the client will never
// see. Everything else is passed through untouched: this proxy exists to show
// what the vendor actually sent.
func prepareResponseHeaders(dst, src http.Header, uncompressed bool) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	stripHopByHop(dst)
	if uncompressed {
		dst.Del("Content-Length")
		dst.Del("Content-Encoding")
	}
}
