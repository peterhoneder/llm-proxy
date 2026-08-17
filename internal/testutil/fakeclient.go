package testutil

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Client is a hand-rolled HTTP client over a raw TCP connection.
//
// http.Client cannot express the states these tests need: it will not let you
// stop reading a response while staying connected, and it closes the connection
// for you at moments that blur exactly the distinction under test. Writing the
// request by hand is the only way to control the client side precisely.
type Client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// Dial opens a connection to the proxy.
func Dial(t *testing.T, addr string) *Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing proxy: %v", err)
	}
	c := &Client{t: t, conn: conn, br: bufio.NewReader(conn)}
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

// Send writes a request. Extra headers are appended verbatim.
func (c *Client) Send(method, path, host, body string, headers ...string) {
	c.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	fmt.Fprint(&b, "Content-Type: application/json\r\n")
	for _, h := range headers {
		fmt.Fprintf(&b, "%s\r\n", h)
	}
	fmt.Fprint(&b, "\r\n")
	b.WriteString(body)

	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		c.t.Fatalf("writing request: %v", err)
	}
}

// ReadStatusLine reads and returns the response status line.
func (c *Client) ReadStatusLine() string {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("reading status line: %v", err)
	}
	return strings.TrimSpace(line)
}

// ReadHeaders consumes the header block and returns it.
func (c *Client) ReadHeaders() map[string]string {
	c.t.Helper()
	headers := map[string]string{}
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("reading headers: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return headers
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
}

// ReadSome reads whatever body bytes are available within the timeout. It does
// not decode chunked framing: these tests care that bytes arrive, not what they
// decode to.
func (c *Client) ReadSome(timeout time.Duration) string {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	n, _ := c.br.Read(buf)
	return string(buf[:n])
}

// ReadUntil reads until the wanted substring appears or the timeout expires.
func (c *Client) ReadUntil(want string, timeout time.Duration) (string, bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var sb strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(deadline)
		n, err := c.br.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), want) {
				return sb.String(), true
			}
		}
		if err != nil {
			break
		}
	}
	return sb.String(), false
}

// HangUp closes the connection gracefully, sending a FIN.
//
// Worth noting for anyone reading a failing test: the proxy's very next write
// still succeeds after this. The kernel accepts it, the peer answers with an
// RST, and only the write after that fails. That lag is exactly why the proxy
// watches the request context rather than waiting for a write error.
func (c *Client) HangUp() {
	_ = c.conn.Close()
}

// Abort sends a TCP RST instead of a FIN, which surfaces immediately.
func (c *Client) Abort() {
	if tcp, ok := c.conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = c.conn.Close()
}

// Freeze stops reading while holding the connection open, so the receive window
// fills and the proxy's writes eventually block. This is the "connected but not
// reading" case, which is distinct from a disconnect and must be reported as
// such.
func (c *Client) Freeze() {
	// Simply not reading is the whole behaviour; shrinking the buffer makes the
	// window fill after a few kilobytes instead of a few hundred.
	if tcp, ok := c.conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
}
