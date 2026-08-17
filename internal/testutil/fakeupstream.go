// Package testutil provides a scriptable fake vendor and a raw HTTP client, so
// every failure mode this proxy is meant to diagnose can be reproduced without
// touching a real LLM API.
package testutil

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Event is one Server-Sent Event the fake upstream will emit.
type Event struct {
	Delay time.Duration
	Raw   string // written verbatim, without the trailing blank line
}

// Ending decides how a streaming response terminates. The distinction is the
// point of several tests: a handler that simply returns emits a well-formed
// chunked terminator and produces io.EOF, which is a completely different
// signal from a connection cut mid-chunk.
type Ending int

const (
	// EndDone finishes with `data: [DONE]` and a proper terminator.
	EndDone Ending = iota
	// EndCleanNoDone terminates the body correctly but never sends the
	// sentinel — what several OpenAI-compatible servers do on every healthy
	// request.
	EndCleanNoDone
	// EndTruncateMidFrame cuts the connection in the middle of a chunk, which
	// surfaces as io.ErrUnexpectedEOF.
	EndTruncateMidFrame
	// EndReset sends a TCP RST, which surfaces as ECONNRESET.
	EndReset
	// EndHang stops sending and holds the connection open, which only the
	// stall watchdog can resolve.
	EndHang
)

// Script describes one response from the fake vendor.
type Script struct {
	Status int
	Header http.Header
	Body   string // non-streaming body

	// Streaming response.
	SSE          []Event
	Ending       Ending
	FinishReason string // emitted as a terminal frame before the ending
	Usage        bool   // emit a usage frame
	Gzip         bool   // gzip-encode the stream, as a vendor would for a
	// client that sent Accept-Encoding: gzip

	// HeaderDelay holds back the status line, for TTFB and header-timeout tests.
	HeaderDelay time.Duration
	// HangFor is how long EndHang holds the connection open.
	HangFor time.Duration
}

// Upstream is a scriptable fake vendor.
type Upstream struct {
	*httptest.Server

	mu      sync.Mutex
	scripts []Script
	seen    []*SeenRequest
}

// SeenRequest is what the fake vendor received, kept so tests can prove a retry
// replayed the body byte for byte.
type SeenRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// NewUpstream starts a fake vendor that plays the given scripts in order. The
// last script repeats if more requests arrive than there are scripts.
func NewUpstream(t *testing.T, scripts ...Script) *Upstream {
	t.Helper()
	u := &Upstream{scripts: scripts}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.Server.Close)
	return u
}

// Seen returns the requests received so far.
func (u *Upstream) Seen() []*SeenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*SeenRequest, len(u.seen))
	copy(out, u.seen)
	return out
}

func (u *Upstream) nextScript(n int) Script {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.scripts) == 0 {
		return Script{Status: 200, Body: "{}"}
	}
	if n >= len(u.scripts) {
		return u.scripts[len(u.scripts)-1]
	}
	return u.scripts[n]
}

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	body := readAll(r)

	u.mu.Lock()
	n := len(u.seen)
	u.seen = append(u.seen, &SeenRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Header: r.Header.Clone(), Body: body,
	})
	u.mu.Unlock()

	s := u.nextScript(n)
	if s.HeaderDelay > 0 {
		time.Sleep(s.HeaderDelay)
	}

	if len(s.SSE) > 0 || s.FinishReason != "" || s.Ending != EndDone {
		u.serveStream(w, s)
		return
	}
	u.serveBody(w, s)
}

func (u *Upstream) serveBody(w http.ResponseWriter, s Script) {
	for k, vs := range s.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(statusOr(s.Status))
	_, _ = w.Write([]byte(s.Body))
}

// serveStream hijacks the connection so the ending can be controlled exactly.
// Returning from a handler always produces a well-formed terminator, which
// makes the truncation cases impossible to express any other way.
func (u *Upstream) serveStream(w http.ResponseWriter, s Script) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("fake upstream needs a hijackable ResponseWriter")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", statusOr(s.Status), http.StatusText(statusOr(s.Status)))
	fmt.Fprint(&head, "Content-Type: text/event-stream\r\n")
	fmt.Fprint(&head, "Cache-Control: no-cache\r\n")
	fmt.Fprint(&head, "Transfer-Encoding: chunked\r\n")
	if s.Gzip {
		fmt.Fprint(&head, "Content-Encoding: gzip\r\n")
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprint(&head, "\r\n")
	_, _ = rw.Write(head.Bytes())
	_ = rw.Flush()

	payload := buildPayload(s)

	// Compression is applied to the whole body, as a vendor's would be.
	if s.Gzip {
		var zbuf bytes.Buffer
		zw := gzip.NewWriter(&zbuf)
		_, _ = zw.Write([]byte(payload))
		_ = zw.Close()
		writeChunk(rw, zbuf.String())
		finish(conn, rw, s)
		return
	}

	frames := splitFrames(payload)
	for i, frame := range frames {
		if i < len(s.SSE) && s.SSE[i].Delay > 0 {
			time.Sleep(s.SSE[i].Delay)
		}
		// The final frame is where a truncation has to happen, so it is
		// written as a partial chunk rather than a complete one.
		if s.Ending == EndTruncateMidFrame && i == len(frames)-1 {
			fmt.Fprintf(rw, "%x\r\n%s", len(frame), frame[:len(frame)/2])
			_ = rw.Flush()
			resetOrClose(conn, false)
			return
		}
		writeChunk(rw, frame)
	}
	finish(conn, rw, s)
}

func finish(conn net.Conn, rw *bufio.ReadWriter, s Script) {
	switch s.Ending {
	case EndReset:
		_ = rw.Flush()
		resetOrClose(conn, true)
	case EndHang:
		_ = rw.Flush()
		d := s.HangFor
		if d == 0 {
			d = 5 * time.Second
		}
		time.Sleep(d)
	default:
		// A well-formed chunked terminator: the body ends cleanly, whether or
		// not it contained a [DONE] sentinel.
		_, _ = rw.WriteString("0\r\n\r\n")
		_ = rw.Flush()
	}
}

func buildPayload(s Script) string {
	var b strings.Builder
	for _, e := range s.SSE {
		b.WriteString(e.Raw)
		b.WriteString("\n\n")
	}
	if s.FinishReason != "" {
		fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-fake\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"finish_reason\":%q,\"delta\":{}}]}\n\n", s.FinishReason)
	}
	if s.Usage {
		b.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1042,\"completion_tokens\":387,\"total_tokens\":1429}}\n\n")
	}
	if s.Ending == EndDone {
		b.WriteString("data: [DONE]\n\n")
	}
	return b.String()
}

// splitFrames keeps SSE event boundaries intact so each becomes its own chunk,
// which is what makes per-event timing meaningful in tests.
func splitFrames(payload string) []string {
	parts := strings.SplitAfter(payload, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeChunk(rw *bufio.ReadWriter, data string) {
	fmt.Fprintf(rw, "%x\r\n%s\r\n", len(data), data)
	_ = rw.Flush()
}

// resetOrClose sends a TCP RST when asked, which is what distinguishes
// ECONNRESET from a plain close in the classification tests.
func resetOrClose(conn net.Conn, reset bool) {
	if tcp, ok := conn.(*net.TCPConn); ok && reset {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func statusOr(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

func readAll(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	_ = r.Body.Close()
	return buf.Bytes()
}

// StreamOf builds a script that streams n content events and finishes cleanly.
func StreamOf(n int, ending Ending) Script {
	s := Script{Ending: ending, FinishReason: "stop", Usage: true}
	for i := 0; i < n; i++ {
		s.SSE = append(s.SSE, Event{
			Raw: fmt.Sprintf(`data: {"id":"chatcmpl-fake","choices":[{"index":0,"delta":{"content":"tok%d"}}]}`, i),
		})
	}
	return s
}
