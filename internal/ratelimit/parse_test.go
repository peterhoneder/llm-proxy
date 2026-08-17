package ratelimit

import (
	"net/http"
	"testing"
	"time"
)

var ref = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func TestParseReset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		format  Format
		wantErr bool
	}{
		// OpenAI emits literal time.Duration.String() output.
		{"go duration minutes", "6m0s", 6 * time.Minute, FormatGoDuration, false},
		{"go duration seconds", "1s", time.Second, FormatGoDuration, false},
		{"go duration millis", "88ms", 88 * time.Millisecond, FormatGoDuration, false},
		{"go duration compound", "1h2m3s", time.Hour + 2*time.Minute + 3*time.Second, FormatGoDuration, false},

		// The trap. A 10-digit epoch also parses as a valid float, so an
		// ordering that tries plain seconds before Unix timestamps reads this
		// as ~56 years of waiting instead of an absolute deadline.
		{"unix seconds", "1786831200", 10 * time.Hour, FormatUnixSeconds, false},
		{"unix millis", "1786831200000", 10 * time.Hour, FormatUnixMillis, false},

		{"rfc3339", "2026-08-15T12:00:30Z", 30 * time.Second, FormatRFC3339, false},
		{"rfc3339 nano offset", "2026-08-15T14:00:30.5+02:00", 30500 * time.Millisecond, FormatRFC3339, false},

		{"plain seconds", "30", 30 * time.Second, FormatSeconds, false},
		{"fractional seconds", "1.5", 1500 * time.Millisecond, FormatSeconds, false},

		{"http date", "Sat, 15 Aug 2026 12:00:45 GMT", 45 * time.Second, FormatHTTPDate, false},
		{"rfc850 date", "Saturday, 15-Aug-26 12:00:45 GMT", 45 * time.Second, FormatHTTPDate, false},
		{"ansi c date", "Sat Aug 15 12:00:45 2026", 45 * time.Second, FormatHTTPDate, false},

		// Already elapsed: a negative wait would turn clock skew into a retry
		// storm.
		{"past deadline clamps to zero", "2026-08-15T11:59:00Z", 0, FormatRFC3339, false},

		{"garbage", "soon", 0, FormatUnknown, true},
		{"empty", "", 0, FormatUnknown, true},
		{"absurd number", "999999999999999999", 0, FormatUnknown, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, format, ok := ParseReset(tc.in, ref)
			if tc.wantErr {
				if ok {
					t.Fatalf("ParseReset(%q) = %v, %s, true; want it rejected", tc.in, got, format)
				}
				return
			}
			if !ok {
				t.Fatalf("ParseReset(%q) was rejected", tc.in)
			}
			if got != tc.want {
				t.Errorf("ParseReset(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if format != tc.format {
				t.Errorf("ParseReset(%q) format = %s, want %s", tc.in, format, tc.format)
			}
		})
	}
}

// Pinned separately because getting it wrong is silent: the wait is merely
// implausible, not invalid, so nothing else would fail.
func TestUnixTimestampIsNotReadAsSeconds(t *testing.T) {
	t.Parallel()
	got, format, ok := ParseReset("1786831200", ref)
	if !ok {
		t.Fatal("a 10-digit epoch should parse")
	}
	if format != FormatUnixSeconds {
		t.Fatalf("format = %s, want unix-seconds", format)
	}
	if got > 24*time.Hour {
		t.Errorf("wait = %v — the epoch was read as a duration in seconds", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		format  Format
		wantErr bool
	}{
		{"delay seconds", "120", 120 * time.Second, FormatSeconds, false},
		{"one second", "1", time.Second, FormatSeconds, false},
		{"zero", "0", 0, FormatSeconds, false},
		// Not in RFC 9110, but observed from real vendors.
		{"fractional", "1.5", 1500 * time.Millisecond, FormatSeconds, false},
		{"http date", "Sat, 15 Aug 2026 12:02:00 GMT", 2 * time.Minute, FormatHTTPDate, false},
		{"past http date", "Sat, 15 Aug 2026 11:00:00 GMT", 0, FormatHTTPDate, false},
		{"negative", "-5", 0, FormatSeconds, false},
		{"garbage", "later", 0, FormatUnknown, true},
		{"empty", "", 0, FormatUnknown, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, format, ok := ParseRetryAfter(tc.in, ref)
			if tc.wantErr {
				if ok {
					t.Fatalf("ParseRetryAfter(%q) unexpectedly parsed", tc.in)
				}
				return
			}
			if !ok {
				t.Fatalf("ParseRetryAfter(%q) was rejected", tc.in)
			}
			if got != tc.want || format != tc.format {
				t.Errorf("ParseRetryAfter(%q) = %v/%s, want %v/%s", tc.in, got, format, tc.want, tc.format)
			}
		})
	}
}

func TestParseOpenAIHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "60")
	h.Set("x-ratelimit-remaining-requests", "0")
	h.Set("x-ratelimit-reset-requests", "1s")
	h.Set("x-ratelimit-limit-tokens", "150000")
	h.Set("x-ratelimit-remaining-tokens", "149984")
	h.Set("x-ratelimit-reset-tokens", "6m0s")

	s := Parse(h, ref, time.Time{})
	requests := findBucket(t, s, "requests")
	tokens := findBucket(t, s, "tokens")

	if *requests.Limit != 60 || *requests.Remaining != 0 || *requests.Reset != time.Second {
		t.Errorf("requests bucket = %+v", requests)
	}
	if !requests.Exhausted() {
		t.Error("a bucket with 0 remaining must report Exhausted")
	}
	if *tokens.Reset != 6*time.Minute {
		t.Errorf("tokens reset = %v, want 6m", *tokens.Reset)
	}
	if tokens.Exhausted() {
		t.Error("the tokens bucket still has capacity")
	}

	// Only exhausted buckets can block a retry; one with capacity left must
	// not inflate the wait.
	if got := s.EarliestReset(); got == nil || *got != time.Second {
		t.Errorf("EarliestReset = %v, want 1s from the exhausted bucket only", got)
	}
}

// Anthropic-style headers put the bucket name before the field, which is the
// opposite of OpenAI's ordering.
func TestParseNameBeforeKindOrdering(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("anthropic-ratelimit-input-tokens-limit", "40000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "39000")
	h.Set("anthropic-ratelimit-input-tokens-reset", "2026-08-15T12:00:20Z")

	s := Parse(h, ref, time.Time{})
	b := findBucket(t, s, "input-tokens")
	if *b.Limit != 40000 || *b.Remaining != 39000 {
		t.Errorf("input-tokens bucket = %+v", b)
	}
	if *b.Reset != 20*time.Second {
		t.Errorf("reset = %v, want 20s", *b.Reset)
	}
}

// The point of suffix matching: a vendor nobody has documented still parses,
// with no code change.
func TestUnknownVendorBucketStillParses(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-scw-ratelimit-remaining-image-units", "7")
	h.Set("x-scw-ratelimit-limit-image-units", "100")

	s := Parse(h, ref, time.Time{})
	b := findBucket(t, s, "image-units")
	if *b.Remaining != 7 || *b.Limit != 100 {
		t.Errorf("image-units bucket = %+v", b)
	}
}

func TestBareRateLimitHeadersDefaultToRequests(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("ratelimit-remaining", "5")
	h.Set("ratelimit-limit", "10")
	h.Set("ratelimit-reset", "30")

	s := Parse(h, ref, time.Time{})
	b := findBucket(t, s, "requests")
	if *b.Remaining != 5 || *b.Limit != 10 || *b.Reset != 30*time.Second {
		t.Errorf("bucket = %+v", b)
	}
}

func TestHyphenatedRateLimitSpelling(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-rate-limit-remaining-requests", "3")
	s := Parse(h, ref, time.Time{})
	if b := findBucket(t, s, "requests"); *b.Remaining != 3 {
		t.Errorf("bucket = %+v", b)
	}
}

// Mistral answers a 429 with Retry-After and often nothing else.
func TestRetryAfterOnly(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Retry-After", "1")

	s := Parse(h, ref, time.Time{})
	if s.RetryAfter == nil || *s.RetryAfter != time.Second {
		t.Fatalf("RetryAfter = %v, want 1s", s.RetryAfter)
	}
	if len(s.Buckets) != 0 {
		t.Errorf("Buckets = %+v, want none", s.Buckets)
	}
	if s.EarliestReset() != nil {
		t.Error("EarliestReset should be nil with no buckets")
	}
}

// Anything rate-limit-shaped that will not parse must still surface: that is
// how an undocumented dialect gets discovered.
func TestUnrecognisedHeadersArePreserved(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", "next tuesday")
	h.Set("x-ratelimit-policy-note", "burst")

	s := Parse(h, ref, time.Time{})
	if len(s.Unrecognised) == 0 {
		t.Fatal("unparseable rate-limit headers must be kept verbatim")
	}
	var sawReset bool
	for _, kv := range s.Unrecognised {
		if kv.Value == "next tuesday" {
			sawReset = true
		}
	}
	if !sawReset {
		t.Errorf("Unrecognised = %+v, want the unparseable reset value", s.Unrecognised)
	}
}

func TestNonRateLimitHeadersAreIgnored(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-Id", "abc")

	s := Parse(h, ref, time.Time{})
	if !s.Empty() {
		t.Errorf("unrelated headers produced %+v", s)
	}
}

// An HTTP-date deadline is stated on the server's clock, so a skewed local
// clock must not distort the wait.
func TestClockSkewCorrection(t *testing.T) {
	t.Parallel()
	serverNow := ref
	localNow := ref.Add(90 * time.Second) // our clock runs fast

	h := http.Header{}
	h.Set("Retry-After", "Sat, 15 Aug 2026 12:00:30 GMT")

	s := Parse(h, localNow, serverNow)
	if s.RetryAfter == nil {
		t.Fatal("Retry-After was not parsed")
	}
	if *s.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s measured against the server's clock", *s.RetryAfter)
	}
	if s.ClockSkew != 90*time.Second {
		t.Errorf("ClockSkew = %v, want 90s recorded", s.ClockSkew)
	}
}

func TestBucketLow(t *testing.T) {
	t.Parallel()
	limit, remaining := int64(100), int64(4)
	b := Bucket{Limit: &limit, Remaining: &remaining}
	if !b.Low(0.05) {
		t.Error("4/100 should count as low at a 5% threshold")
	}
	if b.Low(0.01) {
		t.Error("4/100 is not low at a 1% threshold")
	}
	if (Bucket{}).Low(0.05) {
		t.Error("a bucket with no numbers cannot be low")
	}
}

func TestServerDate(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Date", "Sat, 15 Aug 2026 12:00:00 GMT")
	if got := ServerDate(h); !got.Equal(ref) {
		t.Errorf("ServerDate = %v, want %v", got, ref)
	}
	if got := ServerDate(http.Header{}); !got.IsZero() {
		t.Errorf("ServerDate with no header = %v, want zero", got)
	}
}

func findBucket(t *testing.T, s *Snapshot, name string) Bucket {
	t.Helper()
	for _, b := range s.Buckets {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("no bucket named %q in %+v", name, s.Buckets)
	return Bucket{}
}
