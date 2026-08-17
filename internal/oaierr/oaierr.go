// Package oaierr decodes the error bodies OpenAI-compatible vendors return.
//
// The proxy always forwards the body byte for byte; this package exists only so
// the console can show `code=context_length_exceeded` next to the raw payload
// instead of leaving the reader to find it. Decoding is best-effort by design:
// an unrecognised shape is reported as unrecognised, never guessed at.
package oaierr

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Envelope is a vendor error reduced to the fields worth showing.
type Envelope struct {
	Message string
	Type    string
	Code    string
	Param   string

	// RequestID is whatever identifier the vendor attached, which is what
	// their support will ask for.
	RequestID string
}

// Empty reports whether nothing useful was decoded.
func (e *Envelope) Empty() bool {
	return e == nil || (e.Message == "" && e.Type == "" && e.Code == "" && e.RequestID == "")
}

// code accepts both a string and a number, because vendors disagree.
type code struct{ s string }

func (c *code) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.s = s
		return nil
	}
	c.s = string(b)
	return nil
}

type inner struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      code   `json:"code"`
	Param     string `json:"param"`
	RequestID string `json:"request_id"`
}

type outer struct {
	// {"error": {...}} — the OpenAI shape.
	Error json.RawMessage `json:"error"`
	// {"message": ..., "type": ...} — flat, used by Mistral among others.
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    code   `json:"code"`
	Param   string `json:"param"`
	// {"detail": ...} — FastAPI-based servers such as vLLM.
	Detail    json.RawMessage `json:"detail"`
	RequestID string          `json:"request_id"`
	// Some gateways return {"object":"error","message":...}.
	Object string `json:"object"`
}

// Parse decodes a vendor error body. It returns nil when the body is not JSON
// or carries nothing error-shaped — an HTML 502 page from an intermediate
// proxy, for instance, which is itself a useful thing to be able to tell.
func Parse(body []byte) *Envelope {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	var o outer
	if err := json.Unmarshal(body, &o); err != nil {
		return nil
	}

	e := &Envelope{RequestID: o.RequestID}

	// {"error": {...}}
	if len(o.Error) > 0 && string(o.Error) != "null" {
		var in inner
		if err := json.Unmarshal(o.Error, &in); err == nil {
			e.Message, e.Type, e.Code, e.Param = in.Message, in.Type, in.Code.s, in.Param
			if in.RequestID != "" {
				e.RequestID = in.RequestID
			}
		} else {
			// {"error": "just a string"}
			var s string
			if err := json.Unmarshal(o.Error, &s); err == nil {
				e.Message = s
			}
		}
	}

	// Flat shape, or fields the nested one left blank.
	if e.Message == "" {
		e.Message = o.Message
	}
	if e.Type == "" {
		e.Type = o.Type
	}
	if e.Code == "" {
		e.Code = o.Code.s
	}
	if e.Param == "" {
		e.Param = o.Param
	}

	// {"detail": ...}, which is either a string or a list of validation errors.
	if e.Message == "" && len(o.Detail) > 0 {
		var s string
		if err := json.Unmarshal(o.Detail, &s); err == nil {
			e.Message = s
		} else {
			e.Message = strings.TrimSpace(string(o.Detail))
		}
	}

	if e.Empty() {
		return nil
	}
	return e
}

// Matchers recognises a context-window overflow in a vendor's wording. The
// patterns are configuration rather than code because every vendor phrases it
// differently, and a false positive must be diagnosable — hence Match
// returning which pattern fired.
type Matchers struct {
	patterns []*regexp.Regexp
	sources  []string
}

// NewMatchers compiles the configured patterns case-insensitively.
func NewMatchers(patterns []string) (*Matchers, error) {
	m := &Matchers{}
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, err
		}
		m.patterns = append(m.patterns, re)
		m.sources = append(m.sources, p)
	}
	return m, nil
}

// Match reports whether the error looks like a context-window overflow, and
// returns the pattern that matched so the console can name it.
func (m *Matchers) Match(e *Envelope, body []byte) (string, bool) {
	if m == nil {
		return "", false
	}
	// An explicit code beats any amount of prose matching.
	if e != nil && e.Code != "" {
		if strings.EqualFold(e.Code, "context_length_exceeded") ||
			strings.EqualFold(e.Code, "string_above_max_length") {
			return "code=" + e.Code, true
		}
	}

	subject := ""
	if e != nil && e.Message != "" {
		subject = e.Message
	} else {
		subject = string(body)
	}
	for i, re := range m.patterns {
		if re.MatchString(subject) {
			return "/" + m.sources[i] + "/", true
		}
	}
	return "", false
}
