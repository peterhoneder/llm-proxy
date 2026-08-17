// Command demo drives llm-proxy through every failure mode it can diagnose,
// against a fake vendor running in the same process.
//
// It needs no credentials and no network. The point is to see what the console
// actually looks like when something goes wrong — which is the feature, so it
// deserves a way to be reviewed by eye rather than only asserted in tests.
//
//	go run ./cmd/demo              # every scenario
//	go run ./cmd/demo -scenario truncate
//	go run ./cmd/demo -full-trace
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/obs"
	"github.com/peterhoneder/llm-proxy/internal/proxy"
)

type scenario struct {
	name string
	desc string
	// handler plays the vendor's part.
	handler http.HandlerFunc
	// client, when set, replaces the default request with something that
	// misbehaves on the client side.
	client func(addr string)
	body   string
}

func main() {
	which := flag.String("scenario", "all", "scenario to run, or 'all'")
	fullTrace := flag.Bool("full-trace", false, "show headers, bodies and every SSE frame")
	flag.Parse()

	scenarios := buildScenarios()

	if *which == "list" {
		for _, s := range scenarios {
			fmt.Printf("  %-18s %s\n", s.name, s.desc)
		}
		return
	}

	for _, s := range scenarios {
		if *which != "all" && *which != s.name {
			continue
		}
		if err := run(s, *fullTrace); err != nil {
			fmt.Fprintf(os.Stderr, "demo: %s: %v\n", s.name, err)
			os.Exit(1)
		}
	}
}

func run(s scenario, fullTrace bool) error {
	fmt.Printf("\n\x1b[1m── %s ─────────────────────────────────────────────\x1b[0m\n%s\n\n",
		s.name, s.desc)

	vendor := &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
	vln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = vendor.Serve(vln) }()
	defer func() { _ = vendor.Close() }()

	cfg, err := config.DefaultConfig()
	if err != nil {
		return err
	}
	cfg.Log.FullTrace = fullTrace
	cfg.Log.Color = "always"
	cfg.Log.Level = "info"
	if fullTrace {
		cfg.Log.Level = "debug"
	}
	upstreamAddr := vln.Addr().String()
	if s.name == "upstream-down" {
		// Close the vendor first so the connection is genuinely refused.
		_ = vendor.Close()
		_ = vln.Close()
	}
	cfg.Routes = []config.Route{{
		Name:     "demo",
		Upstream: "http://" + upstreamAddr,
	}}
	cfg.Defaults.Timeouts.GapWarn = durPtr(700 * time.Millisecond)
	if s.name == "slow-first-token" {
		// The shipped default: no deadline at all after the connection.
		cfg.Defaults.Timeouts.StreamIdle = durPtr(0)
	} else {
		// Shortened so the other scenarios resolve quickly.
		cfg.Defaults.Timeouts.StreamIdle = durPtr(2 * time.Second)
	}
	cfg.ApplyRouteDefaultsForTest()

	// The one scenario that needs a non-default route configuration.
	if s.name == "rate-limited" {
		cfg.Routes[0].Retry = &config.Retry{
			MaxAttempts: 3, On: []int{429}, MaxWait: config.Duration(10 * time.Second),
			BaseBackoff: config.Duration(200 * time.Millisecond),
		}
		cfg.ApplyRouteDefaultsForTest()
	}

	log := obs.NewLogger(obs.Options{Cfg: cfg.Log, Out: os.Stdout})
	srv, err := proxy.New(cfg, log)
	if err != nil {
		return err
	}

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = srv.ServeListener(pln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	addr := pln.Addr().String()
	if s.client != nil {
		s.client(addr)
	} else {
		body := s.body
		if body == "" {
			body = `{"model":"demo-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
		}
		resp, err := http.Post("http://"+addr+"/demo/v1/chat/completions", "application/json",
			strings.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}

	// Let the proxy finish its postmortem before the next scenario's output.
	time.Sleep(400 * time.Millisecond)
	return nil
}

func buildScenarios() []scenario {
	return []scenario{
		{
			name:    "clean",
			desc:    "A healthy streaming request. One line, because nothing needs explaining.",
			handler: streamHandler(4, endDone, "stop"),
			body: `{"model":"demo-model","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "slow-first-token",
			desc: "A model that thinks for a long time before its first token. The proxy waits,\n" +
				"reports progress while waiting, and does not give up — otherwise it would be\n" +
				"the one that failed, and you would never see which side really did.",
			handler: slowFirstToken(3 * time.Second),
		},
		{
			name:    "truncated",
			desc:    "The vendor cuts the connection mid-frame — the answer is incomplete.",
			handler: streamHandler(3, endTruncate, ""),
			body: `{"model":"demo-model","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "no-done-sentinel",
			desc: "A backend that never sends [DONE] but does send finish_reason. This is healthy,\n" +
				"and a naive truncation check would flag it on every single request.",
			handler: streamHandler(3, endClean, "stop"),
		},
		{
			name: "stalled",
			desc: "The vendor goes silent mid-answer. The proxy enforces the deadline, but the\n" +
				"fault is the vendor's.",
			handler: streamHandler(1, endHang, ""),
		},
		{
			name:    "client-disconnect",
			desc:    "The client hangs up mid-stream — someone pressed ^C.",
			handler: streamHandler(20, endDone, "stop"),
			client:  hangUpMidStream,
		},
		{
			name: "context-length",
			desc: "The prompt is too big. The vendor's own wording is quoted verbatim.",
			handler: jsonHandler(http.StatusBadRequest,
				`{"error":{"message":"This model's maximum context length is 128000 tokens. `+
					`However, your messages resulted in 131204 tokens.","type":"invalid_request_error",`+
					`"param":"messages","code":"context_length_exceeded"}}`),
			body: `{"model":"demo-model","max_tokens":8192,"messages":[{"role":"user","content":"..."}]}`,
		},
		{
			name:    "output-truncated",
			desc:    "finish_reason=length: the protocol behaved correctly, but the answer is cut off.",
			handler: streamHandler(3, endDone, "length"),
		},
		{
			name:    "rate-limited",
			desc:    "A 429 with Retry-After, honoured and then retried successfully.",
			handler: rateLimitedThenOK(),
		},
		{
			name:    "error-mid-stream",
			desc:    "HTTP 200, then an error frame. The status line already said everything was fine.",
			handler: errorMidStream(),
		},
		{
			name: "upstream-down",
			desc: "Nothing is listening: the request never reached the vendor.",
			// The handler is never reached — the scenario closes the vendor
			// listener before the request is made.
			handler: jsonHandler(http.StatusOK, "{}"),
			client:  againstADeadPort,
		},
	}
}

func durPtr(d time.Duration) *config.Duration {
	v := config.Duration(d)
	return &v
}

type ending int

const (
	endDone ending = iota
	endClean
	endTruncate
	endHang
)

func streamHandler(n int, end ending, finish string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		writeHead(rw)
		for i := 0; i < n; i++ {
			frame := fmt.Sprintf(
				`data: {"id":"chatcmpl-demo","model":"demo-model","choices":[{"index":0,"delta":{"content":"tok%d"}}]}`+"\n\n", i)
			if end == endTruncate && i == n-1 {
				fmt.Fprintf(rw, "%x\r\n%s", len(frame), frame[:len(frame)/2])
				_ = rw.Flush()
				return
			}
			writeChunk(rw, frame)
			time.Sleep(60 * time.Millisecond)
		}

		if finish != "" {
			writeChunk(rw, fmt.Sprintf(
				`data: {"choices":[{"index":0,"finish_reason":%q,"delta":{}}]}`+"\n\n", finish))
			writeChunk(rw, `data: {"choices":[],"usage":{"prompt_tokens":1042,"completion_tokens":387,"total_tokens":1429}}`+"\n\n")
		}
		switch end {
		case endDone:
			writeChunk(rw, "data: [DONE]\n\n")
		case endHang:
			time.Sleep(4 * time.Second)
			return
		}
		_, _ = rw.WriteString("0\r\n\r\n")
		_ = rw.Flush()
	}
}

// slowFirstToken sends the response head, then nothing at all for a while.
func slowFirstToken(think time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		writeHead(rw)
		time.Sleep(think)

		writeChunk(rw, `data: {"choices":[{"index":0,"delta":{"content":"...worth the wait"}}]}`+"\n\n")
		writeChunk(rw, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		writeChunk(rw, `data: {"choices":[],"usage":{"prompt_tokens":1042,"completion_tokens":387,"total_tokens":1429}}`+"\n\n")
		writeChunk(rw, "data: [DONE]\n\n")
		_, _ = rw.WriteString("0\r\n\r\n")
		_ = rw.Flush()
	}
}

func errorMidStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		writeHead(rw)
		writeChunk(rw, `data: {"choices":[{"index":0,"delta":{"content":"partial answer"}}]}`+"\n\n")
		writeChunk(rw, `data: {"error":{"message":"the model backend crashed","type":"server_error"}}`+"\n\n")
		_, _ = rw.WriteString("0\r\n\r\n")
		_ = rw.Flush()
	}
}

func rateLimitedThenOK() http.HandlerFunc {
	var n int
	return func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Ratelimit-Limit-Requests", "5")
			w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
			w.Header().Set("X-Ratelimit-Reset-Requests", "1s")
			w.Header().Set("X-Ratelimit-Limit-Tokens", "500000")
			w.Header().Set("X-Ratelimit-Remaining-Tokens", "480000")
			w.Header().Set("X-Ratelimit-Reset-Tokens", "12.4")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"Requests rate limit exceeded","request_id":"3a1f8b2c"}`)
			return
		}
		streamHandler(3, endDone, "stop")(w, r)
	}
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func writeHead(rw *bufio.ReadWriter) {
	_, _ = rw.WriteString("HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/event-stream\r\n" +
		"Cache-Control: no-cache\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n")
	_ = rw.Flush()
}

func writeChunk(rw *bufio.ReadWriter, data string) {
	fmt.Fprintf(rw, "%x\r\n%s\r\n", len(data), data)
	_ = rw.Flush()
}

// hangUpMidStream reads a couple of events and then closes, which is what a
// harness does when its user interrupts it.
func hangUpMidStream(addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	body := `{"model":"demo-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	fmt.Fprintf(conn, "POST /demo/v1/chat/completions HTTP/1.1\r\nHost: demo\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 3; i++ {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
	_ = conn.Close()
}

// againstADeadPort points the client at a port with nothing behind it, so the
// failure happens before any response header exists.
func againstADeadPort(addr string) {
	resp, err := http.Post("http://"+addr+"/demo/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"demo-model","messages":[]}`))
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}
