package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed default_config.yaml
var defaultConfigYAML []byte

// ReservedPrefix is the path namespace llm-proxy serves itself. Route names may
// not collide with it, so a route can never shadow /_proxy/healthz.
const ReservedPrefix = "_proxy"

var routeNameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Warning is a non-fatal configuration problem worth telling the user about.
// A missing API key is the motivating case: refusing to start would be less
// debuggable than letting the 401 happen where it can be observed.
type Warning struct {
	Route string
	Text  string
}

// DefaultConfig returns the embedded defaults with no user file applied.
func DefaultConfig() (*Config, error) {
	var c Config
	if err := strictDecode(defaultConfigYAML, &c); err != nil {
		return nil, fmt.Errorf("embedded default config is invalid (this is a bug): %w", err)
	}
	return &c, nil
}

// Load reads the embedded defaults, layers the user's file on top, applies
// environment overrides and returns the result with any warnings. Flag
// overrides are applied by the caller afterwards via ApplyFlags, because flags
// must win over everything.
//
// An empty path means "defaults only", which is useful in tests.
func Load(path string) (*Config, []Warning, error) {
	c, err := DefaultConfig()
	if err != nil {
		return nil, nil, err
	}

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		// Decoding into the same value merges scalars and maps field by field;
		// sequences (notably routes) are replaced wholesale, which is what a
		// reader expects.
		if err := strictDecode(raw, c); err != nil {
			return nil, nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}

	c.applyRouteDefaults()
	applyEnv(c)

	warnings, err := c.Validate()
	if err != nil {
		return nil, warnings, err
	}
	return c, warnings, nil
}

// FindConfig returns the first config file that exists, searching the flag
// value, $LLM_PROXY_CONFIG, ./llm-proxy.yaml and the XDG config directory.
// It returns "" when none is found, which is not an error by itself.
func FindConfig(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("LLM_PROXY_CONFIG"); env != "" {
		return env
	}
	candidates := []string{"llm-proxy.yaml", "llm-proxy.yml"}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "llm-proxy", "config.yaml"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ApplyRouteDefaultsForTest exposes the defaults merge to other packages'
// tests, which build routes in Go and so bypass the decode path.
func (c *Config) ApplyRouteDefaultsForTest() { c.applyRouteDefaults() }

// applyRouteDefaults copies Config.Defaults into every route field the user
// left unset. Pointer fields distinguish "unset" from "explicitly false".
func (c *Config) applyRouteDefaults() {
	d := c.Defaults
	for i := range c.Routes {
		r := &c.Routes[i]

		if r.Provider == "" {
			r.Provider = d.Provider
		}
		if r.APIKeyHeader == "" {
			r.APIKeyHeader = d.APIKeyHeader
		}
		if r.APIKeyPrefix == "" {
			r.APIKeyPrefix = d.APIKeyPrefix
		}
		if r.ExpectDone == "" {
			r.ExpectDone = d.ExpectDone
		}
		if r.Headers == nil {
			r.Headers = d.Headers
		}
		if r.StripParams == nil {
			r.StripParams = d.StripParams
		}

		inheritBool(&r.ForwardClientAuth, d.ForwardClientAuth)
		inheritBool(&r.HTTP2, d.HTTP2)
		inheritBool(&r.InsecureSkipVerify, d.InsecureSkipVerify)
		inheritBool(&r.DisableKeepalives, d.DisableKeepalives)
		inheritBool(&r.AbortOnTruncation, d.AbortOnTruncation)

		if r.MaxIdleConnsPerHost == nil {
			r.MaxIdleConnsPerHost = d.MaxIdleConnsPerHost
		}
		if r.IdleConnTimeout == 0 {
			r.IdleConnTimeout = d.IdleConnTimeout
		}
		if r.MaxRequestBody == 0 {
			r.MaxRequestBody = d.MaxRequestBody
		}

		inheritDuration(&r.Timeouts.Connect, d.Timeouts.Connect)
		inheritDuration(&r.Timeouts.TLSHandshake, d.Timeouts.TLSHandshake)
		inheritDuration(&r.Timeouts.ResponseHeader, d.Timeouts.ResponseHeader)
		inheritDuration(&r.Timeouts.StreamIdle, d.Timeouts.StreamIdle)
		inheritDuration(&r.Timeouts.ClientWrite, d.Timeouts.ClientWrite)
		inheritDuration(&r.Timeouts.GapWarn, d.Timeouts.GapWarn)
		inheritDuration(&r.Timeouts.Total, d.Timeouts.Total)

		if r.Retry == nil && d.Retry != nil {
			cp := *d.Retry
			r.Retry = &cp
		}
		if r.Retry != nil {
			r.Retry.applyDefaults()
		}
		if r.Provider == "" {
			r.Provider = providerFromUpstream(r.Upstream)
		}
	}
}

func (r *Retry) applyDefaults() {
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 3
	}
	if len(r.On) == 0 {
		r.On = []int{429, 500, 502, 503, 504}
	}
	if r.MaxWait == 0 {
		r.MaxWait = Duration(60e9)
	}
	if r.BaseBackoff == 0 {
		r.BaseBackoff = Duration(500e6)
	}
	if r.MaxBackoff == 0 {
		r.MaxBackoff = Duration(20e9)
	}
	if r.RespectResetHeaders == nil {
		t := true
		r.RespectResetHeaders = &t
	}
}

func inheritBool(dst **bool, src *bool) {
	if *dst == nil && src != nil {
		v := *src
		*dst = &v
	}
}

// inheritDuration fills in an unset duration. An explicit zero is preserved,
// because zero means "no limit" rather than "not configured".
func inheritDuration(dst **Duration, src *Duration) {
	if *dst == nil && src != nil {
		v := *src
		*dst = &v
	}
}

// providerFromUpstream derives a gen_ai.provider.name from the upstream host
// when the user did not name one: api.mistral.ai -> mistral.
func providerFromUpstream(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	host := u.Hostname()
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return host
}

// applyEnv layers a small set of environment overrides over the file. These
// exist for quick one-off runs; anything structural belongs in the file.
func applyEnv(c *Config) {
	if v := os.Getenv("LLM_PROXY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("LLM_PROXY_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("LLM_PROXY_FULL_TRACE"); v != "" {
		c.Log.FullTrace = truthy(v)
	}
	if v := os.Getenv("NO_COLOR"); v != "" {
		c.Log.Color = "never"
	}
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Validate reports fatal configuration errors, and returns warnings for
// problems that are better observed at runtime than fatal at startup.
func (c *Config) Validate() ([]Warning, error) {
	var warns []Warning
	var errs []error

	if c.Listen == "" {
		errs = append(errs, errors.New("listen must not be empty"))
	}
	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("no routes configured: add at least one entry under `routes`"))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	switch c.Log.Format {
	case "pretty", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q: want pretty or json", c.Log.Format))
	}
	switch c.Log.Color {
	case "auto", "always", "never":
	default:
		errs = append(errs, fmt.Errorf("log.color %q: want auto, always or never", c.Log.Color))
	}
	switch c.Log.Symbols {
	case "auto", "unicode", "ascii":
	default:
		errs = append(errs, fmt.Errorf("log.symbols %q: want auto, unicode or ascii", c.Log.Symbols))
	}
	switch c.Otel.Enabled {
	case "auto", "true", "false":
	default:
		errs = append(errs, fmt.Errorf("otel.enabled %q: want auto, true or false", c.Otel.Enabled))
	}
	if c.Log.UnsafeRevealSecrets {
		warns = append(warns, Warning{Text: "log.unsafe_reveal_secrets is on: API keys will be written to the console and to OTLP in full"})
	}

	for _, p := range c.ContextLengthPatterns {
		if _, err := regexp.Compile("(?i)" + p); err != nil {
			errs = append(errs, fmt.Errorf("context_length_patterns: %q does not compile: %w", p, err))
		}
	}

	warns, errs = c.validateAuth(warns, errs)

	seen := make(map[string]bool, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]
		where := fmt.Sprintf("routes[%d]", i)
		if r.Name != "" {
			where = "route " + r.Name
		}

		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("%s: name must not be empty", where))
		case !routeNameRE.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s: name may only contain letters, digits, dot, dash and underscore", where))
		case r.Name == ReservedPrefix:
			errs = append(errs, fmt.Errorf("%s: name is reserved for llm-proxy's own endpoints", where))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate route name", where))
		}
		seen[r.Name] = true

		if err := validateUpstream(where, r.Upstream); err != nil {
			errs = append(errs, err)
		} else if strings.HasSuffix(strings.TrimSuffix(r.Upstream, "/"), "/v1") {
			warns = append(warns, Warning{Route: r.Name, Text: fmt.Sprintf(
				"upstream ends in /v1 — clients also send /v1, which would produce /v1/v1/chat/completions. "+
					"Drop it and point your client at http://%s/%s/v1", c.Listen, r.Name)})
		}

		if r.APIKeyEnv == "" {
			warns = append(warns, Warning{Route: r.Name, Text: "no api_key_env set — requests will only carry credentials if the client sends its own Authorization header"})
		} else if os.Getenv(r.APIKeyEnv) == "" {
			warns = append(warns, Warning{Route: r.Name, Text: fmt.Sprintf("%s is unset — expect 401s unless the client sends its own Authorization header", r.APIKeyEnv)})
		}

		switch r.ExpectDone {
		case "auto", "true", "false":
		default:
			errs = append(errs, fmt.Errorf("%s: expect_done %q: want auto, true or false", where, r.ExpectDone))
		}

		if err := validateStripParams(where, r.StripParams); err != nil {
			errs = append(errs, err)
		} else if len(r.StripParams) > 0 {
			warns = append(warns, Warning{Route: r.Name, Text: fmt.Sprintf(
				"strip_params is set (%s): request bodies on this route are rewritten before "+
					"being forwarded, so what the vendor sees is not byte-for-byte what the "+
					"client sent", strings.Join(r.StripParams, ", "))})
		}

		t := r.Timeouts
		idle, header, gap := Dur(t.StreamIdle), Dur(t.ResponseHeader), Dur(t.GapWarn)
		if idle > 0 && header > 0 && idle > header {
			warns = append(warns, Warning{Route: r.Name, Text: fmt.Sprintf(
				"timeouts.stream_idle (%s) exceeds timeouts.response_header (%s); a slow first token will fail before a mid-stream stall does",
				idle, header)})
		}
		if gap > 0 && idle > 0 && gap >= idle {
			warns = append(warns, Warning{Route: r.Name, Text: "timeouts.gap_warn is not below timeouts.stream_idle, so the warning can never fire before the fault"})
		}
		if Int(r.MaxIdleConnsPerHost) < 0 {
			errs = append(errs, fmt.Errorf("%s: max_idle_conns_per_host must not be negative", where))
		}
		if r.MaxRequestBody <= 0 {
			errs = append(errs, fmt.Errorf("%s: max_request_body must be positive", where))
		}
		if Bool(r.InsecureSkipVerify) {
			warns = append(warns, Warning{Route: r.Name, Text: "insecure_skip_verify is on: TLS certificates are not checked for this route"})
		}

		if r.Retry != nil {
			if err := validateRetry(where, r.Retry); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return warns, errors.Join(errs...)
	}
	return warns, nil
}

// ResolvedTokens returns the configured tokens with their secrets looked up,
// along with the names of any that did not resolve.
func (c *Config) ResolvedTokens() (resolved []struct{ Name, Secret string }, missing []string) {
	// An explicit false means no credentials at all, whatever the environment
	// says. Without this, anything that builds a config in-process — the demo,
	// the test harness — silently inherits the operator's LLM_PROXY_TOKENS and
	// starts rejecting its own requests.
	if strings.EqualFold(c.Auth.Enabled, "false") {
		return nil, nil
	}

	for i, t := range c.Auth.Tokens {
		name := t.Name
		if name == "" {
			name = fmt.Sprintf("token[%d]", i)
		}
		switch {
		case t.Value != "":
			resolved = append(resolved, struct{ Name, Secret string }{name, t.Value})
		case t.Env == "":
			missing = append(missing, name+" (no env or value set)")
		case os.Getenv(t.Env) != "":
			resolved = append(resolved, struct{ Name, Secret string }{name, os.Getenv(t.Env)})
		default:
			missing = append(missing, name+" ("+t.Env+" is unset)")
		}
	}

	env := c.Auth.TokensEnv
	if env == "" {
		env = "LLM_PROXY_TOKENS"
	}
	for i, secret := range strings.Split(os.Getenv(env), ",") {
		if secret = strings.TrimSpace(secret); secret != "" {
			resolved = append(resolved, struct{ Name, Secret string }{
				fmt.Sprintf("%s[%d]", env, i), secret})
		}
	}
	return resolved, missing
}

// validateAuth fails closed: configuring auth and then starting without it,
// because an environment variable was missing, is the one outcome worth
// refusing to boot over.
func (c *Config) validateAuth(warns []Warning, errs []error) ([]Warning, []error) {
	switch strings.ToLower(c.Auth.Enabled) {
	case "", "auto", "true", "false":
	default:
		errs = append(errs, fmt.Errorf("auth.enabled %q: want auto, true or false", c.Auth.Enabled))
	}

	if strings.EqualFold(c.Auth.Enabled, "false") && len(c.Auth.Tokens) > 0 {
		warns = append(warns, Warning{Text: "auth.enabled is false, so the configured tokens are ignored"})
	}

	resolved, missing := c.ResolvedTokens()

	if len(c.Auth.Tokens) > 0 && Bool(orTrue(c.Auth.Required)) && len(missing) > 0 {
		errs = append(errs, fmt.Errorf(
			"auth: %s — refusing to start unauthenticated. Set the variable, or "+
				"remove the token from the config, or set auth.required: false",
			strings.Join(missing, "; ")))
	}

	for _, t := range c.Auth.Tokens {
		if t.Value != "" {
			warns = append(warns, Warning{Text: fmt.Sprintf(
				"auth token %q is written inline in the config file; prefer `env:` so the secret "+
					"is not committed", t.Name)})
		}
	}
	for _, t := range resolved {
		if len(t.Secret) < 16 {
			warns = append(warns, Warning{Text: fmt.Sprintf(
				"auth token %q is only %d characters; use something like `openssl rand -hex 32`",
				t.Name, len(t.Secret))})
		}
	}

	// The safety net for the case this feature exists for.
	if len(resolved) == 0 && !isLoopback(c.Listen) {
		warns = append(warns, Warning{Text: fmt.Sprintf(
			"listening on %s with no auth tokens configured — anything that can reach this "+
				"address can spend your API credits. Set LLM_PROXY_TOKENS to require a token.",
			c.Listen)})
	}
	return warns, errs
}

func orTrue(p *bool) *bool {
	if p == nil {
		t := true
		return &t
	}
	return p
}

// isLoopback reports whether a listen address is reachable only from this host.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::", "[::]":
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validateUpstream(where, upstream string) error {
	if upstream == "" {
		return fmt.Errorf("%s: upstream must not be empty", where)
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("%s: upstream %q is not a valid URL: %w", where, upstream, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: upstream %q must start with http:// or https://", where, upstream)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: upstream %q has no host", where, upstream)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: upstream %q must not carry a query string or fragment", where, upstream)
	}
	return nil
}

// loadBearingParams may not be stripped. The first two make the request invalid
// on arrival, and the third changes the response protocol out from under a
// client that asked for SSE — three ways to turn an interop shim into a fault
// the proxy itself caused, and then reports as the vendor's.
var loadBearingParams = map[string]string{
	"model":    "the vendor has nothing to route the request to",
	"messages": "there is no prompt left to answer",
	"stream":   "the vendor would answer a client that asked for SSE with a single JSON body",
}

func validateStripParams(where string, keys []string) error {
	var errs []error
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		switch {
		case strings.TrimSpace(k) == "":
			errs = append(errs, fmt.Errorf("%s: strip_params[%d] is empty", where, i))
		case k != strings.TrimSpace(k):
			// A stray space is a key that silently never matches, which looks
			// exactly like the feature not working.
			errs = append(errs, fmt.Errorf(
				"%s: strip_params[%d] %q has leading or trailing whitespace; JSON keys are exact",
				where, i, k))
		case seen[k]:
			errs = append(errs, fmt.Errorf("%s: strip_params lists %q twice", where, k))
		}
		seen[k] = true

		if why, bad := loadBearingParams[k]; bad {
			errs = append(errs, fmt.Errorf(
				"%s: strip_params must not contain %q — %s. strip_params is for parameters a "+
					"vendor rejects, not for reshaping the request", where, k, why))
		}
	}
	return errors.Join(errs...)
}

func validateRetry(where string, r *Retry) error {
	var errs []error
	if r.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("%s: retry.max_attempts must be at least 1", where))
	}
	for _, s := range r.On {
		if s < 400 || s > 599 {
			errs = append(errs, fmt.Errorf("%s: retry.on contains %d; only 4xx and 5xx statuses are retryable", where, s))
		}
	}
	if r.Jitter < 0 || r.Jitter > 1 {
		errs = append(errs, fmt.Errorf("%s: retry.jitter must be between 0 and 1", where))
	}
	if r.MaxWait < 0 || r.BaseBackoff < 0 || r.MaxBackoff < 0 {
		errs = append(errs, fmt.Errorf("%s: retry durations must not be negative", where))
	}
	return errors.Join(errs...)
}

// strictDecode rejects unknown fields so a typo in the config is reported by
// name instead of being silently ignored.
func strictDecode(raw []byte, into *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty file is a legitimate config: it means "defaults only".
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
