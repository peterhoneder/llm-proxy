// Command llm-proxy is an OpenAI-compatible reverse proxy that exists to
// answer one question about a failed LLM request: which side broke it?
//
// It forwards bytes verbatim, watches both ends of every stream, and prints a
// postmortem that names the client, the vendor or the proxy itself as the
// cause. See README.md for the reasoning behind the design.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/obs"
	"github.com/peterhoneder/llm-proxy/internal/proxy"
)

// Set via -ldflags at build time; see the Makefile.
var (
	version = "dev"
	commit  = "none"
)

type flags struct {
	config      string
	listen      string
	logLevel    string
	fullTrace   bool
	jsonLogs    bool
	noColor     bool
	check       bool
	showVersion bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "llm-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var f flags
	fs := flag.NewFlagSet("llm-proxy", flag.ContinueOnError)
	fs.StringVar(&f.config, "config", "", "path to the config file (default: ./llm-proxy.yaml, then $LLM_PROXY_CONFIG)")
	fs.StringVar(&f.listen, "listen", "", "override listen address")
	fs.StringVar(&f.logLevel, "log-level", "", "override log level: debug, info, warn, error")
	fs.BoolVar(&f.fullTrace, "full-trace", false, "dump all headers, bodies and every SSE frame (reveals your prompts)")
	fs.BoolVar(&f.jsonLogs, "json", false, "emit JSON logs instead of the pretty console format")
	fs.BoolVar(&f.noColor, "no-color", false, "disable colour")
	fs.BoolVar(&f.check, "check", false, "validate the config, print the route table and exit")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "llm-proxy — an OpenAI-compatible wire-level debugging proxy\n\nusage: llm-proxy [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if f.showVersion {
		fmt.Printf("llm-proxy %s (%s, %s %s/%s)\n", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}

	path := config.FindConfig(f.config)
	if path == "" {
		return fmt.Errorf("no config file found: pass -config, set $LLM_PROXY_CONFIG, or create ./llm-proxy.yaml\n" +
			"       see llm-proxy.example.yaml for a starting point")
	}

	cfg, _, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("config %s:\n%w", path, err)
	}
	applyFlags(cfg, &f)
	// Re-validate after the flags, and keep *these* warnings: -listen can move
	// the proxy onto a public address, and the warning about an unguarded
	// listener has to reflect where it will actually listen.
	warnings, err := cfg.Validate()
	if err != nil {
		return fmt.Errorf("config %s:\n%w", path, err)
	}

	if f.check {
		printRouteTable(cfg, path, warnings)
		return nil
	}

	return serve(cfg, warnings)
}

func serve(cfg *config.Config, warnings []config.Warning) error {
	shutdownOtel, otelHandler, err := obs.SetupOtel(context.Background(), cfg.Otel)
	if err != nil {
		return fmt.Errorf("opentelemetry: %w", err)
	}
	defer shutdownOtel()

	log := obs.NewLogger(obs.Options{Cfg: cfg.Log, OtelHandler: otelHandler})

	log.Startup(fmt.Sprintf("llm-proxy %s (%s, %s %s/%s)\n",
		version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH))
	for _, w := range warnings {
		if w.Route != "" {
			log.Slog().Warn(w.Text, "route", w.Route)
		} else {
			log.Slog().Warn(w.Text)
		}
	}
	if otelHandler == nil {
		log.Slog().Info("opentelemetry export is off — set OTEL_EXPORTER_OTLP_ENDPOINT to enable it")
	}
	if cfg.Log.FullTrace {
		log.Slog().Warn("full-trace is on: request and response bodies, including your prompts, " +
			"will be written to this console (API keys stay redacted)")
	}

	srv, err := proxy.New(cfg, log)
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errc:
		return err
	case sig := <-stop:
		log.Slog().Info("shutting down", "signal", sig.String(),
			"grace", cfg.ShutdownGrace.String())

		// In-flight LLM streams can run for minutes, so the wait is bounded.
		// Anything still running when the grace period expires is cut and
		// reported as the proxy's doing — never blamed on the vendor.
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace.D())
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Slog().Warn("shutdown did not complete cleanly", "error", err)
		}
		return nil
	}
}

// applyFlags layers command-line overrides on top of file and environment.
// Flags win over everything, which is why this runs after config.Load.
func applyFlags(c *config.Config, f *flags) {
	if f.listen != "" {
		c.Listen = f.listen
	}
	if f.logLevel != "" {
		c.Log.Level = f.logLevel
	}
	if f.fullTrace {
		c.Log.FullTrace = true
	}
	if f.jsonLogs {
		c.Log.Format = "json"
	}
	if f.noColor {
		c.Log.Color = "never"
	}
}

func printRouteTable(c *config.Config, path string, warnings []config.Warning) {
	fmt.Printf("llm-proxy %s — config %s\n", version, path)
	fmt.Printf("listen %s\n", c.Listen)

	if resolved, _ := c.ResolvedTokens(); len(resolved) > 0 {
		names := make([]string, len(resolved))
		for i, t := range resolved {
			names[i] = t.Name
		}
		fmt.Printf("auth   required — %d token(s): %s\n\n", len(resolved), strings.Join(names, ", "))
	} else {
		fmt.Printf("auth   OPEN — no proxy token required\n\n")
	}

	for _, r := range c.Routes {
		var auth string
		switch {
		case r.APIKeyEnv == "":
			auth = "client-supplied only"
		case os.Getenv(r.APIKeyEnv) != "":
			auth = r.APIKeyEnv + " (set)"
		default:
			auth = r.APIKeyEnv + " (MISSING)"
		}

		proto := "http/1.1"
		if config.Bool(r.HTTP2) {
			proto = "h2"
		}

		retry := "off"
		if r.Retry != nil {
			retry = fmt.Sprintf("%dx on %s (max wait %s)", r.Retry.MaxAttempts, joinInts(r.Retry.On), r.Retry.MaxWait)
		}

		fmt.Printf("  /%s/*  ->  %s\n", r.Name, r.Upstream)
		fmt.Printf("      auth=%s  %s  retry=%s\n", auth, proto, retry)
		fmt.Printf("      waits: first byte %s, between chunks %s, progress every %s\n",
			limit(r.Timeouts.ResponseHeader), limit(r.Timeouts.StreamIdle), limit(r.Timeouts.GapWarn))
		// Only printed when it is on: this is the one setting under which the
		// route no longer forwards what the client actually sent.
		if len(r.StripParams) > 0 {
			fmt.Printf("      rewriting requests: stripping %s\n", strings.Join(r.StripParams, ", "))
		}
		fmt.Printf("      client base_url: http://%s/%s/v1\n\n", c.Listen, r.Name)
	}

	for _, w := range warnings {
		if w.Route != "" {
			fmt.Printf("warning [%s]: %s\n", w.Route, w.Text)
		} else {
			fmt.Printf("warning: %s\n", w.Text)
		}
	}
}

// limit renders a duration setting, spelling out that zero means no deadline
// rather than printing "0s" and leaving the reader to guess.
func limit(d *config.Duration) string {
	if config.Dur(d) == 0 {
		return "unlimited"
	}
	return config.Dur(d).String()
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ",")
}
