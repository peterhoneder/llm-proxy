package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

func withStrip(keys ...string) func(*config.Route) {
	return func(r *config.Route) { r.StripParams = keys }
}

// The case the feature exists for: a vendor answers
//
//	{"message":"Validation: Unsupported parameter(s): `prompt_cache_key`", ...}
//
// and the client sends the parameter unconditionally.
func TestStripParamsRemovesTheParameterTheVendorRejects(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key"),
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	snap := h.chat(t, `{"model":"m","prompt_cache_key":"abc","messages":[{"role":"user","content":"hi"}]}`)

	seen := h.upstream.Seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if bytes.Contains(seen[0].Body, []byte("prompt_cache_key")) {
		t.Errorf("the vendor still received the parameter: %s", seen[0].Body)
	}
	if snap.Fault != nil {
		t.Errorf("unexpected fault: %s — %s", snap.Fault.Kind, snap.Fault.Detail)
	}

	// Everything else has to survive, or the shim broke the request it was
	// supposed to rescue.
	var got map[string]any
	if err := json.Unmarshal(seen[0].Body, &got); err != nil {
		t.Fatalf("the forwarded body is not valid JSON: %v\n%s", err, seen[0].Body)
	}
	if got["model"] != "m" {
		t.Errorf("model = %v, want it untouched", got["model"])
	}
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("messages = %v, want the one message the client sent", got["messages"])
	}

	// A rewritten body must go out under its own length, not the client's, and
	// not as a chunked stream — the wire form is evidence in this tool.
	if te := seen[0].Header.Get("Transfer-Encoding"); te != "" {
		t.Errorf("the rewritten request went out chunked (%q), changing the wire form", te)
	}
	if cl := seen[0].Header.Get("Content-Length"); cl != "" &&
		cl != strconv.Itoa(len(seen[0].Body)) {
		t.Errorf("Content-Length = %q, want %d — the length of what was actually sent",
			cl, len(seen[0].Body))
	}

	if want := []string{"prompt_cache_key"}; !equalStrings(snap.StrippedParams, want) {
		t.Errorf("StrippedParams = %v, want %v — a rewritten request must be reported as rewritten",
			snap.StrippedParams, want)
	}
	if snap.ReqBodyBytes != int64(len(seen[0].Body)) {
		t.Errorf("ReqBodyBytes = %d, want %d — the report should describe what went upstream",
			snap.ReqBodyBytes, len(seen[0].Body))
	}
}

// The whole product rests on passing bytes through unaltered. Nothing may be
// rewritten unless a route asked for it by name.
func TestStripParamsIsOffByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	body := `{"model":"m","prompt_cache_key":"abc","messages":[]}`
	snap := h.chat(t, body)

	seen := h.upstream.Seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if string(seen[0].Body) != body {
		t.Errorf("body = %s, want the client's bytes unaltered", seen[0].Body)
	}
	if len(snap.StrippedParams) != 0 {
		t.Errorf("StrippedParams = %v, want none", snap.StrippedParams)
	}
}

// A configured key that is not in this particular body must not cost the
// request a re-encode: an unrelated request going through a stripping route
// still deserves byte-for-byte passthrough.
func TestStripParamsLeavesTheBodyAloneWhenTheKeyIsAbsent(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key"),
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	body := `{ "model":"m",  "messages":[] }`
	snap := h.chat(t, body)

	seen := h.upstream.Seen()
	if string(seen[0].Body) != body {
		t.Errorf("body = %s, want it untouched down to the whitespace", seen[0].Body)
	}
	if len(snap.StrippedParams) != 0 {
		t.Errorf("StrippedParams = %v, want none: the key was not there", snap.StrippedParams)
	}
}

// Everything the rewrite keeps must survive byte-for-byte. Number precision and
// prompts full of XML tags are the two that a naive re-encode quietly mangles.
func TestStripParamsPreservesTheValuesItKeeps(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key"),
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	const prompt = `<instructions>use a & b, not a>b</instructions>`
	h.chat(t, `{"model":"m","prompt_cache_key":"abc","temperature":0.10000000000000000555,`+
		`"messages":[{"role":"user","content":"`+prompt+`"}],"metadata":{"nested":{"deep":[1,2,3]}}}`)

	got := h.upstream.Seen()[0].Body
	if !bytes.Contains(got, []byte(prompt)) {
		t.Errorf("the prompt was re-escaped on the way through:\n%s", got)
	}
	if !bytes.Contains(got, []byte("0.10000000000000000555")) {
		t.Errorf("a number lost precision in the rewrite:\n%s", got)
	}
	if !bytes.Contains(got, []byte(`{"nested":{"deep":[1,2,3]}}`)) {
		t.Errorf("a nested value was reshaped:\n%s", got)
	}
}

// Retry replays whatever went out the first time. If the two attempts differed,
// the second would not be the same request and nothing could be concluded from
// comparing them.
func TestStripParamsSurvivesARetryIdentically(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, func(r *config.Route) {
		withRetry(3, time.Minute)(r)
		withStrip("prompt_cache_key")(r)
	},
		testutil.Script{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`},
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`},
	)

	h.chat(t, `{"model":"m","prompt_cache_key":"abc","messages":[]}`)

	seen := h.upstream.Seen()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	if !bytes.Equal(seen[0].Body, seen[1].Body) {
		t.Errorf("the replayed body differs:\n first: %s\nsecond: %s", seen[0].Body, seen[1].Body)
	}
	if bytes.Contains(seen[1].Body, []byte("prompt_cache_key")) {
		t.Errorf("the retry re-sent the stripped parameter: %s", seen[1].Body)
	}
}

// A vendor that rejects a parameter rejects it everywhere, so the shim is not
// limited to chat completions.
func TestStripParamsAppliesOutsideChatCompletions(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key"),
		testutil.Script{Status: http.StatusOK, Body: `{"data":[]}`})

	h.do(t, "POST", "/vendor/v1/embeddings", `{"model":"e","prompt_cache_key":"abc","input":"hi"}`)

	if got := h.upstream.Seen()[0].Body; bytes.Contains(got, []byte("prompt_cache_key")) {
		t.Errorf("the parameter survived on a non-chat path: %s", got)
	}
}

// There is nothing to strip from a body that is not a JSON object, and
// inventing a rewrite for one is not the proxy's call.
func TestStripParamsLeavesANonJSONBodyAlone(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key"),
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	body := "prompt_cache_key=abc&model=m"
	h.do(t, "POST", "/vendor/v1/anything", body)

	if got := string(h.upstream.Seen()[0].Body); got != body {
		t.Errorf("body = %s, want it forwarded untouched", got)
	}
}

// Past max_request_body the remainder is streamed and never buffered, so there
// is no document to rewrite. Half a rewrite would corrupt the body; the request
// goes through intact and the report says the shim did not run.
func TestStripParamsIsReportedWhenTheBodyIsTooLargeToRewrite(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, func(r *config.Route) {
		withStrip("prompt_cache_key")(r)
		r.MaxRequestBody = 256
	}, testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	body := `{"model":"m","prompt_cache_key":"abc","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 512) + `"}]}`
	snap := h.chat(t, body)

	if got := string(h.upstream.Seen()[0].Body); got != body {
		t.Errorf("an unrewritable body was altered:\n%s", got)
	}
	if len(snap.StrippedParams) != 0 {
		t.Errorf("StrippedParams = %v, want none: nothing was stripped", snap.StrippedParams)
	}
	if !hasWarningKind(snap.Warnings, fault.KindStripSkipped) {
		t.Errorf("no warning that strip_params did not run; warnings = %+v", snap.Warnings)
	}
}

func TestStripParamsRemovesEveryConfiguredKey(t *testing.T) {
	t.Parallel()
	h := newHarnessTuned(t, withStrip("prompt_cache_key", "safety_identifier", "store"),
		testutil.Script{Status: http.StatusOK, Body: `{"id":"ok"}`})

	snap := h.chat(t, `{"model":"m","prompt_cache_key":"a","store":false,"messages":[]}`)

	got := h.upstream.Seen()[0].Body
	for _, k := range []string{"prompt_cache_key", "store"} {
		if bytes.Contains(got, []byte(k)) {
			t.Errorf("%q survived: %s", k, got)
		}
	}
	// Only what was actually present gets reported, so the report never claims
	// to have removed something the client never sent.
	if want := []string{"prompt_cache_key", "store"}; !equalStrings(snap.StrippedParams, want) {
		t.Errorf("StrippedParams = %v, want %v", snap.StrippedParams, want)
	}
}

func TestStripParamsUnit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		body        string
		keys        []string
		want        string
		wantRemoved []string
	}{
		{
			name: "no keys configured",
			body: `{"a":1}`, keys: nil, want: `{"a":1}`,
		},
		{
			name: "key absent leaves the bytes alone",
			body: `{ "a" : 1 }`, keys: []string{"b"}, want: `{ "a" : 1 }`,
		},
		{
			name: "empty body",
			body: ``, keys: []string{"a"}, want: ``,
		},
		{
			name: "JSON null is not an object",
			body: `null`, keys: []string{"a"}, want: `null`,
		},
		{
			name: "a JSON array is not an object",
			body: `[{"a":1}]`, keys: []string{"a"}, want: `[{"a":1}]`,
		},
		{
			name: "only the top level is searched",
			body: `{"a":1,"nested":{"a":2}}`, keys: []string{"a"},
			want: `{"nested":{"a":2}}`, wantRemoved: []string{"a"},
		},
		{
			name: "a null value still counts as present",
			body: `{"a":null,"b":1}`, keys: []string{"a"},
			want: `{"b":1}`, wantRemoved: []string{"a"},
		},
		{
			name: "stripping the only key leaves an empty object",
			body: `{"a":1}`, keys: []string{"a"},
			want: `{}`, wantRemoved: []string{"a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, removed, err := stripParams([]byte(tc.body), tc.keys)
			if err != nil {
				t.Fatalf("stripParams: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
			if !equalStrings(removed, tc.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tc.wantRemoved)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
