package analyze

import (
	"strings"
	"testing"
	"time"
)

const sampleStream = "data: {\"id\":\"chatcmpl-1\",\"model\":\"m-large\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Sure\"}}]}\n\n" +
	": ping\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\", here\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1042,\"completion_tokens\":387,\"total_tokens\":1429}}\n\n" +
	"data: [DONE]\n\n"

func analyzeString(s string) *Postmortem {
	a := New(nil)
	_, _ = a.Write([]byte(s))
	return a.Close()
}

func TestCleanStream(t *testing.T) {
	t.Parallel()
	pm := analyzeString(sampleStream)

	if !pm.DoneSeen {
		t.Error("DoneSeen = false, want true")
	}
	if !pm.FinishSeen {
		t.Error("FinishSeen = false, want true")
	}
	if !pm.Complete() {
		t.Error("Complete() = false on a clean stream")
	}
	if got, want := pm.FinishReasons, []string{"stop"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("FinishReasons = %v, want %v", got, want)
	}
	if pm.ResponseID != "chatcmpl-1" || pm.ResponseModel != "m-large" {
		t.Errorf("id/model = %q/%q, want chatcmpl-1/m-large", pm.ResponseID, pm.ResponseModel)
	}
	if pm.Usage == nil {
		t.Fatal("Usage was not captured")
	}
	if pm.Usage.InputTokens != 1042 || pm.Usage.OutputTokens != 387 || pm.Usage.TotalTokens != 1429 {
		t.Errorf("Usage = %+v, want 1042/387/1429", *pm.Usage)
	}
	if pm.Comments != 1 {
		t.Errorf("Comments = %d, want 1 (the `: ping` keep-alive)", pm.Comments)
	}
	if len(pm.Trailing) != 0 {
		t.Errorf("Trailing = %q, want empty on a cleanly framed stream", pm.Trailing)
	}
	if pm.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0", pm.ParseErrors)
	}
}

// A TCP read boundary is not an event boundary. Splitting the same fixture at
// every possible offset is the cheapest way to prove the scanner never depends
// on where the chunks happen to land.
func TestSplitAtEveryOffsetYieldsIdenticalResult(t *testing.T) {
	t.Parallel()
	want := analyzeString(sampleStream)

	for i := 1; i < len(sampleStream); i++ {
		a := New(nil)
		_, _ = a.Write([]byte(sampleStream[:i]))
		_, _ = a.Write([]byte(sampleStream[i:]))
		got := a.Close()

		if got.DoneSeen != want.DoneSeen || got.FinishSeen != want.FinishSeen ||
			got.DataEvents != want.DataEvents || got.Events != want.Events ||
			got.ResponseID != want.ResponseID || len(got.Trailing) != 0 {
			t.Fatalf("split at byte %d changed the result:\n got %+v\nwant %+v", i,
				summary(got), summary(want))
		}
		if got.Usage == nil || *got.Usage != *want.Usage {
			t.Fatalf("split at byte %d lost the usage object", i)
		}
	}
}

func TestSplitOneByteAtATime(t *testing.T) {
	t.Parallel()
	a := New(nil)
	for i := 0; i < len(sampleStream); i++ {
		_, _ = a.Write([]byte{sampleStream[i]})
	}
	pm := a.Close()
	if !pm.DoneSeen || !pm.FinishSeen || pm.Usage == nil {
		t.Errorf("byte-at-a-time feed lost information: %+v", summary(pm))
	}
}

func TestCRLFFraming(t *testing.T) {
	t.Parallel()
	pm := analyzeString("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n")
	if !pm.DoneSeen || !pm.FinishSeen {
		t.Errorf("CRLF-framed stream not parsed: %+v", summary(pm))
	}
	if len(pm.Trailing) != 0 {
		t.Errorf("Trailing = %q, want empty", pm.Trailing)
	}
}

func TestMultiLineDataIsJoined(t *testing.T) {
	t.Parallel()
	// The SSE spec joins repeated data: lines with a newline before the
	// payload is interpreted.
	pm := analyzeString("data: {\"choices\":[{\"index\":0,\ndata: \"finish_reason\":\"stop\"}]}\n\n")
	if !pm.FinishSeen {
		t.Errorf("multi-line data payload was not reassembled: %+v", summary(pm))
	}
}

// The whole point of keeping the tail: it is unambiguous proof of a mid-frame
// cut, unlike a missing [DONE].
func TestTrailingPartialFrameIsPreserved(t *testing.T) {
	t.Parallel()
	pm := analyzeString("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"id\":\"chatcmp")

	if got := string(pm.Trailing); got != `data: {"id":"chatcmp` {
		t.Errorf("Trailing = %q, want the unterminated fragment", got)
	}
	if pm.Complete() {
		t.Error("a stream cut mid-frame must not look complete")
	}
}

// Many OpenAI-compatible servers never send [DONE]. Treating that as
// truncation would flag every healthy request against them.
func TestFinishReasonWithoutDoneStillCounts(t *testing.T) {
	t.Parallel()
	pm := analyzeString("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
	if pm.DoneSeen {
		t.Error("DoneSeen should be false here")
	}
	if !pm.FinishSeen || !pm.Complete() {
		t.Error("a terminal finish_reason must be accepted as completion evidence")
	}
}

func TestFinishReasonLength(t *testing.T) {
	t.Parallel()
	pm := analyzeString("data: {\"choices\":[{\"index\":0,\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	if len(pm.FinishReasons) != 1 || pm.FinishReasons[0] != "length" {
		t.Errorf("FinishReasons = %v, want [length]", pm.FinishReasons)
	}
}

func TestMultipleChoicesEachReportOnce(t *testing.T) {
	t.Parallel()
	pm := analyzeString(
		"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"},{\"index\":1,\"finish_reason\":\"length\"}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
	if len(pm.FinishReasons) != 2 {
		t.Errorf("FinishReasons = %v, want one entry per choice", pm.FinishReasons)
	}
}

// HTTP 200 followed by an error frame is a real failure mode and trivially
// missed: the status line already said everything was fine.
func TestErrorInsideStream(t *testing.T) {
	t.Parallel()
	pm := analyzeString(
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"upstream model crashed\",\"type\":\"server_error\"}}\n\n")

	if pm.StreamError == nil {
		t.Fatal("a mid-stream error frame was not detected")
	}
	if !strings.Contains(string(pm.StreamError), "upstream model crashed") {
		t.Errorf("StreamError = %q, want the vendor's message verbatim", pm.StreamError)
	}
	if pm.Complete() {
		t.Error("a stream that errored must not look complete")
	}
}

func TestNullErrorFieldIsNotAnError(t *testing.T) {
	t.Parallel()
	pm := analyzeString("data: {\"error\":null,\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
	if pm.StreamError != nil {
		t.Errorf("StreamError = %q, want nil for an explicit null", pm.StreamError)
	}
}

func TestUnparseableFrameIsCountedNotFatal(t *testing.T) {
	t.Parallel()
	pm := analyzeString(
		"data: this is not json\n\n" +
			"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
	if pm.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1", pm.ParseErrors)
	}
	if !pm.FinishSeen {
		t.Error("one bad frame must not derail the rest of the analysis")
	}
}

func TestGapMeasurement(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	steps := []time.Duration{0, 100 * time.Millisecond, 9 * time.Second, 50 * time.Millisecond}
	i := 0
	a := New(func() time.Time {
		t := base.Add(steps[min(i, len(steps)-1)])
		if i < len(steps)-1 {
			base = t
			i++
		}
		return t
	})

	for range steps {
		_, _ = a.Write([]byte("data: {\"choices\":[]}\n\n"))
	}
	pm := a.Close()

	if pm.MaxGap < 9*time.Second {
		t.Errorf("MaxGap = %v, want at least 9s", pm.MaxGap)
	}
	if pm.MaxGapAfterEvent == 0 {
		t.Error("MaxGapAfterEvent should say which event preceded the stall")
	}
}

// When the body cannot be decoded, silence proves nothing. Reporting
// truncation here would fire on every gzip-encoded response from a Python
// client, which sends Accept-Encoding: gzip by default.
func TestUnavailableAnalysisMakesNoClaims(t *testing.T) {
	t.Parallel()
	a := New(nil)
	a.Unavailable("content-encoding: br")
	_, _ = a.Write([]byte("\x1f\x8b\x08\x00 binary nonsense"))
	pm := a.Close()

	if pm.AnalysisUnavailable == "" {
		t.Error("AnalysisUnavailable must be preserved")
	}
	if pm.Events != 0 || pm.ParseErrors != 0 {
		t.Errorf("un-analysable bytes must not be parsed: %+v", summary(pm))
	}
	if pm.Bytes == 0 {
		t.Error("byte counting must continue even when parsing does not")
	}
}

func TestJSONNonStreaming(t *testing.T) {
	t.Parallel()
	pm := JSON([]byte(`{"id":"cmpl-9","model":"m-large",` +
		`"choices":[{"index":0,"finish_reason":"length","message":{"content":"cut off"}}],` +
		`"usage":{"prompt_tokens":3311,"completion_tokens":4096,"total_tokens":7407}}`))

	if !pm.FinishSeen || pm.FinishReasons[0] != "length" {
		t.Errorf("FinishReasons = %v, want [length]", pm.FinishReasons)
	}
	if pm.Usage == nil || pm.Usage.OutputTokens != 4096 {
		t.Errorf("Usage = %+v, want 4096 output tokens", pm.Usage)
	}
	if pm.ResponseID != "cmpl-9" {
		t.Errorf("ResponseID = %q, want cmpl-9", pm.ResponseID)
	}
}

func TestJSONTruncatedBodyLeavesEvidence(t *testing.T) {
	t.Parallel()
	pm := JSON([]byte(`{"id":"cmpl-9","choices":[{"index":0,"message":{"content":"cut o`))
	if len(pm.Trailing) == 0 {
		t.Error("a truncated JSON body must leave evidence for the fault report")
	}
	if pm.ParseErrors == 0 {
		t.Error("ParseErrors should record the failed parse")
	}
}

func TestJSONModernUsageNaming(t *testing.T) {
	t.Parallel()
	pm := JSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":20}}`))
	if pm.Usage == nil || pm.Usage.InputTokens != 10 || pm.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want the input/output naming to be understood", pm.Usage)
	}
	if pm.Usage.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want it derived when absent", pm.Usage.TotalTokens)
	}
}

func TestJSONEmptyBody(t *testing.T) {
	t.Parallel()
	if pm := JSON(nil); pm.ParseErrors != 0 || pm.Complete() {
		t.Errorf("an empty body should be inert, got %+v", summary(pm))
	}
}

func TestParseEventFields(t *testing.T) {
	t.Parallel()
	fields := ParseEvent([]byte("event: message\nid: 42\ndata: hello\n: a comment"))
	want := []struct{ name, value string }{
		{"event", "message"}, {"id", "42"}, {"data", "hello"}, {"", "a comment"},
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(fields), len(want), fields)
	}
	for i, w := range want {
		if fields[i].Name != w.name || string(fields[i].Value) != w.value {
			t.Errorf("field %d = %q/%q, want %q/%q", i, fields[i].Name, fields[i].Value, w.name, w.value)
		}
	}
}

func summary(p *Postmortem) map[string]any {
	return map[string]any{
		"events": p.Events, "data": p.DataEvents, "done": p.DoneSeen,
		"finish": p.FinishSeen, "trailing": string(p.Trailing), "parseErrors": p.ParseErrors,
	}
}

// An HTML error page from a CDN or gateway is not JSON, and is emphatically not
// evidence that the body was cut short. Treating "does not parse" as truncation
// would flag every gateway error page as a truncated response.
func TestNonJSONBodyIsNotTreatedAsTruncated(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"<html><body><h1>502 Bad Gateway</h1></body></html>",
		"Service Unavailable",
		strings.Repeat("E", 4096),
	} {
		pm := JSON([]byte(body))
		if len(pm.Trailing) != 0 {
			t.Errorf("JSON(%.20q...) left Trailing = %q, which reads as a mid-body cut",
				body, pm.Trailing)
		}
		if pm.ParseErrors == 0 {
			t.Errorf("JSON(%.20q...) should still record that it could not be parsed", body)
		}
	}
}

// A document that opens as JSON and then stops is a genuine cut.
func TestTruncatedJSONIsStillDetected(t *testing.T) {
	t.Parallel()
	pm := JSON([]byte(`{"id":"cmpl-9","choices":[{"index":0,"message":{"content":"cut o`))
	if len(pm.Trailing) == 0 {
		t.Error("a JSON document cut mid-object must leave evidence of the cut")
	}
}
