package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secret = "example-proxy-token-not-a-real-secret"

func request(headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/vendor/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestDisabledWithNoTokens(t *testing.T) {
	t.Parallel()
	a := New(nil)
	if a.Enabled() {
		t.Error("an authenticator with no tokens must be disabled")
	}
	if !a.Check(request(nil)).OK {
		t.Error("a disabled authenticator must admit everything")
	}
}

func TestAcceptsBearerToken(t *testing.T) {
	t.Parallel()
	a := New([]Token{NewToken("laptop", secret)})

	res := a.Check(request(map[string]string{"Authorization": "Bearer " + secret}))
	if !res.OK {
		t.Fatal("a correct bearer token was rejected")
	}
	if res.Name != "laptop" {
		t.Errorf("Name = %q, want the token's label so the log can say who is calling", res.Name)
	}
	if res.Via != "Authorization" {
		t.Errorf("Via = %q, want Authorization", res.Via)
	}
}

// The dedicated header exists for routes that forward the client's own vendor
// key in Authorization.
func TestAcceptsDedicatedHeader(t *testing.T) {
	t.Parallel()
	a := New([]Token{NewToken("laptop", secret)})

	res := a.Check(request(map[string]string{
		HeaderName:      secret,
		"Authorization": "Bearer sk-the-vendors-own-key",
	}))
	if !res.OK {
		t.Fatal("a correct token in the dedicated header was rejected")
	}
	if res.Via != HeaderName {
		t.Errorf("Via = %q, want %q", res.Via, HeaderName)
	}
}

func TestRejects(t *testing.T) {
	t.Parallel()
	a := New([]Token{NewToken("laptop", secret)})

	tests := []struct {
		name          string
		headers       map[string]string
		wantPresented bool
	}{
		{"no credential", nil, false},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, false},
		{"wrong token", map[string]string{"Authorization": "Bearer nope-not-this-one-at-all"}, true},
		{"wrong dedicated header", map[string]string{HeaderName: "wrong"}, true},
		// A prefix of a valid token must not pass, which a length-insensitive
		// comparison could allow.
		{"prefix of a valid token", map[string]string{"Authorization": "Bearer " + secret[:10]}, true},
		{"valid token plus suffix", map[string]string{"Authorization": "Bearer " + secret + "x"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := a.Check(request(tc.headers))
			if res.OK {
				t.Fatal("the request should have been rejected")
			}
			if res.Presented != tc.wantPresented {
				t.Errorf("Presented = %v, want %v — 'you sent nothing' and 'you sent the "+
					"wrong thing' are different mistakes", res.Presented, tc.wantPresented)
			}
		})
	}
}

func TestAnyConfiguredTokenIsAccepted(t *testing.T) {
	t.Parallel()
	a := New([]Token{
		NewToken("laptop", "laptop-token-000000000000000000"),
		NewToken("phone", "phone-token-1111111111111111111"),
		NewToken("ci", "ci-token-22222222222222222222222"),
	})

	for _, tc := range []struct{ token, want string }{
		{"laptop-token-000000000000000000", "laptop"},
		{"phone-token-1111111111111111111", "phone"},
		{"ci-token-22222222222222222222222", "ci"},
	} {
		res := a.Check(request(map[string]string{"Authorization": "Bearer " + tc.token}))
		if !res.OK || res.Name != tc.want {
			t.Errorf("token for %s: OK=%v Name=%q", tc.want, res.OK, res.Name)
		}
	}
}

// Some tools send the raw value with no scheme.
func TestBareValueWithoutScheme(t *testing.T) {
	t.Parallel()
	a := New([]Token{NewToken("laptop", secret)})
	if !a.Check(request(map[string]string{"Authorization": secret})).OK {
		t.Error("a token sent without a Bearer prefix should still be accepted")
	}
}

func TestSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	a := New([]Token{NewToken("laptop", secret)})
	if !a.Check(request(map[string]string{"Authorization": "bearer " + secret})).OK {
		t.Error("the Bearer scheme is case-insensitive per RFC 9110")
	}
}

// The credential authenticated one hop and must not travel further. A route
// with no key of its own would otherwise hand the proxy's own token to the LLM
// vendor.
func TestStripCredential(t *testing.T) {
	t.Parallel()

	t.Run("authorization", func(t *testing.T) {
		t.Parallel()
		h := http.Header{"Authorization": {"Bearer " + secret}}
		StripCredential(h, "Authorization")
		if h.Get("Authorization") != "" {
			t.Error("the proxy token would have been forwarded upstream")
		}
	})

	t.Run("dedicated header", func(t *testing.T) {
		t.Parallel()
		h := http.Header{
			HeaderName:      {secret},
			"Authorization": {"Bearer sk-the-vendors-own-key"},
		}
		StripCredential(h, HeaderName)
		if h.Get(HeaderName) != "" {
			t.Error("the proxy token would have been forwarded upstream")
		}
		// The vendor's own key was not the credential and must survive.
		if h.Get("Authorization") != "Bearer sk-the-vendors-own-key" {
			t.Error("stripping the proxy token also removed the client's vendor key")
		}
	})

	t.Run("dedicated header is always removed", func(t *testing.T) {
		t.Parallel()
		h := http.Header{HeaderName: {secret}, "Authorization": {"Bearer " + secret}}
		StripCredential(h, "Authorization")
		if h.Get(HeaderName) != "" {
			t.Error("the dedicated header is never of interest upstream and should always go")
		}
	})
}

// The token keeps only a digest, so nothing that prints or dumps it can
// disclose the secret.
func TestTokenDoesNotRetainTheSecret(t *testing.T) {
	t.Parallel()
	tok := NewToken("laptop", secret)

	if strings.Contains(fmt.Sprintf("%+v", tok), secret) {
		t.Error("printing a Token revealed the secret")
	}
	if strings.Contains(string(tok.digest[:]), secret) {
		t.Error("the digest contains the secret verbatim")
	}
	if tok.Name != "laptop" {
		t.Errorf("Name = %q, want the label preserved", tok.Name)
	}
}
