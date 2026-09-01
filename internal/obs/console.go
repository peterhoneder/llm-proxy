package obs

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/ratelimit"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// Renderer turns a request record into the console block.
//
// The design goal is that the responsible side is obvious without reading
// carefully. Every fault report ends with a one-sentence verdict, and the
// evidence above it is ordered so the reader can check that verdict rather
// than having to derive it.
type Renderer struct {
	color    bool
	sym      symbols
	redactor *Redactor
	full     bool
	maxBody  int64
	level    slog.Level
}

// RendererOptions configure a Renderer.
type RendererOptions struct {
	Color     bool
	Symbols   string // auto | unicode | ascii
	Redactor  *Redactor
	FullTrace bool
	MaxBody   int64
	Level     slog.Level
}

// NewRenderer builds a Renderer.
func NewRenderer(o RendererOptions) *Renderer {
	if o.Redactor == nil {
		o.Redactor = NewRedactor(nil, false)
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 64 << 10
	}
	return &Renderer{
		color:    o.Color,
		sym:      pickSymbols(o.Symbols, o.Color),
		redactor: o.Redactor,
		full:     o.FullTrace,
		maxBody:  o.MaxBody,
		level:    o.Level,
	}
}

// symbols keeps the output legible where Unicode is not. A mangled glyph in a
// CI log or a mis-configured terminal is worse than a plain arrow.
type symbols struct {
	start, end, retry string
	ok, warn, bad     string
	bullet            string
}

var unicodeSymbols = symbols{start: "→", end: "←", retry: "⟳", ok: "✓", warn: "⚠", bad: "✗", bullet: "·"}
var asciiSymbols = symbols{start: "->", end: "<-", retry: "~>", ok: "ok", warn: "!", bad: "X", bullet: "-"}

func pickSymbols(mode string, color bool) symbols {
	switch strings.ToLower(mode) {
	case "ascii":
		return asciiSymbols
	case "unicode":
		return unicodeSymbols
	default:
		// "auto": the same conditions that suppress colour usually indicate a
		// destination that will not render glyphs well either.
		if color {
			return unicodeSymbols
		}
		return asciiSymbols
	}
}

// ANSI styling, applied only when colour is on.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiPurple = "\x1b[35m"
	ansiCyan   = "\x1b[36m"
)

func (r *Renderer) paint(code, s string) string {
	if !r.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

// sideColour maps responsibility to colour: client yellow, upstream red,
// proxy magenta, clean green.
func (r *Renderer) sideColour(side fault.Side) string {
	switch side {
	case fault.SideClient:
		return ansiYellow
	case fault.SideUpstream:
		return ansiRed
	case fault.SideProxy:
		return ansiPurple
	default:
		return ansiGreen
	}
}

// RenderStart is the one-line "request began" notice, printed as soon as the
// request arrives so a hang is visible while it is happening rather than only
// afterwards.
func (r *Renderer) RenderStart(snap record.Snapshot) string {
	var b strings.Builder
	b.WriteString(r.stamp(snap.Start, slog.LevelInfo))
	b.WriteString(r.paint(ansiBlue, r.sym.start))
	b.WriteByte(' ')
	b.WriteString(r.paint(ansiBold, snap.Route))
	b.WriteString("  ")
	b.WriteString(snap.Method + " " + snap.ClientPath)

	if snap.Chat {
		if snap.Model != "" {
			b.WriteString("  chat " + snap.Model)
		}
		if snap.Streaming {
			b.WriteString("  stream")
		}
		if snap.NMessages > 0 {
			b.WriteString("  msgs=" + strconv.Itoa(snap.NMessages))
		}
	}
	b.WriteString("  " + r.paint(ansiDim, "req="+snap.ID))
	b.WriteByte('\n')
	return b.String()
}

// RenderRetry is printed when an attempt is about to be retried, so a long
// rate-limit wait is visible while it happens.
func (r *Renderer) RenderRetry(snap record.Snapshot, a record.AttemptView, maxAttempts int, wait time.Duration) string {
	var b strings.Builder
	b.WriteString(r.stamp(time.Now(), slog.LevelWarn))
	b.WriteString(r.paint(ansiYellow, r.sym.retry))
	b.WriteByte(' ')
	b.WriteString(r.paint(ansiBold, snap.Route))
	b.WriteString("  ")
	b.WriteString(r.paint(ansiYellow, statusText(a.Status)))
	fmt.Fprintf(&b, "  attempt %d/%d — waiting %s", a.N, maxAttempts, fmtDur(wait))
	b.WriteString("  " + r.paint(ansiDim, "req="+snap.ID))
	b.WriteByte('\n')

	r.writeRateLimit(&b, a.RateLimit)
	if a.WaitReason != "" {
		r.field(&b, "wait", fmtDur(wait)+" = "+a.WaitReason)
	}
	if len(a.ErrorBody) > 0 {
		r.field(&b, "body", r.truncBody(a.ErrorBody))
	}
	return b.String()
}

// Render produces the terminal block for a finished request.
func (r *Renderer) Render(snap record.Snapshot) string {
	var b strings.Builder
	f := snap.Fault
	level := levelFor(snap)

	b.WriteString(r.stamp(snap.End, level))
	b.WriteString(r.paint(r.sideColour(snap.Side()), r.sym.end))
	b.WriteByte(' ')
	b.WriteString(r.paint(ansiBold, snap.Route))
	b.WriteString("  ")
	b.WriteString(r.headline(snap))
	b.WriteString("  " + r.paint(ansiDim, "req="+snap.ID))
	b.WriteByte('\n')

	// A clean, unremarkable request is one line. Detail is earned.
	if f == nil && len(snap.Warnings) == 0 && !r.full {
		return b.String()
	}

	// The request line repeats in every detail block: without it, a fault
	// report does not say what was actually asked for.
	if f != nil || r.full {
		r.field(&b, "request", r.requestSummary(snap))
	}

	if f != nil {
		r.writeFault(&b, snap, f)
	}
	r.writeAuth(&b, snap)
	r.writeStream(&b, snap)
	r.writeWarnings(&b, snap)

	last, haveAttempt := lastAttempt(snap)
	if haveAttempt {
		if f != nil || r.full {
			r.field(&b, "conn", r.connSummary(last))
		}
		r.writeAPIError(&b, snap, last)
		if last.RateLimit != nil && !last.RateLimit.Empty() {
			r.writeRateLimit(&b, last.RateLimit)
		}
	}

	// On any fault or non-2xx, every response header is printed regardless of
	// level. The spec for this tool is explicit that no detail may be hidden,
	// faults are rare, and the one header nobody thought to look for is
	// exactly the one that explains the failure.
	if haveAttempt && (f != nil || (snap.Status != 0 && snap.Status >= 400)) {
		r.writeHeaders(&b, "response", last.RespHeaders)
	}

	if r.full {
		r.writeFullTrace(&b, snap)
	}

	if f != nil {
		r.field(&b, "verdict", r.paint(ansiBold+r.sideColour(f.Side), f.Verdict))
	}
	return b.String()
}

func (r *Renderer) headline(snap record.Snapshot) string {
	f := snap.Fault
	var parts []string

	if f != nil && f.Side != fault.SideNone {
		parts = append(parts, r.paint(ansiBold+r.sideColour(f.Side), faultHeadline(f)))
		if snap.Status != 0 {
			parts = append(parts, statusText(snap.Status))
		}
	} else {
		parts = append(parts, statusText(snap.Status))
		if w := warningHeadline(snap); w != "" {
			parts = append(parts, r.paint(ansiBold+ansiYellow, w))
		}
	}

	parts = append(parts, fmtDur(snap.Duration))
	if snap.TTFB > 0 {
		parts = append(parts, "ttfb="+fmtDur(snap.TTFB))
	}
	if n := len(snap.Attempts); n > 1 {
		parts = append(parts, "attempts="+strconv.Itoa(n))
	}

	if s := snap.Stream; s != nil && snap.Streaming {
		parts = append(parts, plural(s.DataEvents, "chunk"))
	}
	if s := snap.Stream; s != nil && s.Usage != nil {
		parts = append(parts, "in="+strconv.FormatInt(s.Usage.InputTokens, 10),
			"out="+strconv.FormatInt(s.Usage.OutputTokens, 10))
	}
	if s := snap.Stream; s != nil && len(s.FinishReasons) > 0 {
		parts = append(parts, "finish="+strings.Join(s.FinishReasons, ","))
	}
	if f == nil && len(snap.Warnings) == 0 {
		parts = append(parts, r.paint(ansiGreen, r.sym.ok))
	}
	return strings.Join(parts, "  ")
}

func faultHeadline(f *fault.Fault) string {
	switch f.Kind {
	case fault.KindClientDisconnect, fault.KindClientEPIPE, fault.KindClientReset:
		return "CLIENT DISCONNECTED"
	case fault.KindClientStalled:
		return "CLIENT STOPPED READING"
	case fault.KindClientUpload:
		return "CLIENT DISCONNECTED WHILE UPLOADING"
	case fault.KindTruncatedBody, fault.KindTruncatedStream:
		return "UPSTREAM TRUNCATED"
	case fault.KindErrorInStream:
		return "UPSTREAM ERROR MID-STREAM"
	case fault.KindStallTimeout, fault.KindReadTimeout:
		return "UPSTREAM STALLED"
	case fault.KindIdleReuseEOF:
		return "STALE KEEP-ALIVE"
	case fault.KindContextLength:
		return "CONTEXT LENGTH EXCEEDED"
	case fault.KindRateLimited:
		return "RATE LIMITED"
	case fault.KindProxyShutdown:
		return "PROXY SHUTDOWN"
	case fault.KindConnRefused:
		return "UPSTREAM UNREACHABLE"
	default:
		return strings.ToUpper(strings.ReplaceAll(string(f.Kind), "_", " "))
	}
}

func warningHeadline(snap record.Snapshot) string {
	for _, w := range snap.Warnings {
		switch w.Kind {
		case fault.KindOutputTruncated:
			return "OUTPUT TRUNCATED (max tokens)"
		case fault.KindContentFilter:
			return "CONTENT FILTERED"
		}
	}
	return ""
}

func (r *Renderer) writeFault(b *strings.Builder, snap record.Snapshot, f *fault.Fault) {
	line := "side=" + r.paint(r.sideColour(f.Side), f.Side.String()) +
		"  kind=" + r.paint(ansiRed, string(f.Kind))
	if f.Op != "" {
		line += "  op=" + f.Op
	}
	if f.Syscall != "" {
		line += "  syscall=" + f.Syscall
	}
	r.field(b, "fault", line)

	if f.Detail != "" {
		r.field(b, "cause", f.Detail)
	}
	if f.Err != nil {
		if msg := f.Err.Error(); msg != "" && msg != f.Detail {
			r.field(b, "error", msg)
		}
	}

	// The two clocks that answer "which side went quiet first". When they
	// differ materially the answer is immediate.
	if !snap.HeadersSentAt.IsZero() {
		up := snap.End.Sub(snap.LastUpstreamByte)
		down := snap.End.Sub(snap.LastClientWrite)
		r.field(b, "timing", "last upstream byte "+fmtDur(up)+" ago "+r.sym.bullet+
			" last downstream write "+fmtDur(down)+" ago")
	}

	if !snap.ClientGoneAt.IsZero() {
		r.field(b, "client", "connection observed closed at "+snap.ClientGoneAt.Format("15:04:05.000"))
	}
	if f.Side == fault.SideClient && snap.Status >= 200 && snap.Status < 300 {
		r.field(b, "upstream", "healthy — "+statusText(snap.Status)+
			"; the proxy aborted the upstream read after the client left")
	}
}

// writeAuth explains where the credentials came from.
//
// On a 401 or 403 this is almost always the answer, and without it the operator
// is left guessing whether the proxy substituted a key, passed the client's
// through, or sent none at all.
func (r *Renderer) writeAuth(b *strings.Builder, snap record.Snapshot) {
	unauthorised := snap.Status == 401 || snap.Status == 403
	if !unauthorised && !r.full {
		return
	}

	if snap.AuthName != "" {
		r.field(b, "client", "authenticated to the proxy as "+snap.AuthName)
	}

	switch snap.KeySource {
	case "injected":
		r.field(b, "auth", "injected by the proxy from the route's api_key_env")
	case "client-supplied":
		r.field(b, "auth", "the client sent its own Authorization header; the proxy did not replace it")
	default:
		msg := "no Authorization was sent at all"
		if unauthorised {
			msg += " — the route's api_key_env is unset or empty, and the client sent no header of its own"
		}
		r.field(b, "auth", r.paint(ansiYellow, msg))
	}
}

func (r *Renderer) writeStream(b *strings.Builder, snap record.Snapshot) {
	s := snap.Stream
	if s == nil {
		return
	}
	if s.AnalysisUnavailable != "" {
		r.field(b, "stream", "analysis unavailable ("+s.AnalysisUnavailable+
			") — the verdict rests on transport signals only")
		return
	}
	if !snap.Streaming && snap.Fault == nil {
		return
	}

	parts := []string{
		plural(snap.ChunksParsed, "read") + " parsed",
		strconv.Itoa(snap.ChunksDelivered) + " delivered",
		fmtBytes(snap.BytesFromUpstream) + " read / " + fmtBytes(snap.BytesToClient) + " written",
	}
	if snap.Streaming {
		parts = append([]string{plural(s.DataEvents, "event")}, parts...)
	}
	if s.MaxGap > 0 {
		parts = append(parts, "max gap "+fmtDur(s.MaxGap)+" after event "+strconv.Itoa(s.MaxGapAfterEvent))
	}
	if snap.Streaming {
		if s.DoneSeen {
			parts = append(parts, "[DONE] "+r.sym.ok)
		} else if s.FinishSeen {
			parts = append(parts, "no [DONE] sentinel (backend does not send one)")
		} else {
			parts = append(parts, "no [DONE]")
		}
	}
	r.field(b, "stream", strings.Join(parts, "  "+r.sym.bullet+" "))

	// The unterminated tail is the strongest evidence of a mid-answer cut, so
	// it is printed raw.
	if len(s.Trailing) > 0 {
		r.field(b, "partial", strconv.Itoa(len(s.Trailing))+" bytes unterminated: `"+
			oneLine(string(s.Trailing), 120)+"`")
	}
	if len(s.StreamError) > 0 {
		r.field(b, "stream-error", oneLine(string(s.StreamError), 400))
	}
	if s.ParseErrors > 0 {
		r.field(b, "parse", strconv.Itoa(s.ParseErrors)+" frames could not be parsed as JSON")
	}
}

func (r *Renderer) writeWarnings(b *strings.Builder, snap record.Snapshot) {
	for _, w := range snap.Warnings {
		label := "warn"
		switch w.Kind {
		case fault.KindOutputTruncated:
			label = "finish"
		case fault.KindContentFilter:
			label = "filtered"
		}
		r.field(b, label, r.paint(ansiYellow, w.Text))
	}
	if s := snap.Stream; s != nil && s.Usage != nil && (snap.Fault != nil || len(snap.Warnings) > 0 || r.full) {
		u := s.Usage
		line := fmt.Sprintf("in=%d  out=%d  total=%d", u.InputTokens, u.OutputTokens, u.TotalTokens)
		if u.CachedInputTokens > 0 {
			line += fmt.Sprintf("  cached=%d", u.CachedInputTokens)
		}
		if snap.MaxTokens != nil {
			line += fmt.Sprintf("   (requested max_tokens=%d)", *snap.MaxTokens)
		}
		r.field(b, "usage", line)
	}
}

func (r *Renderer) writeAPIError(b *strings.Builder, snap record.Snapshot, a record.AttemptView) {
	if len(a.ErrorBody) == 0 {
		return
	}
	r.field(b, "body", r.truncBody(a.ErrorBody))
}

func (r *Renderer) writeRateLimit(b *strings.Builder, s *ratelimit.Snapshot) {
	if s == nil || s.Empty() {
		return
	}
	for _, bucket := range s.Buckets {
		line := padRight(bucket.Name, 14)
		if bucket.Remaining != nil && bucket.Limit != nil {
			line += fmt.Sprintf("%d/%d left", *bucket.Remaining, *bucket.Limit)
		} else if bucket.Remaining != nil {
			line += fmt.Sprintf("%d left", *bucket.Remaining)
		}
		if bucket.Reset != nil {
			line += "  reset in " + fmtDur(*bucket.Reset) + "  (" + bucket.ResetFmt.String() + ")"
		}
		if bucket.Exhausted() {
			line += "  " + r.paint(ansiRed, "EXHAUSTED")
		}
		r.field(b, "ratelimit", line)
	}
	if s.RetryAfter != nil {
		r.field(b, "retry-after", "`"+s.RetryAfterRaw+"` ("+s.RetryAfterFmt.String()+") → "+fmtDur(*s.RetryAfter))
	}
	// Printing what we could not parse is how an undocumented vendor dialect
	// gets discovered.
	for _, kv := range s.Unrecognised {
		r.field(b, "unparsed", kv.Key+": "+kv.Value)
	}
	if s.ClockSkew > time.Second || s.ClockSkew < -time.Second {
		r.field(b, "clock-skew", fmtDur(s.ClockSkew)+" between this host and the vendor")
	}
}

func (r *Renderer) writeHeaders(b *strings.Builder, label string, h map[string][]string) {
	for _, line := range r.redactor.Headers(h) {
		r.field(b, label, line.Key+": "+line.Value)
		label = ""
	}
}

func (r *Renderer) writeFullTrace(b *strings.Builder, snap record.Snapshot) {
	for _, a := range snap.Attempts {
		r.field(b, "attempt", strconv.Itoa(a.N)+" "+r.connDetail(a))
		if len(a.SentHeaders) > 0 {
			r.writeHeaders(b, "request hdr", a.SentHeaders)
		}
		if len(a.RespHeaders) > 0 && snap.Fault == nil && snap.Status < 400 {
			r.writeHeaders(b, "response", a.RespHeaders)
		}
	}
	if len(snap.ReqBody) > 0 {
		r.field(b, "request body", r.truncBody(snap.ReqBody))
	}
}

func (r *Renderer) requestSummary(snap record.Snapshot) string {
	parts := []string{snap.Method + " " + snap.ClientPath}
	if snap.Model != "" {
		parts = append(parts, "chat "+snap.Model)
	}
	if snap.Streaming {
		parts = append(parts, "stream")
	}
	if snap.NMessages > 0 {
		parts = append(parts, "msgs="+strconv.Itoa(snap.NMessages))
	}
	if snap.ReqBodyBytes > 0 {
		parts = append(parts, "body="+fmtBytes(snap.ReqBodyBytes))
	}
	if snap.ClientRequestID != "" {
		parts = append(parts, "x-request-id="+snap.ClientRequestID)
	}
	if snap.MaxTokens != nil {
		parts = append(parts, "max_tokens="+strconv.FormatInt(*snap.MaxTokens, 10))
	}
	if !snap.BodyReplayable {
		parts = append(parts, r.paint(ansiDim, "retry=unavailable (body too large to replay)"))
	}
	if len(snap.StrippedParams) > 0 {
		// A fault report that blames a side has to disclose that the proxy
		// edited the request first. Yellow, not dim: this is the one thing on
		// the line that was not the client's doing.
		parts = append(parts, r.paint(ansiYellow,
			"stripped="+strings.Join(snap.StrippedParams, ",")))
	}
	return strings.Join(parts, "  ")
}

func (r *Renderer) connSummary(a record.AttemptView) string {
	c := &a.Conn
	var parts []string
	if c.Reused {
		s := "reused=true"
		if c.IdleTime > 0 {
			s += " idle=" + fmtDur(c.IdleTime)
		}
		parts = append(parts, s)
	} else {
		parts = append(parts, "reused=false")
	}
	if c.ALPN != "" {
		parts = append(parts, "alpn="+c.ALPN)
	}
	if c.RemoteAddr != "" {
		parts = append(parts, "peer="+c.RemoteAddr)
	}
	if c.TLSVersion != 0 {
		parts = append(parts, tlsVersionName(c.TLSVersion))
	}
	if a.TTFB > 0 {
		parts = append(parts, "ttfb="+fmtDur(a.TTFB))
	}
	// More than one connection inside a single RoundTrip means the transport
	// replayed the request behind our back; unexplained, it makes every timing
	// above look wrong.
	if c.GotConnCount > 1 {
		parts = append(parts, r.paint(ansiYellow, fmt.Sprintf("%s transport silently retried %dx",
			r.sym.warn, c.GotConnCount-1)))
	}
	return strings.Join(parts, "  "+r.sym.bullet+" ")
}

func (r *Renderer) connDetail(a record.AttemptView) string {
	c := &a.Conn
	var parts []string
	if d := c.DNSTime(); d > 0 {
		s := "dns " + fmtDur(d)
		if len(c.DNSAddrs) > 0 {
			s += " → " + c.DNSAddrs[0].String()
		}
		parts = append(parts, s)
	}
	if d := c.ConnectTime(); d > 0 {
		parts = append(parts, "connect "+fmtDur(d))
	}
	if d := c.TLSTime(); d > 0 {
		s := tlsVersionName(c.TLSVersion) + " " + fmtDur(d)
		if c.ALPN != "" {
			s += " alpn=" + c.ALPN
		}
		if !c.CertNotAfter.IsZero() {
			s += " cert expires " + c.CertNotAfter.Format("2006-01-02")
		}
		parts = append(parts, s)
	}
	if c.Reused {
		parts = append(parts, "reused after "+fmtDur(c.IdleTime)+" idle")
	}
	if a.TTFB > 0 {
		parts = append(parts, "ttfb "+fmtDur(a.TTFB))
	}
	if len(parts) == 0 {
		return "(no connection timings recorded)"
	}
	return strings.Join(parts, "  "+r.sym.bullet+" ")
}

// field writes one indented "label   value" line, wrapping continuation lines
// under the value column so a long message stays readable.
func (r *Renderer) field(b *strings.Builder, label, value string) {
	const indent = "                 "
	const labelWidth = 12

	b.WriteString(indent)
	b.WriteString(r.paint(ansiDim, padRight(label, labelWidth)))

	lines := strings.Split(value, "\n")
	for i, line := range lines {
		if i > 0 {
			b.WriteString(indent)
			b.WriteString(strings.Repeat(" ", labelWidth))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func (r *Renderer) stamp(t time.Time, level slog.Level) string {
	if t.IsZero() {
		t = time.Now()
	}
	label := "INF"
	colour := ansiGreen
	switch {
	case level >= slog.LevelError:
		label, colour = "ERR", ansiRed
	case level >= slog.LevelWarn:
		label, colour = "WRN", ansiYellow
	case level < slog.LevelInfo:
		label, colour = "DBG", ansiCyan
	}
	return t.Format("15:04:05.000") + " " + r.paint(colour, label) + " "
}

func (r *Renderer) truncBody(body []byte) string {
	if int64(len(body)) <= r.maxBody {
		return oneLine(string(body), 0)
	}
	return oneLine(string(body[:r.maxBody]), 0) +
		fmt.Sprintf("… truncated, %s total", fmtBytes(int64(len(body))))
}

// oneLine collapses newlines so a JSON body cannot break the block layout.
func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// lastAttempt returns the most recent attempt and whether there was one.
func lastAttempt(snap record.Snapshot) (record.AttemptView, bool) {
	if len(snap.Attempts) == 0 {
		return record.AttemptView{}, false
	}
	return snap.Attempts[len(snap.Attempts)-1], true
}

func statusText(status int) string {
	if status == 0 {
		return "no response"
	}
	return strconv.Itoa(status) + " " + statusName(status)
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " kB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// fmtDur keeps durations to three significant figures, because "4.85s" reads
// faster than "4.851234s" and nothing here needs more precision.
func fmtDur(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + fmtDur(-d)
	case d == 0:
		return "0s"
	case d < time.Millisecond:
		return strconv.FormatInt(int64(d/time.Microsecond), 10) + "µs"
	case d < time.Second:
		return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
	default:
		return d.Round(time.Second).String()
	}
}

func tlsVersionName(v uint16) string {
	switch v {
	case 0x0301:
		return "tls1.0"
	case 0x0302:
		return "tls1.1"
	case 0x0303:
		return "tls1.2"
	case 0x0304:
		return "tls1.3"
	default:
		return "tls?"
	}
}

// summaryLine is the one-line message used for the structured record that goes
// to OpenTelemetry, where the rendered block would be noise.
func summaryLine(snap record.Snapshot) string {
	if f := snap.Fault; f != nil {
		return snap.Route + " " + string(f.Kind) + " (" + f.Side.String() + ")"
	}
	return snap.Route + " " + statusText(snap.Status)
}

// plural renders a count with its noun, so a single event does not read as
// "1 events".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func statusName(status int) string {
	if name := http.StatusText(status); name != "" {
		return name
	}
	return "Unknown"
}
