// Package telemetry owns Harbor's canonical structured logger.
//
// The Logger wraps log/slog with three load-bearing additions:
//
//  1. A pinned eight-attribute identity surface (tenant_id, user_id,
//     session_id, run_id, task_id, trace_id, span_id, tool). The first
//     five flow from ctx via the Phase 01 identity helpers; the rest
//     are passthrough keys reserved for OTel wiring (Phase 55).
//  2. Mandatory redaction: every record's attribute values AND the
//     msg string flow through audit.Redactor before the slog handler
//     sees them. Redaction failures are fail-loudly (D-020) — the
//     record is replaced with a sentinel line, never silently emitted
//     unredacted.
//  3. A BusEmitter seam so Phase 05+ can fire a paired runtime.error
//     bus event without re-opening this package (RFC §6.14, brief 06).
//
// A *Logger is built once at boot via New, then shared across every
// emit path. WithIdentity / WithRun / With return derived loggers
// that carry additional bound attributes; the base *Logger stays
// unchanged. Concurrent reuse is enforced by D-025 — the shipping
// test suite runs N≥100 goroutines through a single shared instance
// under -race.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrLoggerNotConfigured — the constructor received an invalid
	// TelemetryConfig (unknown LogFormat, unknown LogLevel, etc).
	// Phase 02 already validates these but the constructor must not
	// trust upstream; misconfiguration here is a fail-loudly.
	ErrLoggerNotConfigured = errors.New("telemetry: logger not configured")
	// ErrRedactorMissing — the constructor was given a nil redactor.
	// Wraps audit.ErrRedactorMissing so callers can errors.Is on
	// either sentinel.
	ErrRedactorMissing = errors.New("telemetry: redactor missing")
)

// redactedSentinel is what the Logger emits when audit.Redactor.Redact
// returns an error. The original record (msg + attrs) is dropped on
// the floor — callers MUST NOT see the unredacted bytes.
const redactedSentinel = "[redacted: log emission blocked by redactor error]"

// busEmitGuardKey is a context key used to break single-step BusEmitter
// recursion: when Logger.Error invokes EmitRuntimeError, the ctx
// handed to the emitter carries this marker, and a re-entrant call
// to Logger.Error using that ctx skips the emitter (still writes the
// slog record).
type ctxKey int

const busEmitGuardKey ctxKey = iota

// Logger is the canonical structured logger. Built once at boot via
// New; safe for concurrent use; immutable after construction.
//
// WithIdentity, WithRun, and With return DERIVED *Logger instances —
// the receiver is never mutated. Bound attributes flow through every
// subsequent emit; ctx-derived identity attributes fill in keys not
// already bound. Per-call attributes append last.
type Logger struct {
	handler    slog.Handler
	redactor   audit.Redactor
	busEmitter BusEmitter
	writer     io.Writer
	bound      []slog.Attr
}

// New constructs a Logger from validated config and a Redactor.
// Returns a wrapped sentinel on invalid input. The handler is chosen
// once at construction and never swapped (RFC §6.14: no in-library
// toggle).
func New(cfg config.TelemetryConfig, r audit.Redactor, opts ...Option) (*Logger, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %w", ErrRedactorMissing, audit.ErrRedactorMissing)
	}
	l := &Logger{
		redactor:   r,
		busEmitter: noopEmitter{},
		writer:     os.Stdout,
	}
	for _, opt := range opts {
		opt(l)
	}
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLoggerNotConfigured, err)
	}
	handlerOpts := &slog.HandlerOptions{Level: level}
	switch cfg.LogFormat {
	case "json":
		l.handler = slog.NewJSONHandler(l.writer, handlerOpts)
	case "text":
		l.handler = slog.NewTextHandler(l.writer, handlerOpts)
	default:
		return nil, fmt.Errorf("%w: invalid log_format %q (want json or text)",
			ErrLoggerNotConfigured, cfg.LogFormat)
	}
	return l, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log_level %q (want debug, info, warn, or error)", s)
}

// WithIdentity returns a derived Logger that pre-stamps tenant_id,
// user_id, session_id from id. The base Logger is unchanged.
func (l *Logger) WithIdentity(id identity.Identity) *Logger {
	return l.derive(
		slog.String("tenant_id", id.TenantID),
		slog.String("user_id", id.UserID),
		slog.String("session_id", id.SessionID),
	)
}

// WithRun returns a derived Logger that pre-stamps the identity
// triple plus run_id from q. The base Logger is unchanged.
func (l *Logger) WithRun(q identity.Quadruple) *Logger {
	return l.derive(
		slog.String("tenant_id", q.TenantID),
		slog.String("user_id", q.UserID),
		slog.String("session_id", q.SessionID),
		slog.String("run_id", q.RunID),
	)
}

// With returns a derived Logger carrying additional bound attributes.
// In Phase 04 this is the only path for task_id, trace_id, span_id,
// and tool — Phase 55 wires OTel-driven trace_id / span_id auto-stamping.
func (l *Logger) With(attrs ...slog.Attr) *Logger {
	return l.derive(attrs...)
}

func (l *Logger) derive(attrs ...slog.Attr) *Logger {
	merged := make([]slog.Attr, 0, len(l.bound)+len(attrs))
	merged = append(merged, l.bound...)
	merged = append(merged, attrs...)
	clone := *l
	clone.bound = merged
	return &clone
}

// Debug emits a structured record at debug severity. Identity
// attributes are auto-stamped from ctx; every value flows through
// the configured Redactor before the slog handler is invoked.
func (l *Logger) Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelDebug, msg, attrs)
}

// Info emits a structured record at info severity.
func (l *Logger) Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelInfo, msg, attrs)
}

// Warn emits a structured record at warn severity.
func (l *Logger) Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelWarn, msg, attrs)
}

// Error emits a structured record at error severity AND fires the
// configured BusEmitter.EmitRuntimeError. Recursion via the bus
// emitter is broken by a context sentinel: an emitter that hands
// control back to this Logger.Error uses a ctx that suppresses the
// second emitter invocation (the slog record still writes).
//
// A panic from the bus emitter is recovered and recorded as a Warn
// entry (`telemetry.bus_emitter_panicked`); the panic does not
// propagate to the caller.
func (l *Logger) Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	redactedMsg, redactedAttrs, ok := l.emit(ctx, slog.LevelError, msg, attrs)
	if !ok {
		return
	}
	if ctx.Value(busEmitGuardKey) != nil {
		return
	}
	guarded := context.WithValue(ctx, busEmitGuardKey, struct{}{})
	l.invokeBusEmitter(guarded, redactedMsg, redactedAttrs)
}

func (l *Logger) invokeBusEmitter(ctx context.Context, msg string, attrs []slog.Attr) {
	defer func() {
		if r := recover(); r != nil {
			l.emitInternalSentinel(ctx, slog.LevelWarn,
				"telemetry: bus emitter panicked",
				slog.Any("panic", fmt.Sprint(r)))
		}
	}()
	l.busEmitter.EmitRuntimeError(ctx, msg, attrs)
}

// emit assembles the record (bound + ctx + per-call), redacts every
// value plus the msg string, hands the result to the slog handler,
// and returns the redacted msg + attrs for downstream uses (the bus
// emitter receives the same redacted payload).
//
// Returns ok=false when redaction fails — in that case a sentinel
// line has been emitted and the caller MUST NOT trigger any further
// side effect (no bus emit, etc).
func (l *Logger) emit(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) (string, []slog.Attr, bool) {
	final := make([]slog.Attr, 0, len(l.bound)+len(attrs)+5)
	final = append(final, l.bound...)
	final = append(final, attrs...)
	final = appendCtxIdentity(ctx, final)
	final = resolveAttrs(final)

	redactedMsg, redactedAttrs, err := l.redact(ctx, msg, final)
	if err != nil {
		l.emitSentinel(ctx, level)
		return "", nil, false
	}

	rec := slog.NewRecord(time.Now(), level, redactedMsg, 0)
	rec.AddAttrs(redactedAttrs...)
	_ = l.handler.Handle(ctx, rec)
	return redactedMsg, redactedAttrs, true
}

// emitSentinel writes the redactedSentinel line at level with no
// attrs. Used when audit.Redactor.Redact returns an error.
func (l *Logger) emitSentinel(ctx context.Context, level slog.Level) {
	rec := slog.NewRecord(time.Now(), level, redactedSentinel, 0)
	_ = l.handler.Handle(ctx, rec)
}

// emitInternalSentinel writes a canned message that is NOT routed
// through the redactor. Used for telemetry-internal failure modes
// (bus emitter panic, etc.) where the message and attrs are fixed
// strings the package author wrote — no user-supplied bytes — so
// redaction would be ceremony.
func (l *Logger) emitInternalSentinel(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	rec := slog.NewRecord(time.Now(), level, msg, 0)
	rec.AddAttrs(attrs...)
	_ = l.handler.Handle(ctx, rec)
}

// appendCtxIdentity adds tenant_id / user_id / session_id (and run_id
// if a Quadruple is present) to attrs IFF those keys aren't already
// bound. Explicit > ctx — bound or per-call attrs win on conflict.
func appendCtxIdentity(ctx context.Context, attrs []slog.Attr) []slog.Attr {
	has := func(name string) bool {
		for _, a := range attrs {
			if a.Key == name {
				return true
			}
		}
		return false
	}
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if !has("tenant_id") {
			attrs = append(attrs, slog.String("tenant_id", q.TenantID))
		}
		if !has("user_id") {
			attrs = append(attrs, slog.String("user_id", q.UserID))
		}
		if !has("session_id") {
			attrs = append(attrs, slog.String("session_id", q.SessionID))
		}
		if !has("run_id") {
			attrs = append(attrs, slog.String("run_id", q.RunID))
		}
		return attrs
	}
	if id, ok := identity.From(ctx); ok {
		if !has("tenant_id") {
			attrs = append(attrs, slog.String("tenant_id", id.TenantID))
		}
		if !has("user_id") {
			attrs = append(attrs, slog.String("user_id", id.UserID))
		}
		if !has("session_id") {
			attrs = append(attrs, slog.String("session_id", id.SessionID))
		}
	}
	return attrs
}

// resolveAttrs runs Value.Resolve() over every attr — important for
// LogValuer-shaped values that materialize on demand. Without this,
// a LogValuer whose Resolve() returned a secret would bypass
// redaction. Group attrs are recursively resolved.
func resolveAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			sub := resolveAttrs(a.Value.Group())
			out = append(out, slog.Attr{Key: a.Key, Value: slog.GroupValue(sub...)})
			continue
		}
		out = append(out, a)
	}
	return out
}

// redact runs the configured audit.Redactor over the msg string and
// the attribute set. Returns the redacted msg + redacted attrs in
// the same order as the input (slog handlers preserve order).
func (l *Logger) redact(ctx context.Context, msg string, attrs []slog.Attr) (string, []slog.Attr, error) {
	attrMap := attrsToMap(attrs)
	redactedAttrAny, err := l.redactor.Redact(ctx, attrMap)
	if err != nil {
		return "", nil, err
	}
	redactedMap, ok := redactedAttrAny.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("%w: redactor returned %T, want map[string]any",
			audit.ErrRedactionFailed, redactedAttrAny)
	}
	msgAny, err := l.redactor.Redact(ctx, map[string]any{"_msg": msg})
	if err != nil {
		return "", nil, err
	}
	msgMap, ok := msgAny.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("%w: redactor returned %T for msg, want map[string]any",
			audit.ErrRedactionFailed, msgAny)
	}
	redactedMsg, _ := msgMap["_msg"].(string)
	return redactedMsg, mapToOrderedAttrs(redactedMap, attrs), nil
}

// attrsToMap converts []slog.Attr into the map[string]any shape the
// audit.Redactor expects. Group attrs become nested maps; concrete
// scalar kinds round-trip naturally.
func attrsToMap(attrs []slog.Attr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		m[a.Key] = attrValueToAny(a.Value)
	}
	return m
}

func attrValueToAny(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration()
	case slog.KindTime:
		return v.Time()
	case slog.KindGroup:
		sub := make(map[string]any, len(v.Group()))
		for _, ga := range v.Group() {
			sub[ga.Key] = attrValueToAny(ga.Value)
		}
		return sub
	}
	return v.Any()
}

// mapToOrderedAttrs rebuilds []slog.Attr preserving the order of
// `original`. A key that appears multiple times in `original` is
// emitted multiple times (slog convention — ordering matters; some
// handlers print every entry).
func mapToOrderedAttrs(redacted map[string]any, original []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(original))
	for _, a := range original {
		v, ok := redacted[a.Key]
		if !ok {
			continue
		}
		out = append(out, slog.Any(a.Key, v))
	}
	return out
}
