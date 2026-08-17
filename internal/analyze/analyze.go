package analyze

import (
	"bytes"
	"encoding/json"
	"time"
)

// Usage is the token accounting a vendor reported. It is only ever what the
// API said; llm-proxy does not tokenize anything itself.
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	CachedInputTokens int64
	ReasoningTokens   int64
}

// Postmortem is everything learned about one response body.
type Postmortem struct {
	Events     int // complete SSE events
	DataEvents int // events carrying a data: payload
	Comments   int // keep-alive ping comments
	Bytes      int64

	DoneSeen bool // `data: [DONE]` observed

	// FinishReasons holds the terminal reason per choice index, in the order
	// choices first reported one.
	FinishReasons []string
	// FinishSeen reports whether any choice reached a terminal finish_reason.
	// This, not DoneSeen, is the primary evidence a stream completed: [DONE]
	// is an OpenAI convention that several compatible backends never send.
	FinishSeen bool

	FirstEventAt time.Time
	LastEventAt  time.Time
	// MaxGap is the longest silence between two events, and MaxGapAfterEvent
	// says where it happened — the pair that turns "it felt slow" into a fact.
	MaxGap           time.Duration
	MaxGapAfterEvent int

	ResponseID    string
	ResponseModel string
	Usage         *Usage

	// StreamError holds a vendor error delivered inside an otherwise
	// successful stream: HTTP 200, then `data: {"error": ...}`. Easy to miss,
	// and a direct instance of "the API just stopped working".
	StreamError []byte

	// Trailing is the unterminated tail at EOF: proof of a mid-frame cut.
	Trailing []byte
	// LastEvent keeps the final complete event for the fault report.
	LastEvent []byte

	ParseErrors int

	// AnalysisUnavailable explains why no analysis was possible, e.g. a
	// content encoding we cannot decode. When it is set, the absence of
	// FinishSeen or DoneSeen proves nothing and must never be reported as
	// truncation.
	AnalysisUnavailable string
}

// Captured returns the retained copy of the body, capped, for reporting an
// error response verbatim.
func (a *Analyzer) Captured() []byte { return a.captured }

// Complete reports whether the evidence shows a finished answer.
func (p *Postmortem) Complete() bool { return p.DoneSeen || p.FinishSeen }

// Analyzer consumes a copy of a response body and produces a Postmortem.
// It is written to from the copy loop only, so it needs no locking; the
// renderer reads it after Close.
type Analyzer struct {
	now     func() time.Time
	scanner Scanner
	pm      Postmortem

	// streaming selects SSE framing. A plain JSON body has no event
	// terminators, so running it through the frame scanner would leave the
	// entire body pending and look exactly like a response cut mid-frame.
	streaming bool
	buf       []byte

	// captured keeps the first captureLimit bytes the analyzer was fed, so an
	// error body can be reported verbatim even though it was streamed straight
	// through to the client rather than buffered. For a compressed response
	// these are the decoded bytes, which is what a reader wants to see.
	captured []byte

	choiceFinish map[int]string
	closed       bool
}

// New returns an Analyzer. now is injectable so gap measurements are testable
// without sleeping.
func New(now func() time.Time) *Analyzer {
	if now == nil {
		now = time.Now
	}
	return &Analyzer{now: now, streaming: true, choiceFinish: make(map[int]string)}
}

// SetStreaming selects between SSE framing and whole-body JSON analysis.
func (a *Analyzer) SetStreaming(v bool) { a.streaming = v }

// Unavailable marks the body as un-analysable, e.g. brotli-encoded. The proxy
// still forwards every byte; it just cannot draw conclusions about them.
func (a *Analyzer) Unavailable(reason string) { a.pm.AnalysisUnavailable = reason }

// Write feeds bytes read from the upstream. It never returns an error and never
// blocks: nothing here may interfere with forwarding the stream.
// captureLimit bounds the retained copy so a vendor streaming an HTML error
// page cannot pin memory.
const captureLimit = 64 << 10

func (a *Analyzer) Write(p []byte) (int, error) {
	n := len(p)
	a.pm.Bytes += int64(n)

	if room := captureLimit - len(a.captured); room > 0 {
		a.captured = append(a.captured, p[:min(room, len(p))]...)
	}

	if a.pm.AnalysisUnavailable != "" {
		return n, nil
	}

	if !a.streaming {
		// Buffered and parsed once at Close: a JSON body only means anything
		// whole.
		a.buf = append(a.buf, p...)
		return n, nil
	}

	for _, block := range a.scanner.Write(p) {
		a.feedEvent(block)
	}
	return n, nil
}

func (a *Analyzer) feedEvent(block []byte) {
	now := a.now()
	if a.pm.Events == 0 {
		a.pm.FirstEventAt = now
	} else if gap := now.Sub(a.pm.LastEventAt); gap > a.pm.MaxGap {
		a.pm.MaxGap = gap
		a.pm.MaxGapAfterEvent = a.pm.Events
	}
	a.pm.LastEventAt = now
	a.pm.Events++
	a.pm.LastEvent = append(a.pm.LastEvent[:0], block...)

	data := DataOf(block)
	if data == nil {
		// A comment-only event is a keep-alive ping. Worth counting: a stream
		// consisting only of pings is stalled even though bytes keep arriving.
		a.pm.Comments++
		return
	}
	a.pm.DataEvents++

	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		a.pm.DoneSeen = true
		return
	}
	a.feedJSON(data)
}

// chunk covers both the streaming and non-streaming response shapes, plus the
// several error envelopes OpenAI-compatible vendors use.
type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		FinishReason *string         `json:"finish_reason"`
		Delta        json.RawMessage `json:"delta"`
		Message      json.RawMessage `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`

		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func (a *Analyzer) feedJSON(data []byte) {
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		// A frame we cannot parse is not fatal — vendors add fields, and some
		// send non-JSON keep-alives. Count it so a stream that is entirely
		// unparseable is visible.
		a.pm.ParseErrors++
		return
	}

	if len(c.Error) > 0 && !bytes.Equal(bytes.TrimSpace(c.Error), []byte("null")) {
		if a.pm.StreamError == nil {
			a.pm.StreamError = append([]byte(nil), c.Error...)
		}
	}
	if c.ID != "" {
		a.pm.ResponseID = c.ID
	}
	if c.Model != "" {
		a.pm.ResponseModel = c.Model
	}

	for _, ch := range c.Choices {
		if ch.FinishReason == nil || *ch.FinishReason == "" {
			continue
		}
		if _, seen := a.choiceFinish[ch.Index]; !seen {
			a.choiceFinish[ch.Index] = *ch.FinishReason
			a.pm.FinishReasons = append(a.pm.FinishReasons, *ch.FinishReason)
			a.pm.FinishSeen = true
		}
	}

	if c.Usage != nil {
		u := &Usage{
			InputTokens:  pick(c.Usage.PromptTokens, c.Usage.InputTokens),
			OutputTokens: pick(c.Usage.CompletionTokens, c.Usage.OutputTokens),
			TotalTokens:  c.Usage.TotalTokens,
		}
		if d := c.Usage.PromptTokensDetails; d != nil {
			u.CachedInputTokens = d.CachedTokens
		}
		if d := c.Usage.CompletionTokensDetails; d != nil {
			u.ReasoningTokens = d.ReasoningTokens
		}
		if u.TotalTokens == 0 {
			u.TotalTokens = u.InputTokens + u.OutputTokens
		}
		a.pm.Usage = u
	}
}

// pick prefers the legacy prompt/completion naming and falls back to the newer
// input/output naming; vendors are split between the two.
func pick(legacy, modern int64) int64 {
	if legacy != 0 {
		return legacy
	}
	return modern
}

// Close finishes the analysis and returns the Postmortem. It is safe to call
// more than once.
func (a *Analyzer) Close() *Postmortem {
	if a.closed {
		return &a.pm
	}
	a.closed = true

	if !a.streaming {
		if len(bytes.TrimSpace(a.buf)) > 0 {
			switch {
			case json.Valid(a.buf):
				a.feedJSON(a.buf)
			case looksLikeJSON(a.buf):
				// It began as a JSON document and does not parse, so it was
				// almost certainly cut short. Reporting the tail lets the
				// caller reach the same truncation conclusion it would for a
				// cut stream.
				a.pm.ParseErrors++
				a.pm.Trailing = append([]byte(nil), tail(a.buf, 64)...)
			default:
				// Not JSON at all — an HTML error page from a CDN, or plain
				// text. That is worth noticing, but it is emphatically not
				// evidence that the body was truncated, and treating it as
				// such would flag every gateway error page as a cut response.
				a.pm.ParseErrors++
			}
		}
		return &a.pm
	}

	if pending := a.scanner.Pending(); len(pending) > 0 {
		a.pm.Trailing = append([]byte(nil), pending...)
	}
	return &a.pm
}

// JSON analyses a complete non-streaming response body. Truncation matters
// here too: a JSON body cut mid-object is the same failure as a cut stream,
// and finish_reason=length is just as important on a non-streaming call.
func JSON(body []byte) *Postmortem {
	a := New(nil)
	pm := &a.pm
	pm.Bytes = int64(len(body))
	if len(bytes.TrimSpace(body)) == 0 {
		return pm
	}
	if !json.Valid(body) {
		pm.ParseErrors++
		// Only a document that began as JSON and failed to parse is evidence of
		// a cut; anything else is simply not JSON.
		if looksLikeJSON(body) {
			pm.Trailing = append([]byte(nil), tail(body, 64)...)
		}
		return pm
	}
	a.feedJSON(body)
	return pm
}

// looksLikeJSON reports whether the body opens as a JSON object or array.
func looksLikeJSON(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
