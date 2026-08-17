package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/peterhoneder/llm-proxy/internal/analyze"
	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// countingWriter wraps the downstream ResponseWriter.
//
// Unwrap is not optional. http.ResponseController walks the wrapper chain
// looking for it; without it every Flush returns http.ErrNotSupported, the
// proxy silently buffers the stream, and every latency number it prints becomes
// fiction. There is a test for exactly this.
type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// copyParams carries everything the copy loop needs.
type copyParams struct {
	rec      *record.Request
	analyzer *analyze.Analyzer

	// sink receives a copy of every byte read from the upstream. It is the
	// analyzer for an identity-encoded body, and a decoder feeding the
	// analyzer for a compressed one — so the analyzer is never fed twice and
	// never sees bytes it cannot parse.
	sink io.Writer

	// upCtx is the proxy-owned upstream context. Its cancellation cause is the
	// only trustworthy explanation for a context.Canceled from a read.
	upCtx context.Context
	// cancelUp cancels that context with a stamped reason.
	cancelUp context.CancelCauseFunc

	streamIdle  time.Duration
	gapWarn     time.Duration
	clientWrite time.Duration

	streaming bool
	// contentLength is the upstream's declared body length, or -1. A body that
	// stops short of it is truncated just as surely as a cut stream.
	contentLength int64
	// bodyExpected is false for HEAD, 204, 304 and 1xx, which declare a length
	// but send nothing.
	bodyExpected bool
	// expectDone is the route's expect_done setting, which decides how strict
	// the completion check is.
	expectDone string

	// onGapWarn fires once when a stall passes gapWarn but has not yet reached
	// streamIdle, so a hang can be watched while it happens.
	onGapWarn func(time.Duration)

	// flush drains any decoder feeding the analyzer. It must run before the
	// completion verdict is drawn: a gzip stream is decoded on another
	// goroutine, and judging the analyzer before that finishes would find no
	// finish_reason and report a healthy response as truncated.
	flush func()

	now func() time.Time
}

// copyBody streams the upstream response to the client, attributing any failure
// to the side that caused it.
//
// The whole attribution model rests on the ordering here: read first, write
// second. An error from the read is the upstream's; an error from the write is
// the client's. There is no heuristic anywhere in this loop.
func copyBody(w http.ResponseWriter, body io.Reader, p copyParams) *fault.Fault {
	if p.now == nil {
		p.now = time.Now
	}
	if p.sink == nil {
		p.sink = p.analyzer
	}
	rc := http.NewResponseController(w)
	buf := getBuffer()
	defer putBuffer(buf)

	// The inter-chunk clock starts now, not when the request arrived. A ten
	// minute wait for the first token is the response-header budget's business;
	// letting it also eat the idle budget would fail the stream the instant the
	// body began.
	p.rec.TouchUpstream(p.now())

	stop := p.startWatchdog()
	defer stop()

	var writeDeadline time.Time

	for {
		n, rerr := body.Read(buf)
		now := p.now()

		if n > 0 {
			p.rec.AddRead(n, now)
			if p.sink != nil {
				_, _ = p.sink.Write(buf[:n])
			}

			// The deadline is extended rather than set per write: two syscalls
			// per chunk on a loop that runs hundreds of times a second is
			// waste, and one second of granularity is plenty for detecting a
			// client that has stopped reading entirely.
			if p.clientWrite > 0 && now.After(writeDeadline.Add(-time.Second)) {
				writeDeadline = now.Add(p.clientWrite)
				_ = rc.SetWriteDeadline(writeDeadline)
			}

			nw, werr := w.Write(buf[:n])
			p.rec.AddDelivered(nw, p.now())
			if werr != nil {
				// A write failure is terminal on HTTP/1.1: after a partial
				// write the connection is in an indeterminate state, so there
				// is nothing sensible to continue with.
				p.cancelUp(fault.ErrClientGone)
				return fault.FromClientWrite(werr)
			}
			if nw < n {
				p.cancelUp(fault.ErrClientGone)
				return fault.FromClientWrite(io.ErrShortWrite)
			}

			if ferr := rc.Flush(); ferr != nil {
				p.cancelUp(fault.ErrHandlerReturn)
				return fault.FromClientWrite(ferr)
			}
		}

		if rerr != nil {
			// Clear the deadline before returning: an expired one would make
			// net/http's own final write fail and manufacture a fault.
			_ = rc.SetWriteDeadline(time.Time{})
			if errors.Is(rerr, io.EOF) {
				return fault.FromCleanEOF(p.readState())
			}
			return fault.FromRead(rerr, p.upCtx, p.readState())
		}
	}
}

// readState assembles what the classifier needs to tell one io.EOF from
// another.
func (p copyParams) readState() fault.ReadState {
	if p.flush != nil {
		p.flush()
	}
	pm := p.analyzer.Close()
	snap := p.rec.Snapshot()

	st := fault.ReadState{
		BytesRead:     snap.BytesFromUpstream,
		Events:        pm.DataEvents,
		DoneSeen:      pm.DoneSeen,
		FinishSeen:    pm.FinishSeen,
		PartialBytes:  len(pm.Trailing),
		StreamIdle:    p.streamIdle,
		Streaming:     p.streaming,
		ContentLength: p.contentLength,
		BodyExpected:  p.bodyExpected,
		ExpectDone:    p.expectDone,
	}
	if last := snap.Attempts; len(last) > 0 {
		c := &last[len(last)-1].Conn
		st.Reused, st.IdleTime = c.Reused, c.IdleTime
	}
	// A body we could not decode proves nothing about completion, so the
	// classifier must not be allowed to infer truncation from silence.
	if pm.AnalysisUnavailable != "" {
		st.FinishSeen = true
		st.DoneSeen = true
	}
	return st
}

// startWatchdog reports a silent upstream, and — only if configured to — ends
// the wait.
//
// Two separate jobs. gapWarn reports that nothing has arrived yet, repeatedly,
// so a ten-minute wait can be watched rather than guessed at. streamIdle is a
// deadline, and defaults to zero because a proxy that gives up before the
// vendor does destroys the observation it exists to make.
//
// It is a single ticker comparing an atomic timestamp, not a timer reset per
// chunk: Timer.Reset gives no guarantee the func is not already running, and
// paying a timer operation per chunk on the hot path is pure waste.
//
// The watchdog only ever cancels the context with a reason. It never writes the
// fault itself, so a real error arriving a microsecond later cannot be raced
// out of the record.
func (p copyParams) startWatchdog() func() {
	if p.streamIdle <= 0 && p.gapWarn <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	interval := tickInterval(p.streamIdle, p.gapWarn)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		lastMark := p.rec.LastUpstreamByte().UnixNano()
		nextWarn := p.gapWarn

		for {
			select {
			case <-done:
				return
			case <-p.upCtx.Done():
				return
			case <-ticker.C:
				mark := p.rec.LastUpstreamByte()
				if n := mark.UnixNano(); n != lastMark {
					// Data arrived; start counting the silence again.
					lastMark, nextWarn = n, p.gapWarn
					continue
				}

				idle := p.now().Sub(mark)
				if p.streamIdle > 0 && idle >= p.streamIdle {
					p.cancelUp(fault.ErrUpstreamStall)
					return
				}
				if p.gapWarn > 0 && idle >= nextWarn && p.onGapWarn != nil {
					p.onGapWarn(idle)
					nextWarn += p.gapWarn
				}
			}
		}
	}()

	return stop
}

// tickInterval picks a poll period fine enough for whichever job is configured,
// without spinning.
func tickInterval(streamIdle, gapWarn time.Duration) time.Duration {
	candidates := []time.Duration{}
	if streamIdle > 0 {
		candidates = append(candidates, streamIdle/4)
	}
	if gapWarn > 0 {
		candidates = append(candidates, gapWarn/4)
	}
	interval := time.Second
	for _, c := range candidates {
		if c < interval {
			interval = c
		}
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	return interval
}

// A 32 KiB buffer pool: large enough that a busy stream is not syscall-bound,
// small enough that per-chunk timing stays meaningful.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

func getBuffer() []byte  { return *bufPool.Get().(*[]byte) }
func putBuffer(b []byte) { bufPool.Put(&b) }
