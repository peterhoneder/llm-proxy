// Package analyze extracts completion evidence from an LLM response: whether
// the answer actually finished, why it stopped, and how many tokens it used.
//
// It never re-serialises anything. The proxy writes upstream bytes downstream
// verbatim and feeds a copy here, so nothing in this package can alter what the
// client receives.
package analyze

import "bytes"

// Scanner splits a byte stream into Server-Sent Events.
//
// The framing matters more than it looks. A TCP read boundary is not an event
// boundary: one read can carry several events, and a single JSON payload is
// routinely split across two reads. Anything that assumes "one chunk is one
// event" will drop data under load and then report a truncated stream.
//
// Whatever remains unterminated when the stream ends is kept in Pending. That
// leftover is the strongest evidence of a mid-answer cut that exists, so it is
// deliberately preserved rather than discarded.
type Scanner struct {
	buf []byte
}

// Write feeds bytes and returns the complete events they completed. The
// returned slices alias an internal buffer that is reused, so callers must copy
// anything they intend to keep.
func (s *Scanner) Write(p []byte) [][]byte {
	s.buf = append(s.buf, p...)
	var events [][]byte

	for {
		idx, sep := findSeparator(s.buf)
		if idx < 0 {
			break
		}
		events = append(events, s.buf[:idx])
		s.buf = s.buf[idx+sep:]
	}

	// Compact so the buffer does not grow without bound across a long stream.
	if len(s.buf) == 0 && cap(s.buf) > 64<<10 {
		s.buf = nil
	}
	return events
}

// Pending returns the unterminated trailing bytes, if any.
func (s *Scanner) Pending() []byte { return s.buf }

// findSeparator locates the first SSE event terminator and reports its length.
// Both LF and CRLF framing appear in the wild, sometimes from the same vendor
// behind different proxies.
func findSeparator(b []byte) (idx, sepLen int) {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))

	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf < 0:
		return lf, 2
	case lf < 0:
		return crlf, 4
	case crlf < lf:
		return crlf, 4
	case lf < crlf:
		// A bare "\n\n" earlier in the buffer wins, unless it is the tail of
		// the CRLF pair we also found (i.e. "\r\n\r\n" contains "\n\r\n").
		return lf, 2
	default:
		return lf, 2
	}
}

// Field is one parsed line of an SSE event.
type Field struct {
	Name  string // "data", "event", "id", "retry", or "" for a comment
	Value []byte
}

// ParseEvent splits one event block into its fields. Comment lines (":" first)
// are returned with an empty Name; vendors use them as keep-alive pings, and
// they are meaningful here because a stream of nothing but pings is a stall
// that has not technically stopped sending.
func ParseEvent(block []byte) []Field {
	var fields []Field
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' {
			fields = append(fields, Field{Value: bytes.TrimSpace(line[1:])})
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			// A field name with no colon is legal SSE and means an empty value.
			fields = append(fields, Field{Name: string(name)})
			continue
		}
		// Exactly one optional leading space is stripped, per the SSE spec.
		value = bytes.TrimPrefix(value, []byte(" "))
		fields = append(fields, Field{Name: string(name), Value: value})
	}
	return fields
}

// DataOf concatenates the data fields of an event, which the SSE spec joins
// with newlines when a payload spans several `data:` lines.
func DataOf(block []byte) []byte {
	var parts [][]byte
	for _, f := range ParseEvent(block) {
		if f.Name == "data" {
			parts = append(parts, f.Value)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return bytes.Join(parts, []byte("\n"))
}
