// Package auth guards the proxy's own listener with static bearer tokens.
//
// This exists so llm-proxy can be put behind something like Tailscale Funnel
// without standing open on the public internet. It is deliberately the simplest
// thing that is actually safe: a fixed set of secrets, compared in constant
// time, with no sessions, no expiry and no user model.
//
// There is deliberately no "trust loopback" exemption. Funnel — and every other
// tunnel of this shape — forwards to a local port, so requests from the public
// internet arrive with a loopback source address. An exemption for local
// traffic would exempt precisely the traffic that needs checking.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// HeaderName is the dedicated header a client may present its token in, for
// when the Authorization header is already carrying the vendor's own key.
const HeaderName = "X-LLM-Proxy-Key"

// Token is one accepted credential. The secret is stored only as a digest, so
// a memory dump or an accidental struct print cannot reveal it.
type Token struct {
	Name   string
	digest [32]byte
}

// NewToken builds a token from its secret.
func NewToken(name, secret string) Token {
	return Token{Name: name, digest: sha256.Sum256([]byte(secret))}
}

// Authenticator checks presented credentials against the configured tokens.
type Authenticator struct {
	tokens []Token
}

// New returns an Authenticator. With no tokens it is disabled and admits
// everything, which is the right default for a tool bound to localhost.
func New(tokens []Token) *Authenticator { return &Authenticator{tokens: tokens} }

// Enabled reports whether any token is configured.
func (a *Authenticator) Enabled() bool { return a != nil && len(a.tokens) > 0 }

// Result describes the outcome of a check.
type Result struct {
	// OK is true when the request may proceed.
	OK bool
	// Name identifies which configured token matched, so the log can say which
	// device is talking without revealing the secret.
	Name string
	// Via names the header the token arrived in, or "" when none was presented.
	Via string
	// Presented reports whether the client offered a credential at all. It
	// separates "you sent nothing" from "you sent the wrong thing", which are
	// different mistakes and deserve different messages.
	Presented bool
}

// Check validates a request's credentials.
func (a *Authenticator) Check(r *http.Request) Result {
	if !a.Enabled() {
		return Result{OK: true}
	}

	secret, via := extract(r)
	if secret == "" {
		return Result{}
	}

	// Every token is compared, and the loop does not exit early, so the time
	// taken does not reveal how many tokens were tried or which one matched.
	// Comparing fixed-length digests rather than the secrets themselves also
	// keeps the token's length out of the timing.
	presented := sha256.Sum256([]byte(secret))
	matched := 0
	name := ""
	for i := range a.tokens {
		if subtle.ConstantTimeCompare(presented[:], a.tokens[i].digest[:]) == 1 {
			matched = 1
			name = a.tokens[i].Name
		}
	}

	return Result{OK: matched == 1, Name: name, Via: via, Presented: true}
}

// extract pulls a credential out of the request.
//
// Two places are accepted. `Authorization: Bearer …` is what every OpenAI
// client already sends, so pointing a harness at the proxy needs no special
// configuration. The dedicated header exists for the case where Authorization
// is already carrying the vendor's own key, which a route may be configured to
// forward.
func extract(r *http.Request) (secret, via string) {
	if v := strings.TrimSpace(r.Header.Get(HeaderName)); v != "" {
		return v, HeaderName
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return "", ""
	}

	scheme, rest, found := strings.Cut(authz, " ")
	switch {
	case found && strings.EqualFold(scheme, "Bearer"):
		// "Bearer" with nothing after it is an empty credential, not a token
		// whose value happens to be blank.
		return strings.TrimSpace(rest), "Authorization"
	case strings.EqualFold(authz, "Bearer"):
		return "", ""
	default:
		// A bare value with no scheme is accepted too; some tools send one.
		return authz, "Authorization"
	}
}

// StripCredential removes the header the token arrived in, so the proxy's own
// credential is never forwarded to the vendor.
//
// Without this a route with no upstream key of its own would pass the proxy
// token straight through to the LLM provider, handing a third party the secret
// that guards this listener.
func StripCredential(h http.Header, via string) {
	switch via {
	case HeaderName:
		h.Del(HeaderName)
	case "Authorization":
		h.Del("Authorization")
	}
	// The dedicated header is never of interest upstream, whichever header was
	// actually used.
	h.Del(HeaderName)
}
