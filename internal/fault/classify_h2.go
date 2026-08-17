package fault

import (
	"errors"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
)

// errNotSupported is what http.ResponseController returns when the writer
// chain cannot flush. Aliased here so classify.go does not import net/http
// solely for one sentinel.
var errNotSupported = http.ErrNotSupported

// classifyH2 recognises an HTTP/2 failure and returns the matching Kind.
//
// It has to try twice, and the reason is a genuine trap. The standard library
// vendors golang.org/x/net/http2 into net/http as generated, unexported types
// (http.http2StreamError and friends). Those are distinct types from the ones
// in golang.org/x/net/http2, so errors.As against the x/net types compiles
// cleanly and then silently never matches anything the stdlib transport
// produced — dead code that looks like it works.
//
// The typed check is real when a route enables HTTP/2 explicitly, because that
// path builds the transport with http2.ConfigureTransport and so genuinely
// uses the x/net types. For anything reaching the stdlib's own h2 stack, the
// string forms are the only handle available.
func classifyH2(err error) (Kind, bool) {
	var se http2.StreamError
	if errors.As(err, &se) {
		return KindH2Stream, true
	}
	var ge http2.GoAwayError
	if errors.As(err, &ge) {
		return KindH2GoAway, true
	}
	return classifyH2ByString(err)
}

// classifyH2ByString matches the error strings the bundled stdlib http2
// package produces. These are pinned by a test so a Go upgrade that changes
// the wording fails loudly rather than silently degrading.
func classifyH2ByString(err error) (Kind, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "http2: server sent GOAWAY"):
		return KindH2GoAway, true
	case strings.Contains(msg, "http2: stream error"):
		return KindH2Stream, true
	case strings.Contains(msg, "http2: "):
		return KindUpstreamProtocol, true
	default:
		return "", false
	}
}

// isH2Cancel reports whether an HTTP/2 error carries the CANCEL code, which
// downstream means the client aborted its own stream.
func isH2Cancel(err error) bool {
	var se http2.StreamError
	if errors.As(err, &se) {
		return se.Code == http2.ErrCodeCancel
	}
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "CANCEL")
}
