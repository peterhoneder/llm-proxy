package obs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
)

// multiHandler fans one slog record out to several handlers, so a single
// logging call reaches both the pretty console and the OpenTelemetry bridge.
//
// It is about thirty lines, which is why it is written here rather than pulled
// in as a dependency.
type multiHandler []slog.Handler

// NewMultiHandler fans out to the given handlers, skipping nils.
func NewMultiHandler(handlers ...slog.Handler) slog.Handler {
	out := make(multiHandler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
}

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Clone per child: a handler may append to the record's internal
		// attribute slice, and a shared backing array would let one handler
		// corrupt what its siblings see.
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(slices.Clone(attrs))
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// syncWriter serialises writes to the console.
//
// The console renders multi-line fault blocks, and two requests finishing at
// the same moment would otherwise interleave line by line into something
// unreadable. One mutex and one Write per block keeps each report intact.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSyncWriter wraps w so concurrent writes cannot interleave.
func NewSyncWriter(w io.Writer) io.Writer { return &syncWriter{w: w} }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
