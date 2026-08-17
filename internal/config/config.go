// Package config defines llm-proxy's configuration and loads it from an
// embedded default file, a user file, the environment and flags, in that order.
//
// Every default lives in default_config.yaml. There are deliberately no default
// values written in Go code: one file answers "what happens if I set nothing?".
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a time.Duration that unmarshals from "30s", "1m30s", "500ms".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Bytes is a byte count that unmarshals from "8MiB", "64KiB", "1024" or 1024.
type Bytes int64

func (b Bytes) Int64() int64 { return int64(b) }

func (b Bytes) String() string {
	switch {
	case b >= 1<<20 && b%(1<<20) == 0:
		return strconv.FormatInt(int64(b)/(1<<20), 10) + "MiB"
	case b >= 1<<10 && b%(1<<10) == 0:
		return strconv.FormatInt(int64(b)/(1<<10), 10) + "KiB"
	default:
		return strconv.FormatInt(int64(b), 10)
	}
}

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
	{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
	{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
	{"B", 1},
}

func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	var raw string
	if err := n.Decode(&raw); err != nil {
		return fmt.Errorf("byte size must be a number or a string like \"8MiB\": %w", err)
	}
	s := strings.ToUpper(strings.TrimSpace(raw))
	for _, u := range byteUnits {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return fmt.Errorf("invalid byte size %q: %w", raw, err)
		}
		if v < 0 {
			return fmt.Errorf("invalid byte size %q: must not be negative", raw)
		}
		*b = Bytes(int64(v * float64(u.mult)))
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: want a number or a suffix like KiB/MiB", raw)
	}
	if v < 0 {
		return fmt.Errorf("invalid byte size %q: must not be negative", raw)
	}
	*b = Bytes(v)
	return nil
}

// Config is the whole configuration.
type Config struct {
	Listen        string    `yaml:"listen"`
	ShutdownGrace Duration  `yaml:"shutdown_grace"`
	Log           Log       `yaml:"log"`
	Otel          Otel      `yaml:"otel"`
	Auth          Auth      `yaml:"auth"`
	Defaults      RouteOpts `yaml:"defaults"`
	Routes        []Route   `yaml:"routes"`

	// ContextLengthPatterns are matched (case-insensitively, as regexps)
	// against vendor error bodies to recognise a context-window overflow.
	// Vendors word this differently, so it is configuration, not code.
	ContextLengthPatterns []string `yaml:"context_length_patterns"`
}

// Auth guards the proxy's own listener.
//
// It exists so the proxy can be exposed through a tunnel — Tailscale Funnel,
// ngrok, a reverse proxy — without standing open. There is deliberately no
// exemption for local traffic: those tunnels forward to a local port, so
// exempting loopback would exempt exactly the requests that need checking.
type Auth struct {
	// Enabled is auto | true | false.
	//
	// "auto" turns auth on when any token resolves, including from the
	// environment, which is the zero-config path. "false" turns it off
	// regardless — for a test or a demo that runs its own proxy and must not
	// inherit the operator's shell, and for temporarily disabling auth without
	// unsetting a variable.
	Enabled string `yaml:"enabled"`

	// Tokens are the accepted credentials. Each names an environment variable
	// holding the secret, so the config file itself stays safe to commit.
	Tokens []AuthToken `yaml:"tokens"`

	// TokensEnv names one environment variable holding a comma-separated list
	// of tokens, for turning auth on without editing the config at all.
	// Defaults to LLM_PROXY_TOKENS.
	TokensEnv string `yaml:"tokens_env"`

	// Required fails startup when auth is configured but no token resolves.
	// On by default: a proxy that silently starts unauthenticated because an
	// environment variable was missing is the worst possible outcome here.
	Required *bool `yaml:"required"`
}

// AuthToken is one accepted credential.
type AuthToken struct {
	// Name identifies the caller in the log. It is not a secret.
	Name string `yaml:"name"`
	// Env is the environment variable holding the token. Preferred.
	Env string `yaml:"env"`
	// Value is an inline token. Supported, but it puts a secret in a file that
	// tends to end up in version control, so Validate warns about it.
	Value string `yaml:"value"`
}

// Log controls console rendering. OTel export is configured separately.
type Log struct {
	Level   string `yaml:"level"`   // debug | info | warn | error
	Format  string `yaml:"format"`  // pretty | json
	Color   string `yaml:"color"`   // auto | always | never
	Symbols string `yaml:"symbols"` // auto | unicode | ascii

	// FullTrace dumps all headers, bodies and every SSE frame. Prompts and
	// responses reach the console (and OTLP) when this is on.
	FullTrace    bool  `yaml:"full_trace"`
	MaxBodyBytes Bytes `yaml:"max_body_bytes"`

	// RedactHeaders are rendered as a fingerprint rather than a value.
	RedactHeaders []string `yaml:"redact_headers"`

	// UnsafeRevealSecrets disables redaction entirely. Logs a loud warning.
	UnsafeRevealSecrets bool `yaml:"unsafe_reveal_secrets"`
}

// Otel configures OpenTelemetry export. Standard OTEL_* environment variables
// are honoured by the exporters themselves; Enabled: "auto" means "on if an
// OTLP endpoint is configured", so the default build needs no collector.
type Otel struct {
	Enabled     string `yaml:"enabled"` // auto | true | false
	Traces      *bool  `yaml:"traces"`
	Logs        *bool  `yaml:"logs"`
	ServiceName string `yaml:"service_name"`
	LogLevel    string `yaml:"log_level"`
}

// Route maps a URL path prefix to one upstream vendor.
type Route struct {
	Name      string `yaml:"name"`
	Upstream  string `yaml:"upstream"`
	APIKeyEnv string `yaml:"api_key_env"`

	RouteOpts `yaml:",inline"`
}

// RouteOpts are per-route settings. Anything left unset inherits from
// Config.Defaults, so pointer and zero values must be distinguishable.
type RouteOpts struct {
	Provider     string            `yaml:"provider"`       // gen_ai.provider.name; defaults to the upstream host
	APIKeyHeader string            `yaml:"api_key_header"` // "Authorization"
	APIKeyPrefix string            `yaml:"api_key_prefix"` // "Bearer "
	Headers      map[string]string `yaml:"headers"`

	// ForwardClientAuth passes the client's own Authorization header through
	// untouched instead of substituting the configured key.
	ForwardClientAuth *bool `yaml:"forward_client_auth"`

	HTTP2              *bool `yaml:"http2"`
	InsecureSkipVerify *bool `yaml:"insecure_skip_verify"`
	DisableKeepalives  *bool `yaml:"disable_keepalives"`

	MaxIdleConnsPerHost *int     `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout     Duration `yaml:"idle_conn_timeout"`
	MaxRequestBody      Bytes    `yaml:"max_request_body"`

	// ExpectDone: auto | true | false. "auto" treats a terminal finish_reason
	// as sufficient proof of completion when a backend omits `data: [DONE]`,
	// which many OpenAI-compatible servers do.
	ExpectDone string `yaml:"expect_done"`

	// AbortOnTruncation aborts the downstream connection when a stream is
	// found truncated, so the client cannot mistake it for a complete answer.
	AbortOnTruncation *bool `yaml:"abort_on_truncation"`

	Timeouts Timeouts `yaml:"timeouts"`
	Retry    *Retry   `yaml:"retry"` // nil means pure passthrough
}

// Timeouts govern one upstream attempt.
//
// The fields are pointers because zero is a meaningful value here — it means
// "no limit" — and has to be distinguishable from "not set, inherit the
// default". Writing `stream_idle: 0s` on a route must disable the deadline, not
// silently pick the default back up.
type Timeouts struct {
	Connect      *Duration `yaml:"connect"`
	TLSHandshake *Duration `yaml:"tls_handshake"`

	// ResponseHeader bounds the wait for the status line, and StreamIdle the
	// gap between chunks. Both default to no limit: a reasoning model can take
	// ten minutes to its first token, and a proxy that gives up first destroys
	// the observation it exists to make.
	ResponseHeader *Duration `yaml:"response_header"`
	StreamIdle     *Duration `yaml:"stream_idle"`
	Total          *Duration `yaml:"total"`

	// ClientWrite is a per-write deadline downstream, which is what catches a
	// client that is connected but has stopped reading.
	ClientWrite *Duration `yaml:"client_write"`

	// GapWarn is not a deadline: it is how often to report that nothing has
	// arrived yet, so a long wait can be watched instead of guessed at.
	GapWarn *Duration `yaml:"gap_warn"`
}

// Retry is opt-in per route. Retries are only possible before any byte has
// been written downstream; see proxy.retry.
type Retry struct {
	MaxAttempts    int      `yaml:"max_attempts"`
	On             []int    `yaml:"on"`
	OnConnectError *bool    `yaml:"on_connect_error"`
	MaxWait        Duration `yaml:"max_wait"`
	BaseBackoff    Duration `yaml:"base_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
	Jitter         float64  `yaml:"jitter"`

	// RespectResetHeaders makes the wait honour x-ratelimit-*-reset in
	// addition to Retry-After.
	RespectResetHeaders *bool `yaml:"respect_reset_headers"`
}

// Dur dereferences an optional duration. nil means unset; a non-nil zero means
// "no limit", and the two must not be confused.
func Dur(p *Duration) time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(*p)
}

// Bool dereferences an optional bool, treating nil as false.
func Bool(p *bool) bool { return p != nil && *p }

// Int dereferences an optional int, treating nil as zero.
func Int(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
