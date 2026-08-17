// Package ratelimit reads rate-limit information out of a response's headers.
//
// It is deliberately format-tolerant rather than vendor-specific. llm-proxy is
// pointed at arbitrary OpenAI-compatible backends, several of which do not
// document their headers at all — so the parser recognises headers by shape,
// keeps anything it cannot structure, and prints the leftovers. Discovering an
// undocumented vendor's dialect is meant to be something you do *with* this
// tool, not something you must know before using it.
package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Format records how a value was written, so the console can show it and a
// misparse is visible rather than silent.
type Format uint8

const (
	FormatUnknown Format = iota
	FormatGoDuration
	FormatSeconds
	FormatRFC3339
	FormatUnixSeconds
	FormatUnixMillis
	FormatHTTPDate
)

func (f Format) String() string {
	switch f {
	case FormatGoDuration:
		return "go-duration"
	case FormatSeconds:
		return "seconds"
	case FormatRFC3339:
		return "rfc3339"
	case FormatUnixSeconds:
		return "unix-seconds"
	case FormatUnixMillis:
		return "unix-millis"
	case FormatHTTPDate:
		return "http-date"
	default:
		return "unknown"
	}
}

// Bucket is one rate-limited resource: requests, tokens, input-tokens, ...
type Bucket struct {
	Name      string
	Limit     *int64
	Remaining *int64
	Reset     *time.Duration
	ResetRaw  string
	ResetFmt  Format
}

// Exhausted reports whether this bucket has nothing left.
func (b Bucket) Exhausted() bool { return b.Remaining != nil && *b.Remaining <= 0 }

// Low reports whether the bucket is within frac of empty, for an early warning.
func (b Bucket) Low(frac float64) bool {
	if b.Limit == nil || b.Remaining == nil || *b.Limit <= 0 {
		return false
	}
	return float64(*b.Remaining)/float64(*b.Limit) <= frac
}

// KV is a header we recognised as rate-limit-related but could not structure.
// These are printed verbatim so an unknown vendor's dialect becomes visible.
type KV struct{ Key, Value string }

// Snapshot is everything the response said about rate limiting.
type Snapshot struct {
	RetryAfter    *time.Duration
	RetryAfterRaw string
	RetryAfterFmt Format

	Buckets      []Bucket
	Unrecognised []KV

	// ClockSkew is the difference between the server's Date header and our
	// clock. An HTTP-date Retry-After is meaningless without it.
	ClockSkew time.Duration
}

// Empty reports whether the response carried no rate-limit information at all.
func (s *Snapshot) Empty() bool {
	return s == nil || (s.RetryAfter == nil && len(s.Buckets) == 0 && len(s.Unrecognised) == 0)
}

// EarliestReset returns the soonest reset among exhausted buckets, which is the
// only wait a retry actually has to respect. A bucket with capacity left does
// not block anything.
func (s *Snapshot) EarliestReset() *time.Duration {
	var best *time.Duration
	for i := range s.Buckets {
		b := s.Buckets[i]
		if !b.Exhausted() || b.Reset == nil {
			continue
		}
		if best == nil || *b.Reset < *best {
			d := *b.Reset
			best = &d
		}
	}
	return best
}

var kindTokens = map[string]string{
	"limit":     "limit",
	"remaining": "remaining",
	"reset":     "reset",
}

// Parse extracts rate-limit information from response headers. now is the
// reference for relative deadlines; serverDate is the response's Date header
// (zero if absent) and is used to correct for clock skew.
func Parse(h http.Header, now, serverDate time.Time) *Snapshot {
	s := &Snapshot{}
	if h == nil {
		return s
	}

	if !serverDate.IsZero() {
		s.ClockSkew = now.Sub(serverDate)
	}
	// Absolute deadlines the server states are on the server's clock, so
	// resolve them against it when we know it.
	ref := now
	if !serverDate.IsZero() {
		ref = serverDate
	}

	byName := map[string]*Bucket{}
	var order []string

	for key, values := range h {
		if len(values) == 0 {
			continue
		}
		raw := values[0]
		lower := strings.ToLower(key)

		if lower == "retry-after" {
			if d, f, ok := ParseRetryAfter(raw, ref); ok {
				s.RetryAfter, s.RetryAfterRaw, s.RetryAfterFmt = &d, raw, f
			} else {
				s.Unrecognised = append(s.Unrecognised, KV{key, raw})
			}
			continue
		}

		if !looksRateLimited(lower) {
			continue
		}

		name, kind, ok := splitBucketHeader(lower)
		if !ok {
			s.Unrecognised = append(s.Unrecognised, KV{key, raw})
			continue
		}

		b := byName[name]
		if b == nil {
			b = &Bucket{Name: name}
			byName[name] = b
			order = append(order, name)
		}

		switch kind {
		case "limit":
			if v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
				b.Limit = &v
			} else {
				s.Unrecognised = append(s.Unrecognised, KV{key, raw})
			}
		case "remaining":
			if v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
				b.Remaining = &v
			} else {
				s.Unrecognised = append(s.Unrecognised, KV{key, raw})
			}
		case "reset":
			if d, f, ok := ParseReset(raw, ref); ok {
				b.Reset, b.ResetRaw, b.ResetFmt = &d, raw, f
			} else {
				b.ResetRaw, b.ResetFmt = raw, FormatUnknown
				s.Unrecognised = append(s.Unrecognised, KV{key, raw})
			}
		}
	}

	for _, name := range order {
		s.Buckets = append(s.Buckets, *byName[name])
	}
	return s
}

// looksRateLimited is intentionally loose: a header we misjudge is merely
// printed in the unrecognised list, whereas one we skip is invisible.
func looksRateLimited(lower string) bool {
	return strings.Contains(lower, "ratelimit") || strings.Contains(lower, "rate-limit")
}

// splitBucketHeader pulls the bucket name and field out of a header name.
//
// Two orderings exist in the wild and both must work:
//
//	x-ratelimit-remaining-tokens        (OpenAI style: kind then name)
//	anthropic-ratelimit-tokens-reset    (name then kind)
//	ratelimit-remaining                 (no name at all)
//
// so the position of the kind token decides which side the name is on.
func splitBucketHeader(lower string) (name, kind string, ok bool) {
	normalised := strings.ReplaceAll(lower, "rate-limit", "ratelimit")
	parts := strings.Split(normalised, "-")

	marker := -1
	for i, p := range parts {
		if p == "ratelimit" {
			marker = i
			break
		}
	}
	if marker < 0 || marker == len(parts)-1 {
		return "", "", false
	}

	rest := parts[marker+1:]

	// Kind last: anthropic-ratelimit-input-tokens-reset
	if k, isKind := kindTokens[rest[len(rest)-1]]; isKind && len(rest) > 1 {
		return strings.Join(rest[:len(rest)-1], "-"), k, true
	}
	// Kind first: x-ratelimit-remaining-tokens, or bare: ratelimit-remaining
	if k, isKind := kindTokens[rest[0]]; isKind {
		if len(rest) == 1 {
			return "requests", k, true
		}
		return strings.Join(rest[1:], "-"), k, true
	}
	return "", "", false
}

// ParseRetryAfter handles both RFC 9110 forms — delay-seconds and HTTP-date —
// plus the fractional seconds some vendors send in practice.
func ParseRetryAfter(raw string, ref time.Time) (time.Duration, Format, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, FormatUnknown, false
	}

	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, FormatSeconds, true
		}
		return time.Duration(n) * time.Second, FormatSeconds, true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second)), FormatSeconds, true
	}
	// http.ParseTime covers IMF-fixdate, RFC 850 and ANSI C asctime, which is
	// all three formats RFC 9110 permits.
	if t, err := http.ParseTime(v); err == nil {
		return clampNonNegative(t.Sub(ref)), FormatHTTPDate, true
	}
	return 0, FormatUnknown, false
}

// ParseReset turns a *-reset header into a wait. The order of the attempts
// matters and is not the obvious one; see the comment on the digit case.
func ParseReset(raw string, ref time.Time) (time.Duration, Format, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, FormatUnknown, false
	}

	// 1. Go duration: OpenAI emits literal time.Duration.String() output —
	//    "6m0s", "1s", "88ms".
	if d, err := time.ParseDuration(v); err == nil {
		return clampNonNegative(d), FormatGoDuration, true
	}

	// 2. Unix timestamps, BEFORE plain seconds. "1700000000" is a perfectly
	//    valid float, so a naive ordering reads a Unix timestamp as 54 years
	//    of waiting. Length plus a plausibility window disambiguates.
	if isAllDigits(v) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			switch len(v) {
			case 10:
				if t := time.Unix(n, 0); plausible(t, ref) {
					return clampNonNegative(t.Sub(ref)), FormatUnixSeconds, true
				}
			case 13:
				if t := time.UnixMilli(n); plausible(t, ref) {
					return clampNonNegative(t.Sub(ref)), FormatUnixMillis, true
				}
			}
		}
	}

	// 3. RFC 3339 absolute timestamps.
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return clampNonNegative(t.Sub(ref)), FormatRFC3339, true
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return clampNonNegative(t.Sub(ref)), FormatRFC3339, true
	}

	// 4. Bare seconds, capped at a day so a stray timestamp cannot slip
	//    through as an absurd wait.
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 24*60*60 {
		return time.Duration(f * float64(time.Second)), FormatSeconds, true
	}

	// 5. HTTP dates.
	if t, err := http.ParseTime(v); err == nil {
		return clampNonNegative(t.Sub(ref)), FormatHTTPDate, true
	}

	return 0, FormatUnknown, false
}

// plausible rejects a timestamp interpretation that lands absurdly far from
// now, which is how a value that merely looks like an epoch gets caught.
func plausible(t, ref time.Time) bool {
	const window = 370 * 24 * time.Hour
	d := t.Sub(ref)
	return d > -window && d < window
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// clampNonNegative keeps an already-elapsed deadline from becoming a negative
// wait, which would otherwise turn clock skew into an immediate retry storm.
func clampNonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// ServerDate reads the response Date header, returning the zero time when it is
// absent or unparseable.
func ServerDate(h http.Header) time.Time {
	if h == nil {
		return time.Time{}
	}
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return t
}
