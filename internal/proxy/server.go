package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/auth"
	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/oaierr"
	"github.com/peterhoneder/llm-proxy/internal/obs"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// Server is the whole proxy: one listener, many routes.
type Server struct {
	cfg *config.Config
	log *obs.Logger

	targets     []*target
	auth        *auth.Authenticator
	mux         *http.ServeMux
	http        *http.Server
	listener    net.Listener
	ctxMatchers *oaierr.Matchers

	shutdown     chan struct{}
	shutdownOnce sync.Once
	shuttingDown atomic.Bool

	requestSeq atomic.Uint64
	connSeq    atomic.Uint64

	// onRecord, when set, receives every completed request. Tests use it to
	// assert on the verdict the operator would have seen.
	onRecord func(record.Snapshot)

	// onGapWarn, when set, fires each time a still-waiting report is emitted.
	onGapWarn func()

	nowFn func() time.Time
}

// connIDKey carries a per-connection identifier so several requests sharing one
// keep-alive connection can be told apart in the console. It is deliberately
// not used as a disconnect signal: r.Context() already carries that, earlier.
type connIDKey struct{}

func connIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(connIDKey{}).(string)
	return id
}

// New builds a Server from a validated config.
func New(cfg *config.Config, log *obs.Logger) (*Server, error) {
	matchers, err := oaierr.NewMatchers(cfg.ContextLengthPatterns)
	if err != nil {
		return nil, fmt.Errorf("context_length_patterns: %w", err)
	}

	resolved, _ := cfg.ResolvedTokens()
	tokens := make([]auth.Token, 0, len(resolved))
	for _, t := range resolved {
		tokens = append(tokens, auth.NewToken(t.Name, t.Secret))
	}

	s := &Server{
		cfg:         cfg,
		log:         log,
		auth:        auth.New(tokens),
		mux:         http.NewServeMux(),
		ctxMatchers: matchers,
		shutdown:    make(chan struct{}),
		nowFn:       time.Now,
	}

	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		key := ""
		if route.APIKeyEnv != "" {
			key = os.Getenv(route.APIKeyEnv)
		}
		t, err := newTarget(route, key)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", route.Name, err)
		}
		s.targets = append(s.targets, t)

		h := &routeHandler{srv: s, target: t}
		// Mounting with a trailing slash lets Go 1.22+ longest-pattern
		// precedence resolve /mistral against /mistral-eu correctly. Every path
		// under the prefix is proxied: a harness calls /v1/models at startup,
		// and a 404 there breaks it.
		s.mux.Handle("/"+route.Name+"/", h)
		s.mux.Handle("/"+route.Name, h)
	}

	s.mux.HandleFunc("/"+config.ReservedPrefix+"/healthz", s.handleHealth)
	s.mux.HandleFunc("/"+config.ReservedPrefix+"/routes", s.handleRoutes)
	s.mux.HandleFunc("/", s.handleUnknown)

	s.http = &http.Server{
		Addr:    cfg.Listen,
		Handler: s.requireAuth(s.mux),

		// ReadHeaderTimeout bounds a client that opens a connection and never
		// finishes its request line.
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout stays off: a large prompt upload is legitimate.
		ReadTimeout: 0,
		// WriteTimeout MUST stay zero. An LLM stream runs for minutes, and a
		// copy-pasted 30s here would make the proxy manufacture exactly the
		// mid-stream cut it exists to diagnose.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			id := "c-" + strconv.FormatUint(s.connSeq.Add(1), 10)
			return context.WithValue(ctx, connIDKey{}, id)
		},
	}
	return s, nil
}

func (s *Server) now() time.Time { return s.nowFn() }

// nextRequestID numbers requests sequentially. A counter beats a ULID prefix
// here: it is readable, it sorts, and it does not repeat for hours the way a
// truncated timestamp does.
func (s *Server) nextRequestID() string {
	return fmt.Sprintf("r-%05d", s.requestSeq.Add(1))
}

// Addr reports the address the server is listening on, which is what tests need
// when they ask for port 0.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.Listen
}

// Serve listens and serves until Shutdown is called.
func (s *Server) Serve() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.listener = ln
	s.log.Startup(s.StartupBanner())

	if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ServeListener serves on an existing listener, which tests use.
func (s *Server) ServeListener(ln net.Listener) error {
	s.listener = ln
	if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests, then
// gives up.
//
// An LLM stream can legitimately run for minutes, so waiting indefinitely would
// make ^C feel broken. Anything still running when the grace period expires is
// cut — and, because the client watcher stamps ErrProxyShutdown as the
// cancellation cause, those requests are reported as the proxy's doing rather
// than blamed on the vendor.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.shutdownOnce.Do(func() { close(s.shutdown) })

	err := s.http.Shutdown(ctx)
	for _, t := range s.targets {
		t.CloseIdleConnections()
	}
	if err == context.DeadlineExceeded {
		return s.http.Close()
	}
	return err
}

// requireAuth rejects unauthenticated requests before they reach any route.
//
// Only the health endpoint is exempt, so an uptime probe needs no credential.
// Everything else is covered — including the 404 handler, which lists the
// configured routes and would otherwise disclose the upstreams to anyone who
// guessed a wrong path.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() || r.URL.Path == "/"+config.ReservedPrefix+"/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		res := s.auth.Check(r)
		if !res.OK {
			s.denyUnauthenticated(w, r, res)
			return
		}

		// The credential authenticated this hop and must not travel further:
		// a route with no key of its own would otherwise forward the proxy's
		// own token to the LLM vendor.
		auth.StripCredential(r.Header, res.Via)
		next.ServeHTTP(w, authContext(r, res.Name))
	})
}

type authNameKey struct{}

func authContext(r *http.Request, name string) *http.Request {
	if name == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), authNameKey{}, name))
}

func authNameFrom(ctx context.Context) string {
	name, _ := ctx.Value(authNameKey{}).(string)
	return name
}

func (s *Server) denyUnauthenticated(w http.ResponseWriter, r *http.Request, res auth.Result) {
	msg := "llm-proxy: missing credentials. Send your proxy token as `Authorization: Bearer <token>` " +
		"or in the " + auth.HeaderName + " header."
	if res.Presented {
		msg = "llm-proxy: the token presented is not one of the configured tokens."
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="llm-proxy"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    "invalid_api_key",
		},
	})

	// Worth seeing when the listener is exposed, and the only thing recorded
	// about the attempt is where it came from — never what was presented.
	s.log.Slog().WarnContext(r.Context(), "rejected an unauthenticated request",
		"path", r.URL.Path, "from", r.RemoteAddr, "presented_credential", res.Presented)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("shutting down\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	type routeInfo struct {
		Name     string `json:"name"`
		Prefix   string `json:"prefix"`
		Upstream string `json:"upstream"`
		BaseURL  string `json:"client_base_url"`
		Auth     string `json:"auth"`
		Retry    bool   `json:"retry"`
	}
	out := make([]routeInfo, 0, len(s.targets))
	for _, t := range s.targets {
		out = append(out, routeInfo{
			Name:     t.route.Name,
			Prefix:   "/" + t.route.Name + "/",
			Upstream: t.route.Upstream,
			BaseURL:  "http://" + s.cfg.Listen + "/" + t.route.Name + "/v1",
			Auth:     authDescription(t),
			Retry:    t.route.Retry != nil,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"routes": out})
}

// handleUnknown answers an unmatched path in the shape an OpenAI client can
// parse, listing the routes that do exist. An HTML 404 here would show up in a
// harness's logs as an unreadable parse error.
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		names = append(names, "/"+t.route.Name+"/v1")
	}

	msg := fmt.Sprintf("llm-proxy: no route matches %q. Configured routes: %s",
		r.URL.Path, strings.Join(names, ", "))
	if len(names) == 0 {
		msg = "llm-proxy: no routes are configured"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    "route_not_found",
		},
	})

	s.log.Slog().WarnContext(r.Context(), "no route matches request",
		"path", r.URL.Path, "routes", names)
}

// StartupBanner renders the route table printed at boot. Seeing which key each
// route resolved and what base URL to point a client at removes most of the
// first-run confusion.
func (s *Server) StartupBanner() string {
	var b strings.Builder
	fmt.Fprintf(&b, "listening on %s\n", s.cfg.Listen)
	if s.auth.Enabled() {
		fmt.Fprintf(&b, "  auth   required — clients must send a proxy token (%s or Authorization: Bearer)\n",
			auth.HeaderName)
	} else {
		fmt.Fprint(&b, "  auth   OPEN — no proxy token required\n")
	}

	// The rendered cell is "/name/*", so the column has to allow for the three
	// extra characters or the arrows do not line up.
	width := 0
	for _, t := range s.targets {
		if n := len(t.route.Name) + 3; n > width {
			width = n
		}
	}

	for _, t := range s.targets {
		r := t.route
		proto := "http/1.1"
		if config.Bool(r.HTTP2) {
			proto = "h2"
		}
		retry := "off"
		if r.Retry != nil {
			retry = fmt.Sprintf("%dx on %s", r.Retry.MaxAttempts, joinInts(r.Retry.On))
		}
		fmt.Fprintf(&b, "  route %-*s → %-38s auth=%-28s %s  retry=%s\n",
			width, "/"+r.Name+"/*", r.Upstream, authDescription(t), proto, retry)
		fmt.Fprintf(&b, "        %-*s   client base_url: http://%s/%s/v1\n",
			width, "", s.cfg.Listen, r.Name)
	}
	return b.String()
}

func authDescription(t *target) string {
	switch {
	case t.route.APIKeyEnv == "":
		return "client-supplied only"
	case t.apiKey != "":
		return t.route.APIKeyEnv + " (set)"
	default:
		return t.route.APIKeyEnv + " (MISSING)"
	}
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
