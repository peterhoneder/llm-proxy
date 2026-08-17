package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/auth"
	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/testutil"
)

const proxyToken = "example-proxy-token-not-a-real-secret"

func withToken(name, secret string) func(*config.Config) {
	return func(c *config.Config) {
		c.Auth.Tokens = append(c.Auth.Tokens, config.AuthToken{Name: name, Value: secret})
	}
}

func TestUnauthenticatedRequestIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	resp, err := http.Post("http://"+h.addr+"/vendor/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}

	// The rejection must be something an OpenAI client can parse, not an HTML
	// page that surfaces in a harness log as a JSON decode failure.
	var body struct {
		Error struct{ Message, Code string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 401 body is not parseable JSON: %v", err)
	}
	if body.Error.Code != "invalid_api_key" {
		t.Errorf("code = %q, want invalid_api_key", body.Error.Code)
	}

	// Nothing should have reached the vendor.
	if len(h.upstream.Seen()) != 0 {
		t.Error("an unauthenticated request was forwarded upstream")
	}
}

func TestAuthenticatedRequestIsProxied(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: `{"id":"ok"}`})

	snap := h.authedChat(t, "Bearer "+proxyToken, `{"model":"m","messages":[]}`)

	if snap.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", snap.Status)
	}
	if snap.AuthName != "laptop" {
		t.Errorf("AuthName = %q, want the token's label recorded so the log says who called",
			snap.AuthName)
	}
}

// The most important property here. The proxy's token authenticates one hop; if
// it were forwarded, the LLM vendor would be handed the secret guarding this
// listener.
func TestProxyTokenIsNeverForwardedUpstream(t *testing.T) {
	t.Parallel()

	var seen http.Header
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, "{}")
	})

	// No api_key_env on the route: nothing would overwrite the Authorization
	// header, so an unstripped token would go straight through.
	h := newHarnessWithUpstreamAndAuth(t, upstream,
		withToken("laptop", proxyToken),
		func(r *config.Route) { r.APIKeyEnv = "" })

	h.authedChat(t, "Bearer "+proxyToken, `{"model":"m","messages":[]}`)

	for name, values := range seen {
		for _, v := range values {
			if strings.Contains(v, proxyToken) {
				t.Fatalf("the proxy token was forwarded upstream in %s", name)
			}
		}
	}
	if seen.Get(auth.HeaderName) != "" {
		t.Errorf("%s reached the upstream", auth.HeaderName)
	}
}

// A route that forwards the client's own vendor key needs the token somewhere
// else, which is what the dedicated header is for.
func TestDedicatedHeaderLeavesTheVendorKeyAlone(t *testing.T) {
	t.Parallel()

	var seen http.Header
	upstream := newRawUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, "{}")
	})

	h := newHarnessWithUpstreamAndAuth(t, upstream,
		withToken("laptop", proxyToken),
		func(r *config.Route) { r.APIKeyEnv = "" })

	req, _ := http.NewRequest("POST", "http://"+h.addr+"/vendor/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set(auth.HeaderName, proxyToken)
	req.Header.Set("Authorization", "Bearer sk-the-vendors-own-key")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	h.waitForRecord(t, 3*time.Second)

	if got := seen.Get("Authorization"); got != "Bearer sk-the-vendors-own-key" {
		t.Errorf("upstream Authorization = %q, want the client's own vendor key preserved", got)
	}
	if seen.Get(auth.HeaderName) != "" {
		t.Error("the proxy token reached the upstream")
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	req, _ := http.NewRequest("POST", "http://"+h.addr+"/vendor/v1/chat/completions",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer definitely-not-the-right-token-here")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", resp.StatusCode)
	}
	if len(h.upstream.Seen()) != 0 {
		t.Error("a request with a wrong token was forwarded upstream")
	}
}

// /_proxy/routes discloses upstream URLs and the names of the environment
// variables holding the keys, so it must be behind the same gate.
func TestIntrospectionEndpointsRequireAuth(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	resp, err := http.Get("http://" + h.addr + "/_proxy/routes")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401 — this endpoint lists upstreams", resp.StatusCode)
	}
	if strings.Contains(string(body), "upstream") {
		t.Error("the route table leaked to an unauthenticated caller")
	}
}

// The 404 handler names every configured route, so it needs the gate too.
func TestUnknownPathDoesNotLeakRoutes(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	resp, err := http.Get("http://" + h.addr + "/guessing/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", resp.StatusCode)
	}
	if strings.Contains(string(body), "/vendor") {
		t.Error("the 404 body disclosed the configured routes to an unauthenticated caller")
	}
}

// An uptime probe should not need a credential.
func TestHealthEndpointStaysOpen(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	resp, err := http.Get("http://" + h.addr + "/_proxy/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want the health endpoint reachable without a token", resp.StatusCode)
	}
}

func TestNoTokensMeansOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testutil.Script{Status: 200, Body: "{}"})

	if snap := h.chat(t, `{"model":"m","messages":[]}`); snap.Status != http.StatusOK {
		t.Errorf("Status = %d, want an unguarded proxy to admit the request", snap.Status)
	}
}

// The token must not appear in the report, at any level.
func TestProxyTokenIsNotRendered(t *testing.T) {
	t.Parallel()
	h := newHarnessWithAuth(t, withToken("laptop", proxyToken), testutil.Script{Status: 200, Body: "{}"})

	snap := h.authedChat(t, "Bearer "+proxyToken, `{"model":"m","messages":[]}`)

	if out := h.srv.log.Renderer().Render(snap); strings.Contains(out, proxyToken) {
		t.Errorf("the proxy token appears in the rendered report:\n%s", out)
	}
}
