// Package record holds everything observed about one proxied request.
//
// A Request is written from several goroutines — the handler, the copy loop,
// the stall watchdog, the client watcher, and httptrace hooks that run on the
// transport's own goroutines — so every field is guarded and all mutation goes
// through methods. The renderer and the span exporter both read a Snapshot,
// which is taken under the lock.
package record

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/analyze"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/ratelimit"
)

// Warning is something worth telling the operator that is not a failure:
// finish_reason=length, a long inter-chunk gap, a nearly-exhausted rate limit.
type Warning struct {
	Kind fault.Kind
	Text string
}

// ConnTimeline is what httptrace observed while establishing and using one
// upstream connection. Reused and IdleTime matter disproportionately: an EOF on
// a connection taken from the pool after a long idle period is a stale
// keep-alive, and reporting it as a vendor outage would be a misdiagnosis.
type ConnTimeline struct {
	HostPort string

	DNSStart, DNSDone time.Time
	DNSAddrs          []net.IPAddr
	DNSErr            error
	DNSCoalesced      bool

	ConnectStart, ConnectDone time.Time
	RemoteAddr, LocalAddr     string
	DialErr                   error

	TLSStart, TLSDone time.Time
	TLSVersion        uint16
	TLSCipher         uint16
	ALPN              string
	TLSErr            error
	CertNotAfter      time.Time
	CertSubject       string

	GotConn  time.Time
	Reused   bool
	WasIdle  bool
	IdleTime time.Duration

	WroteRequest time.Time
	WriteErr     error

	FirstByte time.Time

	// GotConnCount counts connection acquisitions inside a single RoundTrip.
	// More than one means the transport silently replayed the request, which
	// otherwise makes the timings nonsense with no visible explanation.
	GotConnCount int
}

// Durations derived from the timeline; zero means "did not happen".
func (c *ConnTimeline) DNSTime() time.Duration     { return sub(c.DNSStart, c.DNSDone) }
func (c *ConnTimeline) ConnectTime() time.Duration { return sub(c.ConnectStart, c.ConnectDone) }
func (c *ConnTimeline) TLSTime() time.Duration     { return sub(c.TLSStart, c.TLSDone) }

func sub(a, b time.Time) time.Duration {
	if a.IsZero() || b.IsZero() || !b.After(a) {
		return 0
	}
	return b.Sub(a)
}

// Attempt is one try against the upstream. Retries add attempts; nothing is
// ever overwritten, so the console can show what each try did.
type Attempt struct {
	// mu guards everything below. The httptrace hooks that fill Conn run on
	// the transport's own goroutines, so this cannot be left unsynchronised.
	mu sync.Mutex

	N     int
	Start time.Time
	End   time.Time

	Conn ConnTimeline

	// SentHeaders is the outbound header block exactly as written on the wire,
	// captured by httptrace. Values are redacted at render time, never here.
	SentHeaders http.Header

	Status      int
	RespHeaders http.Header
	TTFB        time.Duration

	RateLimit *ratelimit.Snapshot
	ErrorBody []byte

	// WaitReason spells out the retry arithmetic, e.g.
	// "max(retry-after 1s, reset 1s, backoff 500ms)".
	WaitReason string
	Waited     time.Duration

	Fault *fault.Fault
}

// WithConn mutates the connection timeline under the attempt's lock.
func (a *Attempt) WithConn(fn func(*ConnTimeline)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.Conn)
}

// AddSentHeader records one outbound header exactly as written on the wire.
func (a *Attempt) AddSentHeader(key string, values []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SentHeaders == nil {
		a.SentHeaders = make(http.Header, 12)
	}
	a.SentHeaders[key] = append([]string(nil), values...)
}

// ErrorBodyCopy returns the retained error body under the lock.
func (a *Attempt) ErrorBodyCopy() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ErrorBody
}

// Set applies a mutation under the attempt's lock.
func (a *Attempt) Set(fn func(*Attempt)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(a)
}

// AttemptView is a value copy of an Attempt, safe to read without the lock.
type AttemptView struct {
	N           int
	Start, End  time.Time
	Conn        ConnTimeline
	SentHeaders http.Header
	Status      int
	RespHeaders http.Header
	TTFB        time.Duration
	RateLimit   *ratelimit.Snapshot
	ErrorBody   []byte
	WaitReason  string
	Waited      time.Duration
	Fault       *fault.Fault
}

func (a *Attempt) view() AttemptView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AttemptView{
		N: a.N, Start: a.Start, End: a.End,
		Conn:        a.Conn,
		SentHeaders: a.SentHeaders,
		Status:      a.Status,
		RespHeaders: a.RespHeaders,
		TTFB:        a.TTFB,
		RateLimit:   a.RateLimit,
		ErrorBody:   a.ErrorBody,
		WaitReason:  a.WaitReason,
		Waited:      a.Waited,
		Fault:       a.Fault,
	}
}

// Request is the whole story of one proxied request.
type Request struct {
	mu sync.Mutex

	ID     string
	ConnID string
	Start  time.Time

	Route    string
	Provider string

	Method      string
	ClientPath  string
	ClientAddr  string
	UserAgent   string
	UpstreamURL string

	// Parsed from the client's body. Nothing here is ever sent upstream; the
	// original bytes are forwarded unmodified.
	Model          string
	Streaming      bool
	IncludeUsage   bool
	MaxTokens      *int64
	Temperature    *float64
	TopP           *float64
	NMessages      int
	ReqBody        []byte
	ReqBodyBytes   int64
	BodyReplayable bool

	// ClientRequestID is an inbound X-Request-Id, kept so a harness that
	// stamps its own correlation id can be matched to this report.
	ClientRequestID string

	// AuthName identifies which proxy token admitted the request, when the
	// listener is guarded. It is a label, never the secret.
	AuthName string

	// KeySource says where the credentials came from — injected from the
	// configured environment variable, supplied by the client, or absent.
	// Rendering it turns most 401 mysteries into one line.
	KeySource string

	// Chat marks a request the analyzer inspects. Everything else under a
	// route prefix is still proxied verbatim, just rendered as one line.
	Chat bool

	attempts []*Attempt

	Status int
	// HeadersSentAt is the retry-window sentinel: once it is set, bytes with
	// this status line are on the client's socket and nothing can be retried.
	HeadersSentAt time.Time
	TTFB          time.Duration

	// Read and written byte counts are tracked separately because they differ
	// exactly when it matters: on a failed write the last chunk was parsed and
	// not delivered.
	BytesFromUpstream int64
	BytesToClient     int64
	ChunksParsed      int
	ChunksDelivered   int

	Stream *analyze.Postmortem

	fault    *fault.Fault
	faultSet sync.Once

	Warnings []Warning

	// ClientGoneAt is when the client watcher saw the connection close. It is
	// set independently of any write failure, because a graceful FIN does not
	// fail the next write and the last chunk's write never fails at all.
	ClientGoneAt time.Time

	End      time.Time
	Duration time.Duration

	// Hot-path timestamps, read by the watchdog on another goroutine.
	lastUpstreamByte atomic.Int64
	lastClientWrite  atomic.Int64
}

// New starts a record for an inbound request.
func New(id, connID, route, provider string, r *http.Request, now time.Time) *Request {
	rec := &Request{
		ID:             id,
		ConnID:         connID,
		Start:          now,
		Route:          route,
		Provider:       provider,
		Method:         r.Method,
		ClientPath:     r.URL.Path,
		ClientAddr:     r.RemoteAddr,
		UserAgent:      r.Header.Get("User-Agent"),
		BodyReplayable: true,
	}
	rec.lastUpstreamByte.Store(now.UnixNano())
	rec.lastClientWrite.Store(now.UnixNano())
	return rec
}

// NewAttempt appends an attempt and returns it. The caller mutates the
// returned Attempt only through the Request's methods or before the attempt is
// visible to other goroutines.
func (r *Request) NewAttempt(now time.Time) *Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := &Attempt{N: len(r.attempts) + 1, Start: now}
	r.attempts = append(r.attempts, a)
	return a
}

// Attempts returns the attempts recorded so far.
func (r *Request) Attempts() []*Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Attempt, len(r.attempts))
	copy(out, r.attempts)
	return out
}

// LastAttempt returns the most recent attempt, or nil.
func (r *Request) LastAttempt() *Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.attempts) == 0 {
		return nil
	}
	return r.attempts[len(r.attempts)-1]
}

// AttemptCount reports how many upstream attempts were made.
func (r *Request) AttemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attempts)
}

// SetFault records the fault. First writer wins, so a watchdog firing a
// microsecond after a real error cannot overwrite the real one — and an
// induced upstream error can never displace the client fault that caused it.
func (r *Request) SetFault(f *fault.Fault) {
	if f == nil {
		return
	}
	r.faultSet.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.fault = f
	})
}

// Fault returns the recorded fault, or nil.
func (r *Request) Fault() *fault.Fault {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fault
}

// SetKeySource records where the request's credentials came from.
func (r *Request) SetKeySource(src string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.KeySource = src
}

// Warn appends a warning.
func (r *Request) Warn(kind fault.Kind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Warnings = append(r.Warnings, Warning{Kind: kind, Text: text})
}

// WarnUpdate records a warning, replacing any previous one of the same kind.
// A long wait reports progress repeatedly for the console; the record should
// keep one line saying how long it ended up being, not twenty.
func (r *Request) WarnUpdate(kind fault.Kind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Warnings {
		if r.Warnings[i].Kind == kind {
			r.Warnings[i].Text = text
			return
		}
	}
	r.Warnings = append(r.Warnings, Warning{Kind: kind, Text: text})
}

// TouchUpstream resets the inter-chunk clock without counting a read. The copy
// loop calls it when the body starts, so a slow first token is measured by the
// response-header budget and does not also consume the idle budget.
func (r *Request) TouchUpstream(t time.Time) {
	r.lastUpstreamByte.Store(t.UnixNano())
}

// SetClientGone records when the client's connection went away. This is the
// primary disconnect signal: a write error arrives one or two chunks late, and
// for the final chunk it never arrives at all.
func (r *Request) SetClientGone(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ClientGoneAt.IsZero() {
		r.ClientGoneAt = t
	}
}

// ClientGone reports whether the client connection was observed closed.
func (r *Request) ClientGone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.ClientGoneAt.IsZero()
}

// AddRead records bytes read from the upstream.
func (r *Request) AddRead(n int, at time.Time) {
	r.lastUpstreamByte.Store(at.UnixNano())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.BytesFromUpstream += int64(n)
	r.ChunksParsed++
}

// AddDelivered records bytes actually written to the client.
func (r *Request) AddDelivered(n int, at time.Time) {
	r.lastClientWrite.Store(at.UnixNano())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.BytesToClient += int64(n)
	if n > 0 {
		r.ChunksDelivered++
	}
}

// LastUpstreamByte and LastClientWrite are read by the watchdog on another
// goroutine, so they are atomics rather than mutex-guarded fields.
func (r *Request) LastUpstreamByte() time.Time {
	return time.Unix(0, r.lastUpstreamByte.Load())
}

func (r *Request) LastClientWrite() time.Time {
	return time.Unix(0, r.lastClientWrite.Load())
}

// MarkHeadersSent closes the retry window.
func (r *Request) MarkHeadersSent(status int, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Status = status
	if r.HeadersSentAt.IsZero() {
		r.HeadersSentAt = at
		r.TTFB = at.Sub(r.Start)
	}
}

// HeadersSent reports whether anything has been committed to the client.
// Retry is only possible while this is false.
func (r *Request) HeadersSent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.HeadersSentAt.IsZero()
}

// Finish stamps the end of the request.
func (r *Request) Finish(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.End.IsZero() {
		r.End = at
		r.Duration = at.Sub(r.Start)
	}
}

// Snapshot is a consistent value copy for rendering.
//
// It deliberately copies rather than embedding *Request: embedding would let
// the console and the span exporter read fields that other goroutines are
// still writing, which is a data race the -race build catches and a torn
// report in production.
type Snapshot struct {
	ID, ConnID string
	Start, End time.Time
	Duration   time.Duration

	Route, Provider string
	Method          string
	ClientPath      string
	ClientAddr      string
	UserAgent       string
	UpstreamURL     string
	KeySource       string
	ClientRequestID string
	AuthName        string

	Chat         bool
	Model        string
	Streaming    bool
	IncludeUsage bool
	MaxTokens    *int64
	Temperature  *float64
	TopP         *float64
	NMessages    int
	ReqBody      []byte
	ReqBodyBytes int64

	BodyReplayable bool

	Status        int
	HeadersSentAt time.Time
	TTFB          time.Duration

	BytesFromUpstream int64
	BytesToClient     int64
	ChunksParsed      int
	ChunksDelivered   int

	LastUpstreamByte time.Time
	LastClientWrite  time.Time

	Stream       *analyze.Postmortem
	ClientGoneAt time.Time

	Attempts []AttemptView
	Fault    *fault.Fault
	Warnings []Warning
}

// Snapshot returns a consistent view of the record.
func (r *Request) Snapshot() Snapshot {
	attempts := r.Attempts()
	views := make([]AttemptView, len(attempts))
	for i, a := range attempts {
		views[i] = a.view()
	}

	lastUp := r.LastUpstreamByte()
	lastDown := r.LastClientWrite()

	r.mu.Lock()
	defer r.mu.Unlock()

	warnings := make([]Warning, len(r.Warnings))
	copy(warnings, r.Warnings)

	return Snapshot{
		ID: r.ID, ConnID: r.ConnID,
		Start: r.Start, End: r.End, Duration: r.Duration,

		Route: r.Route, Provider: r.Provider,
		Method: r.Method, ClientPath: r.ClientPath,
		ClientAddr: r.ClientAddr, UserAgent: r.UserAgent,
		UpstreamURL: r.UpstreamURL, KeySource: r.KeySource,
		ClientRequestID: r.ClientRequestID, AuthName: r.AuthName,

		Chat: r.Chat, Model: r.Model, Streaming: r.Streaming,
		IncludeUsage: r.IncludeUsage, MaxTokens: r.MaxTokens,
		Temperature: r.Temperature, TopP: r.TopP,
		NMessages: r.NMessages, ReqBody: r.ReqBody, ReqBodyBytes: r.ReqBodyBytes,
		BodyReplayable: r.BodyReplayable,

		Status: r.Status, HeadersSentAt: r.HeadersSentAt, TTFB: r.TTFB,

		BytesFromUpstream: r.BytesFromUpstream,
		BytesToClient:     r.BytesToClient,
		ChunksParsed:      r.ChunksParsed,
		ChunksDelivered:   r.ChunksDelivered,

		LastUpstreamByte: lastUp,
		LastClientWrite:  lastDown,

		Stream: r.Stream, ClientGoneAt: r.ClientGoneAt,

		Attempts: views, Fault: r.fault, Warnings: warnings,
	}
}

// OK reports whether the request completed without a fault.
func (s Snapshot) OK() bool { return s.Fault == nil }

// Side returns the responsible side, or SideNone for a clean request.
func (s Snapshot) Side() fault.Side {
	if s.Fault == nil {
		return fault.SideNone
	}
	return s.Fault.Side
}
