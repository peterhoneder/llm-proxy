package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/peterhoneder/llm-proxy/internal/fault"
	"github.com/peterhoneder/llm-proxy/internal/record"
)

// applyStripParams removes the route's strip_params from the request body.
//
// This is the only code path in llm-proxy that edits what a client sent, so it
// is deliberately narrow: it is off unless a route names keys, it only ever
// deletes top-level keys, and anything it cannot do cleanly it does not do at
// all. Whatever happens is recorded — a rewritten request that is not reported
// as rewritten would undermine every verdict this tool exists to give.
func (h *routeHandler) applyStripParams(rec *record.Request, body []byte, rest io.Reader) []byte {
	keys := h.target.route.StripParams
	if len(keys) == 0 {
		return body
	}

	if rest != nil {
		// Past max_request_body the remainder is streamed straight through and
		// was never buffered, so there is no whole document to rewrite. Half a
		// rewrite would corrupt the body outright, so the request goes as the
		// client wrote it and the report says the shim did not run.
		rec.Warn(fault.KindStripSkipped, fmt.Sprintf(
			"strip_params (%s) not applied: the body exceeds max_request_body, so it is streamed "+
				"rather than buffered and cannot be rewritten", strings.Join(keys, ", ")))
		return body
	}

	out, removed, err := stripParams(body, keys)
	if err != nil {
		// Re-encoding a document that decoded cleanly should not fail. If it
		// somehow does, forward the client's original bytes rather than
		// anything half-built.
		rec.Warn(fault.KindStripSkipped, fmt.Sprintf(
			"strip_params (%s) not applied: the body could not be re-encoded: %v",
			strings.Join(keys, ", "), err))
		return body
	}
	if len(removed) == 0 {
		return body
	}

	rec.SetStrippedParams(removed)
	// Both of these describe what goes upstream, not what arrived: the report
	// names the stripped keys separately, so the body it shows should be the
	// one the vendor is answering. ReqBody is non-nil only under full_trace.
	rec.ReqBodyBytes = int64(len(out))
	if rec.ReqBody != nil {
		rec.ReqBody = out
	}
	return out
}

// stripParams deletes top-level keys from a JSON object body and returns the
// re-encoded document along with the keys that were actually present.
//
// Values are held as json.RawMessage so everything that is kept survives
// byte-for-byte: number precision, string escaping, the exact shape of nested
// objects. Only the top level is rebuilt, which reorders keys alphabetically
// and drops the client's whitespace. That is the smallest edit that can remove
// a key, and it is why the caller reports what it did.
//
// A body that is not a JSON object is not an error: there is nothing to strip
// from, and inventing a rewrite for it is not this function's call. It goes
// upstream untouched, exactly as peekPayload leaves an unparseable body alone.
func stripParams(body []byte, keys []string) ([]byte, []string, error) {
	if len(keys) == 0 || len(body) == 0 {
		return body, nil, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, nil, nil
	}

	var removed []string
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			delete(obj, k)
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return body, nil, nil
	}

	// encoding/json escapes <, > and & by default, which would rewrite every
	// HTML or XML tag inside a prompt into < escapes — semantically equal,
	// but a gratuitous change to bytes the operator may well be diffing, and a
	// sizeable one for the tag-heavy prompts agents send.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return body, nil, err
	}
	// Encode appends a newline that the client did not send.
	return bytes.TrimRight(buf.Bytes(), "\n"), removed, nil
}
