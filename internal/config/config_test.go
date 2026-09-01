package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llm-proxy.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimalRoute = `
routes:
  - name: vendor
    upstream: https://api.example.com
`

func TestEmbeddedDefaultsAreValid(t *testing.T) {
	t.Parallel()
	c, err := DefaultConfig()
	if err != nil {
		t.Fatalf("embedded defaults do not decode: %v", err)
	}
	if c.Listen == "" {
		t.Error("embedded defaults leave listen empty")
	}
	if Dur(c.Defaults.Timeouts.Connect) == 0 {
		t.Error("embedded defaults leave timeouts.connect unset")
	}
	if len(c.ContextLengthPatterns) == 0 {
		t.Error("embedded defaults carry no context_length_patterns")
	}
	// The defaults alone must fail validation: a proxy with no routes is a
	// configuration error, not a usable default.
	if _, err := c.Validate(); err == nil {
		t.Error("defaults with no routes should not validate")
	}
}

func TestDefaultsMergeIntoRoutes(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, minimalRoute))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := c.Routes[0]
	if got, want := Dur(r.Timeouts.Connect), 10*time.Second; got != want {
		t.Errorf("connect = %v, want %v (inherited)", got, want)
	}
	if got, want := r.APIKeyPrefix, "Bearer "; got != want {
		t.Errorf("api_key_prefix = %q, want %q (inherited)", got, want)
	}
	if got, want := Int(r.MaxIdleConnsPerHost), 32; got != want {
		t.Errorf("max_idle_conns_per_host = %d, want %d — Go's own default of 2 would "+
			"make the proxy churn connections and manufacture the noise it measures", got, want)
	}
	if !Bool(r.AbortOnTruncation) {
		t.Error("abort_on_truncation should default on")
	}
	if r.Retry != nil {
		t.Error("retry must stay nil by default: passthrough is the transparent behaviour")
	}
}

func TestRouteOverridesBeatDefaults(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, `
defaults:
  timeouts:
    stream_idle: 60s
routes:
  - name: slow
    upstream: https://api.example.com
    timeouts:
      stream_idle: 5m
  - name: normal
    upstream: https://api.example.com
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := Dur(c.Routes[0].Timeouts.StreamIdle), 5*time.Minute; got != want {
		t.Errorf("overridden stream_idle = %v, want %v", got, want)
	}
	if got, want := Dur(c.Routes[1].Timeouts.StreamIdle), 60*time.Second; got != want {
		t.Errorf("inherited stream_idle = %v, want %v", got, want)
	}
}

// A false override must survive the defaults merge, which is why the optional
// booleans are pointers.
func TestExplicitFalseIsNotTreatedAsUnset(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, `
routes:
  - name: vendor
    upstream: https://api.example.com
    abort_on_truncation: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Bool(c.Routes[0].AbortOnTruncation) {
		t.Error("explicit abort_on_truncation: false was overwritten by the default true")
	}
}

// The proxy must never be the side that gives up first. A reasoning model can
// take ten minutes to its first token, and a deadline of the proxy's own would
// report its impatience as a vendor failure — destroying the observation the
// tool exists to make.
func TestShippedDefaultsImposeNoDeadlineOnALongRequest(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, minimalRoute))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := c.Routes[0]

	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"timeouts.response_header", Dur(r.Timeouts.ResponseHeader)},
		{"timeouts.stream_idle", Dur(r.Timeouts.StreamIdle)},
		{"timeouts.total", Dur(r.Timeouts.Total)},
	} {
		if tc.got != 0 {
			t.Errorf("%s defaults to %v; it must default to no limit, or a slow "+
				"first token is cut off by the proxy rather than observed", tc.name, tc.got)
		}
	}

	// Connecting is still bounded — nothing useful happens after ten seconds of
	// failing to open a socket.
	if Dur(r.Timeouts.Connect) == 0 {
		t.Error("timeouts.connect should stay bounded")
	}
	// And the progress heartbeat must be on, or a ten-minute wait shows nothing.
	if Dur(r.Timeouts.GapWarn) == 0 {
		t.Error("timeouts.gap_warn must default on so a long wait is visible")
	}
}

// Zero means "no limit" and must survive the defaults merge, which is why the
// timeout fields are pointers.
func TestExplicitZeroTimeoutIsNotTreatedAsUnset(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, `
defaults:
  timeouts:
    stream_idle: 60s
routes:
  - name: patient
    upstream: https://api.example.com
    timeouts:
      stream_idle: 0s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := Dur(c.Routes[0].Timeouts.StreamIdle); got != 0 {
		t.Errorf("stream_idle = %v, want 0 — an explicit zero disables the deadline "+
			"and must not inherit the default back", got)
	}
}

func TestUnknownFieldIsRejectedAndNamed(t *testing.T) {
	t.Parallel()
	_, _, err := Load(writeConfig(t, `
routes:
  - name: vendor
    upstream: https://api.example.com
    stream_idle_timout: 30s
`))
	if err == nil {
		t.Fatal("a misspelled field must not be silently ignored")
	}
	// Naming the offending key is the whole point; a bare "invalid config"
	// would send the user hunting.
	if !strings.Contains(err.Error(), "stream_idle_timout") {
		t.Errorf("error must name the offending key, got: %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"no routes", "routes: []", "no routes configured"},
		{"empty name", "routes:\n  - upstream: https://a.example.com", "name must not be empty"},
		{"slash in name", "routes:\n  - name: a/b\n    upstream: https://a.example.com", "may only contain"},
		{"reserved name", "routes:\n  - name: _proxy\n    upstream: https://a.example.com", "reserved"},
		{
			"duplicate names",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n  - name: a\n    upstream: https://b.example.com",
			"duplicate route name",
		},
		{"empty upstream", "routes:\n  - name: a\n    upstream: \"\"", "upstream must not be empty"},
		{"scheme-less upstream", "routes:\n  - name: a\n    upstream: api.example.com", "must start with http"},
		{"upstream with query", "routes:\n  - name: a\n    upstream: https://a.example.com?x=1", "query string"},
		{"bad log level", minimalRoute + "log:\n  level: verbose\n", "log.level"},
		{"bad expect_done", "routes:\n  - name: a\n    upstream: https://a.example.com\n    expect_done: maybe", "expect_done"},
		{
			"retry on 2xx",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    retry:\n      on: [200]",
			"only 4xx and 5xx",
		},
		{
			"retry max_attempts zero",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    retry:\n      max_attempts: -1",
			"max_attempts",
		},
		{"bad pattern", minimalRoute + "context_length_patterns:\n  - \"a(\"\n", "does not compile"},
		{
			"strip_params empty entry",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    strip_params:\n      - \"\"",
			"strip_params[0] is empty",
		},
		{
			"strip_params padded key",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    strip_params:\n      - \" store \"",
			"whitespace",
		},
		{
			"strip_params duplicate key",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    strip_params:\n      - store\n      - store",
			"twice",
		},
		{
			"strip_params messages",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    strip_params:\n      - messages",
			"must not contain \"messages\"",
		},
		{
			"strip_params stream",
			"routes:\n  - name: a\n    upstream: https://a.example.com\n    strip_params:\n      - stream",
			"must not contain \"stream\"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// strip_params is the one setting that makes the proxy edit what a client sent,
// so configuring it has to be visible at startup rather than only in a report
// somebody may never read.
func TestStripParamsWarnsThatRequestsAreRewritten(t *testing.T) {
	t.Parallel()
	c, warns, err := Load(writeConfig(t, `
routes:
  - name: nebul
    upstream: https://api.nebul.example
    strip_params:
      - prompt_cache_key
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Routes[0].StripParams; len(got) != 1 || got[0] != "prompt_cache_key" {
		t.Errorf("strip_params = %v, want [prompt_cache_key]", got)
	}
	if !hasWarning(warns, "prompt_cache_key") {
		t.Errorf("configuring strip_params produced no warning: %+v", warns)
	}
}

// Like every other route setting, it can be set once under defaults.
func TestStripParamsInheritsFromDefaults(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, `
defaults:
  strip_params:
    - prompt_cache_key
routes:
  - name: inherits
    upstream: https://a.example.com
  - name: overrides
    upstream: https://b.example.com
    strip_params: []
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Routes[0].StripParams; len(got) != 1 || got[0] != "prompt_cache_key" {
		t.Errorf("route[0].strip_params = %v, want it inherited", got)
	}
	// An explicit empty list is a decision, not an absence: a route that says
	// it strips nothing must not have the default handed back to it.
	if got := c.Routes[1].StripParams; len(got) != 0 {
		t.Errorf("route[1].strip_params = %v, want an explicit empty list to win", got)
	}
}

// A missing key must not stop the proxy from starting: watching the 401 happen
// is more useful than a refusal to boot.
func TestMissingAPIKeyWarnsButDoesNotFail(t *testing.T) {
	t.Parallel()
	_, warns, err := Load(writeConfig(t, `
routes:
  - name: vendor
    upstream: https://api.example.com
    api_key_env: DEFINITELY_UNSET_KEY_FOR_TEST
`))
	if err != nil {
		t.Fatalf("a missing API key must not be fatal, got: %v", err)
	}
	if !hasWarning(warns, "DEFINITELY_UNSET_KEY_FOR_TEST") {
		t.Errorf("expected a warning naming the unset variable, got %v", warns)
	}
}

// The /v1/v1 foot-gun: clients append /v1 themselves.
func TestUpstreamEndingInV1Warns(t *testing.T) {
	t.Parallel()
	_, warns, err := Load(writeConfig(t, `
routes:
  - name: vendor
    upstream: https://api.example.com/v1
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !hasWarning(warns, "/v1/v1") {
		t.Errorf("expected a warning about the doubled /v1, got %v", warns)
	}
}

func TestProviderDerivedFromUpstream(t *testing.T) {
	t.Parallel()
	c, _, err := Load(writeConfig(t, `
routes:
  - name: a
    upstream: https://api.mistral.ai
  - name: b
    upstream: https://llm.example-vendor.dev
  - name: c
    upstream: https://api.example.com
    provider: my-provider
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i, want := range []string{"mistral", "example-vendor", "my-provider"} {
		if got := c.Routes[i].Provider; got != want {
			t.Errorf("routes[%d].provider = %q, want %q", i, got, want)
		}
	}
}

func TestEnvOverridesFile(t *testing.T) {
	// Not parallel: mutates the environment.
	t.Setenv("LLM_PROXY_LISTEN", "0.0.0.0:9999")
	t.Setenv("LLM_PROXY_LOG_LEVEL", "debug")
	c, _, err := Load(writeConfig(t, minimalRoute+"listen: 127.0.0.1:1234\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "0.0.0.0:9999" {
		t.Errorf("listen = %q, want the environment to win", c.Listen)
	}
	if c.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug", c.Log.Level)
	}
}

func TestNoColorEnvForcesColorOff(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	c, _, err := Load(writeConfig(t, minimalRoute))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Log.Color != "never" {
		t.Errorf("log.color = %q, want never when NO_COLOR is set", c.Log.Color)
	}
}

func TestDurationParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30s", 30 * time.Second, false},
		{"1m30s", 90 * time.Second, false},
		{"500ms", 500 * time.Millisecond, false},
		{"0s", 0, false},
		{"soon", 0, true},
		{"30", 0, true}, // a bare number is ambiguous; require a unit
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			var c Config
			err := strictDecode([]byte("shutdown_grace: \""+tc.in+"\"\n"), &c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q should not parse", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got := c.ShutdownGrace.D(); got != tc.want {
				t.Errorf("%q = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBytesParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"64KiB", 64 << 10, false},
		{"8MiB", 8 << 20, false},
		{"8mib", 8 << 20, false},
		{"1GiB", 1 << 30, false},
		{"512B", 512, false},
		{"-1", 0, true},
		{"lots", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			var c Config
			err := strictDecode([]byte("log:\n  max_body_bytes: \""+tc.in+"\"\n"), &c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q should not parse", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got := c.Log.MaxBodyBytes.Int64(); got != tc.want {
				t.Errorf("%q = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestBytesString(t *testing.T) {
	t.Parallel()
	for in, want := range map[Bytes]string{
		8 << 20:  "8MiB",
		64 << 10: "64KiB",
		1500:     "1500",
	} {
		if got := in.String(); got != want {
			t.Errorf("Bytes(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestFindConfigPrefersFlagThenEnv(t *testing.T) {
	p := writeConfig(t, minimalRoute)
	if got := FindConfig(p); got != p {
		t.Errorf("FindConfig(flag) = %q, want %q", got, p)
	}
	t.Setenv("LLM_PROXY_CONFIG", p)
	if got := FindConfig(""); got != p {
		t.Errorf("FindConfig(env) = %q, want %q", got, p)
	}
}

func hasWarning(warns []Warning, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w.Text, substr) {
			return true
		}
	}
	return false
}

// Fail closed. Starting unauthenticated because an environment variable was
// missing is the one outcome worth refusing to boot over — it is silent, and it
// leaves the listener open exactly when the operator believed it was guarded.
func TestAuthFailsClosedWhenTokenIsMissing(t *testing.T) {
	_, _, err := Load(writeConfig(t, minimalRoute+`
auth:
  tokens:
    - name: laptop
      env: DEFINITELY_UNSET_PROXY_TOKEN
`))
	if err == nil {
		t.Fatal("a configured token whose variable is unset must refuse to start")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_UNSET_PROXY_TOKEN") {
		t.Errorf("error = %v, want it to name the missing variable", err)
	}
	if !strings.Contains(err.Error(), "refusing to start unauthenticated") {
		t.Errorf("error = %v, want it to say why this is fatal", err)
	}
}

func TestAuthRequiredCanBeRelaxed(t *testing.T) {
	_, _, err := Load(writeConfig(t, minimalRoute+`
auth:
  required: false
  tokens:
    - name: laptop
      env: DEFINITELY_UNSET_PROXY_TOKEN
`))
	if err != nil {
		t.Fatalf("required: false should allow starting without the token: %v", err)
	}
}

func TestAuthTokenResolvesFromEnv(t *testing.T) {
	t.Setenv("TEST_PROXY_TOKEN", "example-proxy-token-not-a-real")
	c, _, err := Load(writeConfig(t, minimalRoute+`
auth:
  tokens:
    - name: laptop
      env: TEST_PROXY_TOKEN
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, missing := c.ResolvedTokens()
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(resolved) != 1 || resolved[0].Name != "laptop" {
		t.Fatalf("resolved = %+v, want one token named laptop", resolved)
	}
	if resolved[0].Secret != "example-proxy-token-not-a-real" {
		t.Error("the secret was not read from the environment")
	}
}

// The zero-config path: turning auth on without editing the file at all.
func TestAuthTokensFromEnvShortcut(t *testing.T) {
	t.Setenv("LLM_PROXY_TOKENS", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	c, _, err := Load(writeConfig(t, minimalRoute))
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := c.ResolvedTokens()
	if len(resolved) != 2 {
		t.Fatalf("resolved %d tokens, want 2 from the comma-separated list", len(resolved))
	}
}

func TestShortTokenWarns(t *testing.T) {
	t.Setenv("LLM_PROXY_TOKENS", "hunter2")
	_, warns, err := Load(writeConfig(t, minimalRoute))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warns, "openssl rand") {
		t.Errorf("warnings = %v, want a warning about the short token", warns)
	}
}

func TestInlineTokenWarns(t *testing.T) {
	_, warns, err := Load(writeConfig(t, minimalRoute+`
auth:
  tokens:
    - name: laptop
      value: example-proxy-token-not-a-real
`))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warns, "not committed") {
		t.Errorf("warnings = %v, want a warning about the inline secret", warns)
	}
}

// The safety net for the case this feature exists for: a listener reachable
// from off-box with nothing guarding it.
func TestNonLoopbackWithoutAuthWarns(t *testing.T) {
	_, warns, err := Load(writeConfig(t, minimalRoute+"listen: \"0.0.0.0:8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warns, "no auth tokens configured") {
		t.Errorf("warnings = %v, want a warning about the exposed listener", warns)
	}
}

func TestLoopbackWithoutAuthIsQuiet(t *testing.T) {
	_, warns, err := Load(writeConfig(t, minimalRoute+"listen: \"127.0.0.1:8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(warns, "no auth tokens configured") {
		t.Errorf("warnings = %v, want no exposure warning for a loopback listener", warns)
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	for listen, want := range map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		"[::]:8080":      false,
		":8080":          false,
		"192.168.1.5:80": false,
		// Tailscale hands out addresses in 100.64.0.0/10; reachable from other
		// machines, so emphatically not loopback.
		"100.101.102.103:8080": false,
	} {
		if got := isLoopback(listen); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

func TestDefaultListenPort(t *testing.T) {
	t.Parallel()
	c, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:14701" {
		t.Errorf("listen = %q, want 127.0.0.1:14701", c.Listen)
	}
	if !isLoopback(c.Listen) {
		t.Error("the default must bind to loopback, since the default has no auth tokens")
	}
}

// An explicit false must beat the environment. Without this, anything building
// a config in-process — the demo, the test harness — inherits the operator's
// LLM_PROXY_TOKENS and starts rejecting its own requests.
func TestAuthEnabledFalseIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("LLM_PROXY_TOKENS", "example-proxy-token-not-a-real")

	c, warns, err := Load(writeConfig(t, minimalRoute+"auth:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := c.ResolvedTokens(); len(resolved) != 0 {
		t.Errorf("resolved %d tokens with auth.enabled=false, want none", len(resolved))
	}
	// The listener is now unguarded, so the exposure warning still applies on a
	// public address — but this one is loopback, so it should stay quiet.
	if hasWarning(warns, "no auth tokens configured") {
		t.Errorf("unexpected exposure warning for a loopback listener: %v", warns)
	}
}

// Configuring tokens and then disabling auth is almost certainly a mistake.
func TestAuthEnabledFalseWithTokensWarns(t *testing.T) {
	_, warns, err := Load(writeConfig(t, minimalRoute+`
auth:
  enabled: false
  tokens:
    - name: laptop
      value: example-proxy-token-not-a-real
`))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warns, "tokens are ignored") {
		t.Errorf("warnings = %v, want one saying the tokens are ignored", warns)
	}
}

func TestAuthEnabledRejectsGarbage(t *testing.T) {
	_, _, err := Load(writeConfig(t, minimalRoute+"auth:\n  enabled: maybe\n"))
	if err == nil || !strings.Contains(err.Error(), "auth.enabled") {
		t.Errorf("err = %v, want it to reject an invalid auth.enabled", err)
	}
}
