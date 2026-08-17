package oaierr

import "testing"

func TestParseShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantMsg string
		wantTyp string
		wantCod string
	}{
		{
			"openai nested",
			`{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			"This model's maximum context length is 128000 tokens.", "invalid_request_error", "context_length_exceeded",
		},
		{
			"flat mistral style",
			`{"message":"Requests rate limit exceeded","type":"rate_limit","request_id":"3a1f8b2c"}`,
			"Requests rate limit exceeded", "rate_limit", "",
		},
		{
			"error as a bare string",
			`{"error":"model not found"}`,
			"model not found", "", "",
		},
		{
			"fastapi detail string",
			`{"detail":"Not authenticated"}`,
			"Not authenticated", "", "",
		},
		{
			// Vendors disagree on whether code is a string or a number.
			"numeric code",
			`{"error":{"message":"too many requests","code":429}}`,
			"too many requests", "", "429",
		},
		{
			"vllm object error",
			`{"object":"error","message":"This model's maximum context length is 4096 tokens","type":"BadRequestError","code":400}`,
			"This model's maximum context length is 4096 tokens", "BadRequestError", "400",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := Parse([]byte(tc.body))
			if e == nil {
				t.Fatal("expected an envelope")
			}
			if e.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", e.Message, tc.wantMsg)
			}
			if e.Type != tc.wantTyp {
				t.Errorf("Type = %q, want %q", e.Type, tc.wantTyp)
			}
			if e.Code != tc.wantCod {
				t.Errorf("Code = %q, want %q", e.Code, tc.wantCod)
			}
		})
	}
}

func TestRequestIDIsFoundAtEitherLevel(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"request_id":"abc123","error":{"message":"x"}}`,
		`{"error":{"message":"x","request_id":"abc123"}}`,
	} {
		e := Parse([]byte(body))
		if e == nil || e.RequestID != "abc123" {
			t.Errorf("Parse(%s) lost the request id: %+v", body, e)
		}
	}
}

// A body that is not an error envelope must be reported as such rather than
// invented — an HTML 502 page from an intermediate proxy is itself a finding.
func TestNonEnvelopeBodies(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"",
		"   ",
		"<html><body><h1>502 Bad Gateway</h1></body></html>",
		"not json at all",
		`{"unrelated":"payload"}`,
		`{"error":null}`,
	} {
		if e := Parse([]byte(body)); e != nil {
			t.Errorf("Parse(%q) = %+v, want nil", body, e)
		}
	}
}

func TestContextLengthMatching(t *testing.T) {
	t.Parallel()
	m, err := NewMatchers([]string{
		"maximum context length",
		"context_length_exceeded",
		"prompt is too long",
		"too many tokens",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Real wordings from OpenAI, vLLM, llama.cpp and Anthropic-compatible
	// gateways.
	shouldMatch := []string{
		`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 131204 tokens.","code":"context_length_exceeded"}}`,
		`{"object":"error","message":"This model's maximum context length is 4096 tokens. However, you requested 5000 tokens."}`,
		`{"error":{"message":"prompt is too long: 210000 tokens > 200000 maximum"}}`,
		`{"message":"Too many tokens in the request"}`,
	}
	for _, body := range shouldMatch {
		which, ok := m.Match(Parse([]byte(body)), []byte(body))
		if !ok {
			t.Errorf("did not recognise a context overflow in %s", body)
			continue
		}
		if which == "" {
			t.Error("Match must name the pattern that fired, so a false positive is diagnosable")
		}
	}

	// Errors that merely mention tokens or limits must not be swept up.
	shouldNotMatch := []string{
		`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`,
		`{"error":{"message":"Rate limit reached for gpt-4o","code":"rate_limit_exceeded"}}`,
		`{"error":{"message":"The model does not exist","code":"model_not_found"}}`,
	}
	for _, body := range shouldNotMatch {
		if which, ok := m.Match(Parse([]byte(body)), []byte(body)); ok {
			t.Errorf("false positive on %s (matched %s)", body, which)
		}
	}
}

// The code is authoritative; prose matching is only the fallback.
func TestCodeBeatsProse(t *testing.T) {
	t.Parallel()
	m, err := NewMatchers(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"error":{"message":"something entirely different","code":"context_length_exceeded"}}`
	which, ok := m.Match(Parse([]byte(body)), []byte(body))
	if !ok {
		t.Fatal("an explicit context_length_exceeded code must match with no patterns configured")
	}
	if which != "code=context_length_exceeded" {
		t.Errorf("matched by %q, want the code to be named", which)
	}
}

func TestMatchFallsBackToRawBody(t *testing.T) {
	t.Parallel()
	m, err := NewMatchers([]string{"maximum context length"})
	if err != nil {
		t.Fatal(err)
	}
	// Not a JSON envelope at all, so Parse returns nil and the raw bytes are
	// the only thing left to match against.
	body := []byte("upstream error: maximum context length is 8192 tokens")
	if _, ok := m.Match(nil, body); !ok {
		t.Error("a non-JSON body should still be matched against the raw bytes")
	}
}

func TestNewMatchersRejectsBadPattern(t *testing.T) {
	t.Parallel()
	if _, err := NewMatchers([]string{"a("}); err == nil {
		t.Error("an uncompilable pattern must be reported")
	}
}

func TestNilMatcherIsSafe(t *testing.T) {
	t.Parallel()
	var m *Matchers
	if _, ok := m.Match(nil, []byte("maximum context length")); ok {
		t.Error("a nil Matchers must not match")
	}
}
