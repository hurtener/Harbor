package auth_test

import (
	"context"
	"log/slog"
	"sync"
)

// recordingLogger captures every emitted slog record's Message + Attrs
// as a flat string so tests can scan for sentinel substrings (used
// to assert the audit emit does NOT leak the raw token).
//
// Concurrent-safe: the security suite + the concurrent-reuse test
// share one recordingLogger across N goroutines.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) slog() *slog.Logger {
	return slog.New(&recordingHandler{rec: r})
}

func (r *recordingLogger) append(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *recordingLogger) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

type recordingHandler struct {
	rec   *recordingLogger
	attrs []slog.Attr
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	line := rec.Message
	for _, a := range h.attrs {
		line += " " + a.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		line += " " + a.String()
		return true
	})
	h.rec.append(line)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{rec: h.rec, attrs: append(h.attrs, attrs...)}
}

func (h *recordingHandler) WithGroup(_ string) slog.Handler { return h }
