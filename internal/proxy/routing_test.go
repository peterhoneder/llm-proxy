package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

func TestJoinPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base, rest, want string
	}{
		{"", "/v1/chat/completions", "/v1/chat/completions"},
		{"/", "/v1/chat/completions", "/v1/chat/completions"},
		// A base path is preserved, which is how a vendor that hosts its API
		// under a sub-path is reached.
		{"/openai", "/v1/models", "/openai/v1/models"},
		{"/openai/", "/v1/models", "/openai/v1/models"},
		{"/openai", "", "/openai"},
		{"/openai", "/", "/openai"},
		{"", "", "/"},
		{"", "v1/models", "/v1/models"},
	}
	for _, tc := range tests {
		t.Run(tc.base+"|"+tc.rest, func(t *testing.T) {
			t.Parallel()
			got, _ := joinPath(tc.base, tc.rest)
			if got != tc.want {
				t.Errorf("joinPath(%q, %q) = %q, want %q", tc.base, tc.rest, got, tc.want)
			}
		})
	}
}

func TestUpstreamURL(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://api.example.com")

	tests := []struct {
		in   string
		want string
	}{
		{"/vendor/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"/vendor/v1/models", "https://api.example.com/v1/models"},
		{"/vendor/v1/models?limit=10", "https://api.example.com/v1/models?limit=10"},
		{"/vendor/", "https://api.example.com/"},
		{"/vendor", "https://api.example.com/"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			in, err := url.Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := upstreamURL(base, "vendor", in).String(); got != tc.want {
				t.Errorf("upstreamURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// An encoded separator must survive the rewrite: silently decoding %2F would
// change which resource the request addresses.
func TestUpstreamURLPreservesEncoding(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://api.example.com")
	in, err := url.Parse("/vendor/v1/models/org%2Fmodel-name")
	if err != nil {
		t.Fatal(err)
	}
	got := upstreamURL(base, "vendor", in).String()
	if !strings.Contains(got, "org%2Fmodel-name") {
		t.Errorf("upstream URL = %q, want the percent-encoded segment preserved", got)
	}
}

// A base path ending in /v1 plus a client that also sends /v1 is the documented
// foot-gun; the join itself must not paper over it, because config validation
// is what warns about it.
func TestUpstreamURLDoublesV1WhenConfigured(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://api.example.com/v1")
	in, _ := url.Parse("/vendor/v1/chat/completions")
	if got := upstreamURL(base, "vendor", in).String(); got != "https://api.example.com/v1/v1/chat/completions" {
		t.Errorf("upstreamURL = %q; the join must be literal so the config warning is the fix", got)
	}
}

func TestIsChatCompletions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/vendor/v1/chat/completions", true},
		{"POST", "/vendor/v1/chat/completions/", true},
		{"GET", "/vendor/v1/chat/completions", false},
		{"POST", "/vendor/v1/completions", false},
		{"POST", "/vendor/v1/embeddings", false},
		{"GET", "/vendor/v1/models", false},
	}
	for _, tc := range tests {
		if got := isChatCompletions(tc.method, tc.path); got != tc.want {
			t.Errorf("isChatCompletions(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// Every path under a route prefix is proxied. A harness calls /v1/models at
// startup, and a 404 there breaks it before a single completion is attempted.
func TestNonChatPathsAreProxied(t *testing.T) {
	t.Parallel()
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream saw path %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m-1"}]}`)
	})
	h := newHarnessWithUpstream(t, upstream)

	snap := h.do(t, "GET", "/vendor/v1/models", "")

	if snap.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", snap.Status)
	}
	if snap.Fault != nil {
		t.Errorf("unexpected fault: %s", snap.Fault.Kind)
	}
	if snap.Chat {
		t.Error("a non-chat path must not be treated as a chat completion")
	}
}

func TestUnknownRouteReturnsParseableError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(1, testutil.EndDone))

	resp, err := http.Post("http://"+h.addr+"/nope/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", resp.StatusCode)
	}
	// An HTML error page here would surface in a harness's logs as an
	// unreadable parse failure rather than as a routing mistake.
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 404 body is not JSON an OpenAI client can parse: %v", err)
	}
	if body.Error.Code != "route_not_found" {
		t.Errorf("code = %q, want route_not_found", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "/vendor/v1") {
		t.Errorf("message = %q, want it to list the configured routes", body.Error.Message)
	}
}

func TestHealthAndRoutesEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.StreamOf(1, testutil.EndDone))

	resp, err := http.Get("http://" + h.addr + "/_proxy/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}

	resp, err = http.Get("http://" + h.addr + "/_proxy/routes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var routes struct {
		Routes []struct {
			Name    string `json:"name"`
			BaseURL string `json:"client_base_url"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Routes) != 1 || routes.Routes[0].Name != "vendor" {
		t.Errorf("routes = %+v, want the one configured route", routes.Routes)
	}
	if !strings.HasSuffix(routes.Routes[0].BaseURL, "/vendor/v1") {
		t.Errorf("client_base_url = %q, want it to include the /v1 a client appends",
			routes.Routes[0].BaseURL)
	}
}

func TestQueryStringIsPreserved(t *testing.T) {
	t.Parallel()
	var seen string
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		_, _ = io.WriteString(w, "{}")
	})
	h := newHarnessWithUpstream(t, upstream)

	h.do(t, "GET", "/vendor/v1/models?limit=10&after=abc%20def", "")

	if seen != "limit=10&after=abc%20def" {
		t.Errorf("upstream query = %q, want it forwarded verbatim", seen)
	}
}
