package telemetry

import "io"

// Option configures the Logger at construction.
type Option func(*Logger)

// WithBusEmitter installs the production runtime.error emitter. Phase
// 05+ wires this when constructing the Logger. Without it, Logger.Error
// only writes the slog record (the default noopEmitter is a no-op).
func WithBusEmitter(b BusEmitter) Option {
	return func(l *Logger) {
		if b != nil {
			l.busEmitter = b
		}
	}
}

// WithWriter sets the destination io.Writer for the slog handler.
// Default is os.Stdout. Tests use this to inspect emitted records;
// production callers do not need it. Concurrent writes to the
// supplied writer must be safe at the writer's own layer — the
// Logger does not serialize writes itself (the slog handler does
// best-effort serialization but bytes.Buffer is not safe).
func WithWriter(w io.Writer) Option {
	return func(l *Logger) {
		if w != nil {
			l.writer = w
		}
	}
}
