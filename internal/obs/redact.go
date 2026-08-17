// Package obs renders a request record for humans and, when configured, for
// OpenTelemetry.
package obs

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Redactor rewrites sensitive header values.
//
// Every path that renders a header goes through this — console, full trace,
// span attributes, error reports — because httptrace hands back the literal
// `Authorization: Bearer sk-...` and there are three separate destinations
// (terminal, log file, and an OTLP collector that may not even be yours) where
// it must not land.
//
// The replacement is a fingerprint rather than a blank, because the questions
// people actually ask are "is a key set", "is it the key I think it is", and
// "did the client send its own" — all answerable without revealing it.
type Redactor struct {
	keys   map[string]bool
	reveal bool
}

// NewRedactor builds a Redactor for the given header names. reveal disables
// redaction entirely and is expected to be accompanied by a loud warning.
func NewRedactor(headers []string, reveal bool) *Redactor {
	keys := make(map[string]bool, len(headers))
	for _, h := range headers {
		keys[strings.ToLower(strings.TrimSpace(h))] = true
	}
	return &Redactor{keys: keys, reveal: reveal}
}

// Sensitive reports whether a header name is redacted.
func (r *Redactor) Sensitive(key string) bool {
	if r == nil || r.reveal {
		return false
	}
	return r.keys[strings.ToLower(key)]
}

// Value returns the renderable form of one header value.
func (r *Redactor) Value(key, value string) string {
	if !r.Sensitive(key) {
		return value
	}
	return Fingerprint(value)
}

// Fingerprint reduces a secret to something identifiable but unusable:
//
//	Bearer sk-pr…9f2c (len=51, sha256:1a2b3c4d)
//
// The scheme prefix is kept because "the client sent Basic auth" is itself a
// finding, and the hash lets two keys be told apart across runs.
func Fingerprint(value string) string {
	if value == "" {
		return "(empty)"
	}

	scheme, secret := "", value
	if i := strings.IndexByte(value, ' '); i > 0 && i < 16 {
		scheme, secret = value[:i+1], value[i+1:]
	}

	sum := sha256.Sum256([]byte(secret))
	digest := hex.EncodeToString(sum[:])[:8]

	var shown string
	switch n := len(secret); {
	case n <= 8:
		// Too short to reveal any of it without giving most of it away.
		shown = "…"
	default:
		head := 5
		if n < 16 {
			head = 2
		}
		shown = secret[:head] + "…" + secret[n-4:]
	}

	return scheme + shown + " (len=" + strconv.Itoa(len(secret)) + ", sha256:" + digest + ")"
}

// HeaderLine is one header ready to print.
type HeaderLine struct {
	Key   string
	Value string
}

// Headers renders a header block in a stable order with values redacted.
// Repeated values are shown on their own lines rather than joined, so a
// duplicated header is visible.
func (r *Redactor) Headers(h http.Header) []HeaderLine {
	if len(h) == 0 {
		return nil
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]HeaderLine, 0, len(h))
	for _, k := range keys {
		for _, v := range h[k] {
			lines = append(lines, HeaderLine{Key: k, Value: r.Value(k, v)})
		}
	}
	return lines
}

// KeySource describes where a request's credentials came from. Rendering this
// on every request resolves most 401 mysteries in one line.
type KeySource int

const (
	KeyNone KeySource = iota
	KeyInjected
	KeyClientSupplied
)

func (k KeySource) String() string {
	switch k {
	case KeyInjected:
		return "injected"
	case KeyClientSupplied:
		return "client-supplied"
	default:
		return "none"
	}
}
