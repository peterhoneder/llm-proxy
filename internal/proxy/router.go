package proxy

import (
	"net/url"
	"strings"
)

// joinPath builds the upstream path from the route's base and the part of the
// client's path that follows the route prefix.
//
// The rule is spelled out because the obvious implementation is wrong in a way
// that only shows up against a real vendor. Clients append /v1 themselves, so a
// config of `upstream: https://api.example.com/v1` plus an inbound
// `/vendor/v1/chat/completions` yields `/v1/v1/chat/completions` and a 404 from
// every backend. Config validation warns about that; this function is careful
// only about slashes.
//
// Escaped segments are preserved: %2F in a path must not silently become a
// separator on the way through.
func joinPath(base, rest string) (path, rawPath string) {
	base = strings.TrimSuffix(base, "/")

	switch {
	case rest == "" || rest == "/":
		if base == "" {
			return "/", ""
		}
		return base, ""
	case !strings.HasPrefix(rest, "/"):
		rest = "/" + rest
	}
	return base + rest, ""
}

// upstreamURL rewrites a client request URL into the upstream URL for a route.
// The query string is carried across verbatim, including its exact encoding.
func upstreamURL(base *url.URL, routeName string, in *url.URL) *url.URL {
	rest := strings.TrimPrefix(in.EscapedPath(), "/"+routeName)
	if rest == in.EscapedPath() {
		// The prefix was not present, which means the mux matched the bare
		// route path with no trailing slash.
		rest = ""
	}

	out := *base
	joined, _ := joinPath(base.EscapedPath(), rest)

	// Setting RawPath as well as Path keeps percent-encoded segments intact;
	// Go only consults RawPath when it is a valid encoding of Path.
	unescaped, err := url.PathUnescape(joined)
	if err != nil {
		unescaped = joined
	}
	out.Path = unescaped
	out.RawPath = joined
	out.RawQuery = in.RawQuery
	return &out
}

// isChatCompletions reports whether a path is the endpoint worth analysing.
// Every other path under a route prefix is still proxied verbatim — a harness
// calls /v1/models at startup and a 404 there breaks it — but only chat
// completions get body parsing and stream analysis.
func isChatCompletions(method, path string) bool {
	return method == "POST" && strings.HasSuffix(strings.TrimSuffix(path, "/"), "/chat/completions")
}
