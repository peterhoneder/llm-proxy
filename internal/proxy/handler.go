package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/analyze"
	"github.com/peterhoneder/llm-proxy/internal/config"
	"github.com/peterhoneder/llm-proxy/internal/conntrace"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/oaierr"
	"github.com/peterhoneder/llm-proxy/internal/obs"
	"github.com/peterhoneder/llm-proxy/internal/ratelimit"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

const maxErrorBody = 64 << 10

// attemptView takes a lock-safe copy of the most recent attempt for rendering.
func attemptView(rec *record.Request) record.AttemptView {
	views := rec.Snapshot().Attempts
	if len(views) == 0 {
		return record.AttemptView{}
	}
	return views[len(views)-1]
}

// routeHandler proxies every path under one route prefix.
type routeHandler struct {
	srv    *Server
	target *target
}

// ServeHTTP proxies one request and reports what happened to it.
func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := h.srv.now()

	// The span wraps everything, and r is re-based on its context so every
	// log record made below carries the trace and span ids. With OpenTelemetry
	// unconfigured the provider is a no-op and this costs nothing.
	ctx, span := obs.StartRequestSpan(r.Context(), h.target.route.Name)
	defer span.End()
	r = r.WithContext(ctx)

	rec := record.New(h.srv.nextRequestID(), connIDFrom(r.Context()),
		h.target.route.Name, h.target.route.Provider, r, now)
	rec.Chat = isChatCompletions(r.Method, r.URL.Path)
	rec.ClientRequestID = r.Header.Get("X-Request-Id")
	rec.AuthName = authNameFrom(r.Context())

	cw := &countingWriter{ResponseWriter: w}

	// The upstream context is deliberately detached from the request context
	// and cancelled only by us, with a stamped reason.
	//
	// Deriving it from r.Context() — the idiomatic choice — would invert the
	// whole attribution model: a client hangup would cancel the upstream read,
	// and the single most important case in this tool would be blamed on the
	// vendor or on the proxy. Go's server cancels a request context with a bare
	// context.Canceled and no cause, so the cause set here is the only reliable
	// explanation available afterwards.
	upCtx, cancelUp := context.WithCancelCause(context.WithoutCancel(r.Context()))
	defer cancelUp(fault.ErrHandlerReturn)

	copyDone := make(chan struct{})
	go h.watchClient(r, rec, upCtx, cancelUp, copyDone)
	defer close(copyDone)

	h.proxy(cw, r, rec, upCtx, cancelUp)

	rec.Finish(h.srv.now())
	snap := rec.Snapshot()
	obs.ApplySpan(span, snap)
	h.srv.log.Request(ctx, snap)
	if h.srv.onRecord != nil {
		h.srv.onRecord(snap)
	}

	// Returning normally after a truncated stream would emit a well-formed
	// chunked terminator, and the client would accept an incomplete answer as
	// complete. Aborting the connection is the honest signal. This runs last,
	// after the report has been written.
	if f := rec.Fault(); f != nil && h.shouldAbort(rec, f) {
		panic(http.ErrAbortHandler)
	}
}

func (h *routeHandler) shouldAbort(rec *record.Request, f *fault.Fault) bool {
	if !boolOr(h.target.route.AbortOnTruncation, true) {
		return false
	}
	// Aborting only makes sense once a 2xx is already on the wire. Before that
	// the status code carries the bad news by itself, and tearing down the
	// connection would destroy a response the client could have read.
	if !rec.HeadersSent() {
		return false
	}
	switch f.Kind {
	case fault.KindTruncatedBody, fault.KindTruncatedStream, fault.KindErrorInStream:
		// Only meaningful once a 2xx is already on the wire; before that the
		// status code carries the bad news by itself.
		return true
	default:
		return false
	}
}

// watchClient turns a client disconnect into a prompt, attributable event.
//
// Waiting for a write to fail is not enough. A graceful FIN does not fail the
// next write — the kernel accepts it, and only the write after the peer's RST
// fails — so that signal arrives a chunk or two late, and for the final chunk
// it never arrives at all. Worse, a client that leaves while the proxy is
// blocked reading a silent upstream would go unnoticed until the stall watchdog
// fired and blamed the vendor for it.
func (h *routeHandler) watchClient(
	r *http.Request,
	rec *record.Request,
	upCtx context.Context,
	cancelUp context.CancelCauseFunc,
	copyDone <-chan struct{},
) {
	select {
	case <-copyDone:
	case <-upCtx.Done():
	case <-h.srv.shutdown:
		cancelUp(fault.ErrProxyShutdown)
	case <-r.Context().Done():
		// Shutdown and a client hangup both cancel the request context, and
		// only one of them is the client's doing.
		if h.srv.shuttingDown.Load() {
			cancelUp(fault.ErrProxyShutdown)
			return
		}
		rec.SetClientGone(h.srv.now())
		cancelUp(fault.ErrClientGone)
	}
}

func (h *routeHandler) proxy(
	cw *countingWriter,
	r *http.Request,
	rec *record.Request,
	upCtx context.Context,
	cancelUp context.CancelCauseFunc,
) {
	route := h.target.route

	body, rest, err := h.readRequestBody(r, rec)
	if err != nil {
		f := fault.FromRequestBodyRead(err)
		rec.SetFault(f)
		h.writeProxyError(cw, rec, http.StatusBadRequest, f)
		return
	}
	if rec.Chat {
		peekPayload(rec, body)
		// Announced only now: before the peek there is no model, stream flag or
		// message count to show, and a bare path is not worth a line.
		h.srv.log.Startup(h.srv.log.Renderer().RenderStart(rec.Snapshot()))
	}

	outURL := upstreamURL(h.target.base, route.Name, r.URL)
	rec.UpstreamURL = outURL.String()

	for attempt := 1; ; attempt++ {
		a := rec.NewAttempt(h.srv.now())

		resp, f := h.attempt(r, rec, a, outURL, body, rest, upCtx)
		if f != nil {
			a.Set(func(at *record.Attempt) { at.Fault = f })
			// Nothing has been written downstream yet, so a retry is still
			// possible here and nowhere later.
			d := shouldRetryConnect(route.Retry, attempt, rec.BodyReplayable, f.Retryable)
			if d.Retry {
				recordWait(a, d)
				h.srv.log.Startup(h.srv.log.Renderer().RenderRetry(rec.Snapshot(), attemptView(rec), route.Retry.MaxAttempts, d.Wait))
				if !sleepCtx(r.Context(), d.Wait) {
					rec.SetClientGone(h.srv.now())
					rec.SetFault(clientLeftDuringWait())
					return
				}
				continue
			}
			if d.Refused != "" {
				rec.Warn(f.Kind, "not retried: "+d.Refused)
			}
			rec.SetFault(f)
			h.writeProxyError(cw, rec, http.StatusBadGateway, f)
			return
		}

		a.Set(func(at *record.Attempt) {
			at.Status = resp.StatusCode
			at.RespHeaders = resp.Header.Clone()
			at.TTFB = h.srv.now().Sub(at.Start)
			at.RateLimit = ratelimit.Parse(resp.Header, h.srv.now(), ratelimit.ServerDate(resp.Header))
		})

		d := shouldRetryStatus(route.Retry, attempt, resp.StatusCode, rec.BodyReplayable,
			attemptView(rec).RateLimit)
		if d.Retry || d.Refused != "" {
			// Peek at the body so it can be reported verbatim either way. The
			// peek is capped, so what is peeked is NOT what gets forwarded —
			// see below.
			peeked := peekErrorBody(resp, maxErrorBody)
			if d.Retry {
				// Retrying: drain the rest and close, so the connection stays
				// reusable. Dropping it costs a fresh TLS handshake on the
				// retry, which then pollutes the very timings being measured.
				drainAndClose(resp)
				a.Set(func(at *record.Attempt) {
					at.ErrorBody = decodeForDisplay(peeked, resp.Header.Get("Content-Encoding"))
				})
				recordWait(a, d)
				h.srv.log.Startup(h.srv.log.Renderer().RenderRetry(rec.Snapshot(), attemptView(rec), route.Retry.MaxAttempts, d.Wait))
				if !sleepCtx(r.Context(), d.Wait) {
					rec.SetClientGone(h.srv.now())
					rec.SetFault(clientLeftDuringWait())
					return
				}
				continue
			}
			// Not retrying: forward the response in full. Writing only the
			// peeked prefix would hand the client a truncated body under the
			// upstream's own Content-Length — this proxy's contract is to pass
			// bytes through unaltered, and a 100 kB error page from a CDN is
			// entirely ordinary on exactly these statuses.
			rec.Warn(refusalKind(resp.StatusCode), "not retried: "+d.Refused)
			h.deliver(cw, rec, resp, a, upCtx, cancelUp, peeked)
			return
		}

		h.deliver(cw, rec, resp, a, upCtx, cancelUp, nil)
		return
	}
}

// attempt performs one upstream round trip.
func (h *routeHandler) attempt(
	r *http.Request,
	rec *record.Request,
	a *record.Attempt,
	outURL *url.URL,
	body []byte,
	rest io.Reader,
	upCtx context.Context,
) (*http.Response, *fault.Fault) {
	ctx := upCtx
	if total := config.Dur(h.target.route.Timeouts.Total); total > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(upCtx, total)
		defer cancel()
	}
	ctx, attemptSpan := obs.StartAttemptSpan(ctx, a.N)
	defer attemptSpan.End()

	ctx = conntrace.WithTrace(ctx, a, h.srv.now, h.srv.cfg.Log.FullTrace)

	// A slow first token produces no output at all otherwise: no headers, no
	// body, nothing on the console for minutes. This says the wait is still in
	// progress, so the operator can see the proxy has not silently stalled.
	stopHeartbeat := h.startWaitHeartbeat(ctx, rec, a.N)
	defer stopHeartbeat()

	req, err := http.NewRequestWithContext(ctx, r.Method, outURL.String(), nil)
	if err != nil {
		return nil, fault.New(fault.SideProxy, fault.KindProxyInternal, "build-request", err)
	}

	if rest != nil {
		// Over the buffering cap: forward the prefix we peeked plus the rest of
		// the client's stream, keeping the length the client declared.
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), rest))
		req.ContentLength = r.ContentLength
	} else {
		// A fresh reader per attempt, with an explicit ContentLength. Leaving
		// the length unset makes net/http report -1 and send the request
		// chunked, changing the wire form from what the client actually sent —
		// which in a tool for observing the wire would be its own kind of lie.
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	req.Header = copyHeaders(r.Header)
	req.Host = ""
	rec.SetKeySource(applyAuth(req.Header, h.target.route, h.target.apiKey).String())
	applyExtraHeaders(req.Header, h.target.route)

	resp, err := h.target.client.Do(req)
	if err != nil {
		f := fault.FromRoundTrip(err, upCtx)
		obs.SetAttemptOutcome(attemptSpan, 0, f)
		return nil, f
	}
	obs.SetAttemptOutcome(attemptSpan, resp.StatusCode, nil)
	return resp, nil
}

// startWaitHeartbeat reports, repeatedly, that the upstream has not answered
// yet. It is reporting only — it never ends the wait.
func (h *routeHandler) startWaitHeartbeat(ctx context.Context, rec *record.Request, attemptN int) func() {
	every := config.Dur(h.target.route.Timeouts.GapWarn)
	if every <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	start := h.srv.now()
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				waited := roundForHumans(h.srv.now().Sub(start))
				h.srv.log.Slog().WarnContext(ctx,
					"still waiting for the upstream's first byte",
					"route", rec.Route, "request", rec.ID,
					"attempt", attemptN, "waited", waited.String())
				// One line in the report, updated, rather than one per tick.
				rec.WarnUpdate(kindSlowFirstByte, fmt.Sprintf(
					"waited %s for the upstream's first byte", waited))
				if h.srv.onGapWarn != nil {
					h.srv.onGapWarn()
				}
			}
		}
	}()
	return stop
}

// deliver commits the response to the client and streams the body.
func (h *routeHandler) deliver(
	cw *countingWriter,
	rec *record.Request,
	resp *http.Response,
	a *record.Attempt,
	upCtx context.Context,
	cancelUp context.CancelCauseFunc,
	prefix []byte,
) {
	defer resp.Body.Close()

	streaming := isEventStream(resp.Header)
	analyzer := analyze.New(h.srv.now)
	analyzer.SetStreaming(streaming)
	sink, cleanup := h.analysisSink(resp, analyzer)
	defer cleanup()

	h.commit(cw, rec, resp, upCtx)

	// Bytes already peeked for the report are put back in front, so the client
	// still receives the response byte for byte.
	var src io.Reader = resp.Body
	if len(prefix) > 0 {
		src = io.MultiReader(bytes.NewReader(prefix), resp.Body)
	}

	f := copyBody(cw, src, copyParams{
		rec:           rec,
		analyzer:      analyzer,
		sink:          sink,
		flush:         cleanup,
		upCtx:         upCtx,
		cancelUp:      cancelUp,
		streamIdle:    h.target.route.Timeouts.StreamIdle.D(),
		gapWarn:       h.target.route.Timeouts.GapWarn.D(),
		clientWrite:   h.target.route.Timeouts.ClientWrite.D(),
		streaming:     streaming,
		contentLength: resp.ContentLength,
		bodyExpected:  bodyExpected(rec.Method, resp.StatusCode),
		expectDone:    h.target.route.ExpectDone,
		now:           h.srv.now,
		onGapWarn: func(idle time.Duration) {
			rounded := roundForHumans(idle)
			// Live on the console every gap_warn, so a hang can be watched as
			// it happens...
			h.srv.log.Slog().WarnContext(context.Background(),
				"no data from the upstream, still waiting",
				"route", rec.Route, "request", rec.ID, "idle", rounded.String())
			// ...but a single line in the report saying how long it got to.
			rec.WarnUpdate(kindSlowChunk, fmt.Sprintf(
				"no data from the upstream for %s while the stream was open", rounded))
			if h.srv.onGapWarn != nil {
				h.srv.onGapWarn()
			}
		},
	})

	cleanup()
	rec.Stream = analyzer.Close()

	// An error response was streamed straight through rather than buffered, so
	// the retained copy is the only way the report can quote it verbatim.
	a.Set(func(at *record.Attempt) {
		if resp.StatusCode >= 400 && len(at.ErrorBody) == 0 {
			at.ErrorBody = analyzer.Captured()
		}
	})
	h.postmortem(rec, resp.StatusCode, a, f, streaming)
}

// commit writes the response head downstream. Once this returns, the retry
// window is closed permanently.
func (h *routeHandler) commit(cw *countingWriter, rec *record.Request, resp *http.Response, traceCtx context.Context) {
	prepareResponseHeaders(cw.Header(), resp.Header, resp.Uncompressed)
	cw.Header().Set("X-LLM-Proxy-Request-Id", rec.ID)
	if id := obs.TraceID(traceCtx); id != "" {
		cw.Header().Set("X-LLM-Proxy-Trace-Id", id)
	}
	// Echoed back so a harness that stamps its own id keeps it on the response.
	if rec.ClientRequestID != "" {
		cw.Header().Set("X-Request-Id", rec.ClientRequestID)
	}

	cw.WriteHeader(resp.StatusCode)
	rec.MarkHeadersSent(resp.StatusCode, h.srv.now())

	// Flush the head immediately so the client's SSE parser starts before the
	// first token arrives rather than after it.
	//
	// A failure here means the ResponseWriter chain cannot flush, so the proxy
	// is silently buffering and every timing it goes on to report is fiction.
	// That is the proxy's own bug and has to be loud, not swallowed.
	if err := http.NewResponseController(cw).Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			rec.SetFault(fault.New(fault.SideProxy, fault.KindProxyConfig, "flush", err).
				WithDetail("the ResponseWriter cannot flush, so responses are being buffered").
				WithVerdict("llm-proxy bug: a ResponseWriter wrapper is missing Unwrap(). " +
					"Timings from this request are not trustworthy."))
		}
		h.srv.log.Slog().WarnContext(context.Background(),
			"flushing the response head failed", "request", rec.ID, "error", err)
	}
}

// analysisSink returns where a copy of the upstream bytes should go so the
// analyzer sees something it can parse.
//
// This matters more than it looks. Python's httpx and requests — and therefore
// the OpenAI SDK — send Accept-Encoding: gzip by default. The proxy forwards
// that untouched and passes the compressed bytes through verbatim, so without
// decoding a copy here the analyzer would see gzip, find no finish_reason and
// no [DONE], and report a truncated stream on every healthy request from the
// most likely client.
func (h *routeHandler) analysisSink(resp *http.Response, analyzer *analyze.Analyzer) (io.Writer, func()) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if resp.Uncompressed || enc == "" || enc == "identity" {
		return analyzer, func() {}
	}

	if enc != "gzip" {
		// Anything we cannot decode: forward it, and make no claims about it.
		// Silence from an unreadable body is not evidence of truncation.
		analyzer.Unavailable("content-encoding: " + enc)
		return io.Discard, func() {}
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		zr, err := gzip.NewReader(pr)
		if err != nil {
			analyzer.Unavailable("gzip body could not be decoded")
			_, _ = io.Copy(io.Discard, pr)
			return
		}
		if _, err := io.Copy(writerFunc(analyzer.Write), zr); err != nil {
			analyzer.Unavailable("gzip body ended early")
		}
		_ = zr.Close()
		_, _ = io.Copy(io.Discard, pr)
	}()

	var closed bool
	return pw, func() {
		if closed {
			return
		}
		closed = true
		_ = pw.Close()
		<-done
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// postmortem draws the conclusion once the body is done.
func (h *routeHandler) postmortem(rec *record.Request, status int, a *record.Attempt, f *fault.Fault, streaming bool) {
	errBody := a.ErrorBodyCopy()
	pm := rec.Stream

	// An upstream error reported after the client had already gone is a
	// consequence of that departure, not an independent vendor failure. Left
	// alone it would blame the vendor for whatever its socket did in response
	// to a cancelled read.
	if f != nil && f.Side == fault.SideUpstream && rec.ClientGone() {
		f = fault.AsInduced(f)
	}

	// HTTP 200 followed by an error frame: the status line already said
	// everything was fine, so this is easy to miss entirely.
	//
	// It also outranks a truncation verdict. A stream that carried an explicit
	// vendor error will usually *also* look incomplete, and "the vendor said it
	// crashed" is a strictly better answer than "the stream stopped early".
	if streaming && status < 400 && pm != nil && len(pm.StreamError) > 0 {
		if f == nil || f.Side == fault.SideUpstream {
			f = fault.New(fault.SideUpstream, fault.KindErrorInStream, "read", nil).
				WithDetail("the upstream answered %d and then reported an error mid-stream: %s",
					status, oneLine(string(pm.StreamError)))
		}
	}

	rec.SetFault(f)

	if status >= 400 && rec.Fault() == nil {
		env := oaierr.Parse(errBody)
		switch {
		case status == http.StatusTooManyRequests:
			rec.SetFault(statusFault(fault.KindRateLimited, status, env))
		default:
			if which, ok := h.srv.ctxMatchers.Match(env, errBody); ok {
				fl := statusFault(fault.KindContextLength, status, env)
				// Naming the pattern that fired is what makes a false positive
				// diagnosable instead of mysterious.
				fl.Detail += "\nmatched " + which
				rec.SetFault(fl)
			} else {
				rec.SetFault(statusFault(fault.KindHTTPStatus, status, env))
			}
		}
	}

	if rl := attemptView(rec).RateLimit; rl != nil {
		for _, b := range rl.Buckets {
			if b.Exhausted() || !b.Low(0.05) {
				continue
			}
			rec.Warn(fault.KindRateLimited, fmt.Sprintf(
				"rate limit nearly exhausted: %d of %d %s remaining", *b.Remaining, *b.Limit, b.Name))
		}
	}

	if pm != nil {
		for _, reason := range pm.FinishReasons {
			switch reason {
			case "length":
				rec.Warn(fault.KindOutputTruncated,
					"finish_reason=length — the model hit the output token cap, so the answer is cut off")
			case "content_filter":
				rec.Warn(fault.KindContentFilter,
					"finish_reason=content_filter — the vendor stopped the response")
			}
		}
		// Token usage is absent from streams unless the client asks for it.
		// Saying so beats silently reporting nothing, because "no usage" reads
		// as a proxy failure otherwise.
		if streaming && pm.Usage == nil && rec.Chat && !rec.IncludeUsage && rec.Fault() == nil {
			rec.Warn(kindUsageMissing,
				"usage not reported — set stream_options.include_usage=true in your client to see token counts")
		}
	}

	// Every write may have succeeded into the kernel buffer while the client
	// was already gone, in which case the answer was never received. A client
	// that closed *after* getting a complete response is fine, so completeness
	// decides.
	if rec.Fault() == nil && rec.ClientGone() && !responseWasComplete(rec, streaming) {
		rec.SetFault(fault.New(fault.SideClient, fault.KindClientDisconnect, "write", nil).
			WithDetail("the upstream response completed, but the client had already closed the connection").
			WithVerdict("your LLM tool disconnected before the answer reached it. The vendor completed normally."))
	}
}

const (
	kindUsageMissing  fault.Kind = "usage_not_reported"
	kindSlowFirstByte fault.Kind = "slow_first_byte"
	kindSlowChunk     fault.Kind = "slow_chunk"
)

// responseWasComplete reports whether the client had the whole answer before it
// went away.
func responseWasComplete(rec *record.Request, streaming bool) bool {
	pm := rec.Stream
	if pm == nil {
		return false
	}
	snap := rec.Snapshot()
	if snap.BytesToClient < snap.BytesFromUpstream {
		return false
	}
	if streaming {
		return pm.Complete()
	}
	return len(pm.Trailing) == 0 && pm.ParseErrors == 0
}

func statusFault(kind fault.Kind, status int, env *oaierr.Envelope) *fault.Fault {
	f := fault.New(fault.SideUpstream, kind, "status", nil)
	f.HTTPStatus = status

	detail := fmt.Sprintf("the upstream answered %d", status)
	if env != nil {
		var bits []string
		if env.Type != "" {
			bits = append(bits, "type="+env.Type)
		}
		if env.Code != "" {
			bits = append(bits, "code="+env.Code)
		}
		if env.Param != "" {
			bits = append(bits, "param="+env.Param)
		}
		if env.RequestID != "" {
			bits = append(bits, "request_id="+env.RequestID)
		}
		if len(bits) > 0 {
			detail += "  " + strings.Join(bits, "  ")
		}
		if env.Message != "" {
			detail += "\n" + env.Message
		}
	}
	f.Detail = detail
	return f
}

// writeProxyError reports a failure that happened before any upstream response,
// in the shape an OpenAI client can parse. The diagnostic headers let the
// harness's own logs be correlated with this proxy's report.
func (h *routeHandler) writeProxyError(cw *countingWriter, rec *record.Request, status int, f *fault.Fault) {
	if rec.HeadersSent() {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "llm-proxy: " + f.Detail,
			"type":    "upstream_error",
			"code":    string(f.Kind),
			"param":   nil,
		},
	})

	cw.Header().Set("Content-Type", "application/json")
	cw.Header().Set("X-LLM-Proxy-Request-Id", rec.ID)
	cw.Header().Set("X-LLM-Proxy-Fault-Side", f.Side.String())
	cw.Header().Set("X-LLM-Proxy-Fault-Kind", string(f.Kind))
	cw.WriteHeader(status)
	rec.MarkHeadersSent(status, h.srv.now())
	_, _ = cw.Write(body)
}

// readRequestBody buffers the request so a retry can replay it.
//
// Over the cap the request still goes through — transparency wins — but retry
// becomes unavailable and the console says so. A silent behaviour change would
// be worse than the limit itself.
func (h *routeHandler) readRequestBody(r *http.Request, rec *record.Request) ([]byte, io.Reader, error) {
	limit := h.target.route.MaxRequestBody.Int64()
	if limit <= 0 {
		limit = 8 << 20
	}

	buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}

	if int64(len(buf)) > limit {
		// Deliberately NOT read into memory: reading the rest here would make
		// the cap bound nothing at all, and a multi-gigabyte upload would be
		// buffered in full. The remainder is streamed straight through, at the
		// cost of not being able to replay it.
		rec.BodyReplayable = false
		rec.Warn(fault.KindBodyTooLarge, fmt.Sprintf(
			"request body exceeds max_request_body (%s) — forwarded anyway, streamed rather than "+
				"buffered, so retry is unavailable", h.target.route.MaxRequestBody))
		rec.ReqBodyBytes = r.ContentLength
		return buf, r.Body, nil
	}

	rec.ReqBodyBytes = int64(len(buf))
	if h.srv.cfg.Log.FullTrace {
		rec.ReqBody = buf
	}
	return buf, nil, nil
}

// peekPayload reads the facts worth reporting out of the client's body. The
// bytes themselves are forwarded unmodified; nothing here is ever written back.
func peekPayload(rec *record.Request, body []byte) {
	var p struct {
		Model         string            `json:"model"`
		Stream        bool              `json:"stream"`
		MaxTokens     *int64            `json:"max_tokens"`
		MaxCompTokens *int64            `json:"max_completion_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		Messages      []json.RawMessage `json:"messages"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	// A body we cannot parse is not an error: it still gets forwarded, we
	// simply have less to report about it.
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}

	rec.Model = p.Model
	rec.Streaming = p.Stream
	rec.NMessages = len(p.Messages)
	rec.Temperature, rec.TopP = p.Temperature, p.TopP
	switch {
	case p.MaxTokens != nil:
		rec.MaxTokens = p.MaxTokens
	case p.MaxCompTokens != nil:
		rec.MaxTokens = p.MaxCompTokens
	}
	if p.StreamOptions != nil {
		rec.IncludeUsage = p.StreamOptions.IncludeUsage
	}
}

// peekErrorBody reads the first bytes of an error body for the report without
// consuming the rest, so the caller can still forward it intact.
func peekErrorBody(resp *http.Response, limit int64) []byte {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return body
}

// drainAndClose finishes with a response we are not forwarding, so its
// connection can go back to the pool.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// decodeForDisplay gunzips a retained body so the report quotes the vendor's
// words rather than compressed bytes.
//
// The proxy forwards the client's Accept-Encoding untouched, and Python's httpx
// — so the OpenAI SDK — asks for gzip by default. Without this the error body
// on a 429 renders as binary, which is the exact opposite of "no details are
// hidden from me".
func decodeForDisplay(body []byte, encoding string) []byte {
	if len(body) == 0 || !strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer zr.Close()
	// A capped peek is usually a partial gzip stream, so a read error still
	// leaves whatever decoded successfully.
	out, _ := io.ReadAll(io.LimitReader(zr, maxErrorBody))
	if len(out) == 0 {
		return body
	}
	return out
}

// bodyExpected reports whether a response is allowed to carry a body at all.
// HEAD and 204/304/1xx declare a Content-Length for a body they deliberately
// do not send.
func bodyExpected(method string, status int) bool {
	if method == http.MethodHead {
		return false
	}
	switch {
	case status >= 100 && status < 200:
		return false
	case status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	default:
		return true
	}
}

// refusalKind labels a refused retry by what actually happened, rather than
// calling every non-retried status a rate limit.
func refusalKind(status int) fault.Kind {
	if status == http.StatusTooManyRequests {
		return fault.KindRateLimited
	}
	return fault.KindHTTPStatus
}

func clientLeftDuringWait() *fault.Fault {
	return fault.New(fault.SideClient, fault.KindClientDisconnect, "retry-wait", nil).
		WithDetail("the client disconnected while the proxy was waiting to retry")
}

func isEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// sleepCtx waits, returning false if the client gave up first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " "))
}

// roundForHumans keeps short waits distinguishable from one another. Rounding
// everything to whole seconds makes consecutive progress reports print the same
// number, which reads as a stuck counter rather than a running one.
func roundForHumans(d time.Duration) time.Duration {
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
