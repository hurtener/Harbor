package telemetry

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// W3C TraceContext propagation carriers (RFC §6.14, brief 06 §"Key
// data shapes"). Harbor propagates a trace across three southbound
// idioms, each with an Inject* / Extract* half so trace continuity
// holds across HTTP and stdio process boundaries:
//
//   - HTTP southbound: the W3C `traceparent` (+ `tracestate`) header.
//   - stdio MCP: `_meta.traceparent` (+ `_meta.tracestate`) in the
//     per-request `_meta` map.
//   - stdio spawn: the HARBOR_TRACEPARENT (+ HARBOR_TRACESTATE) env
//     var on the child process environment.
//
// All three idioms encode the SAME W3C TraceContext values — the
// stdio idioms are just different carriers for the bytes the HTTP
// header would carry. The carriers consult the OTel global
// TextMapPropagator (set once by NewTracer to a W3C composite);
// callers that have not constructed a Tracer get the OTel default,
// which is a no-op propagator — Inject* writes nothing, Extract*
// returns ctx unchanged. That is fail-safe by W3C design: extracting
// a remote trace id is best-effort.
//
// These helpers are standalone functions, NOT Tracer methods, so the
// southbound transport drivers (Phase 27 tools/HTTP, Phase 28
// tools/MCP) can wire them in without holding a *Tracer reference.

// W3C header / map / env key names.
const (
	headerTraceparent = "traceparent"
	headerTracestate  = "tracestate"

	// EnvTraceparent is the env var key carrying the W3C traceparent
	// to a stdio child process spawned by Harbor.
	EnvTraceparent = "HARBOR_TRACEPARENT"
	// EnvTracestate is the env var key carrying the W3C tracestate to
	// a stdio child process. Present only when the upstream trace
	// carried tracestate.
	EnvTracestate = "HARBOR_TRACESTATE"
)

// InjectHTTP writes the W3C traceparent (and tracestate, when the
// span context carries it) header from the span context in ctx into
// h. A ctx with no active span writes nothing — h is left untouched.
func InjectHTTP(ctx context.Context, h http.Header) {
	if h == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

// ExtractHTTP returns a ctx carrying the remote span context decoded
// from the W3C headers in h. A malformed or absent traceparent yields
// a ctx with no valid span context (best-effort, never panics) — the
// caller's own SpanFromEvent then starts a root span instead of a
// child.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	if h == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(h))
}

// metaCarrier adapts a stdio-MCP `_meta` map[string]any to the OTel
// propagation.TextMapCarrier interface. The MCP `_meta` map is
// untyped JSON, so values round-trip as strings; non-string values
// already present are left untouched (Keys / Get skip them).
type metaCarrier struct {
	meta map[string]any
}

func (c metaCarrier) Get(key string) string {
	v, ok := c.meta[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (c metaCarrier) Set(key, value string) {
	c.meta[key] = value
}

func (c metaCarrier) Keys() []string {
	out := make([]string, 0, len(c.meta))
	for k, v := range c.meta {
		if _, ok := v.(string); ok {
			out = append(out, k)
		}
	}
	return out
}

// InjectMeta writes traceparent (and tracestate, when present) into a
// stdio-MCP `_meta` map. A nil map is a no-op; a ctx with no active
// span writes nothing.
func InjectMeta(ctx context.Context, meta map[string]any) {
	if meta == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, metaCarrier{meta: meta})
}

// ExtractMeta returns a ctx carrying the remote span context decoded
// from a stdio-MCP `_meta` map. A nil map or a map without a valid
// traceparent yields a ctx with no valid span context (best-effort).
func ExtractMeta(ctx context.Context, meta map[string]any) context.Context {
	if meta == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, metaCarrier{meta: meta})
}

// envCarrier adapts a process-environment string slice (KEY=VALUE
// entries) to the OTel propagation.TextMapCarrier interface, mapping
// the W3C `traceparent` / `tracestate` keys onto the HARBOR_TRACEPARENT
// / HARBOR_TRACESTATE env var names.
//
// Set appends to (never rewrites) the slice — InjectEnv returns the
// extended slice. Get / Keys read whatever entries are present.
type envCarrier struct {
	env *[]string
}

// w3cToEnv maps a W3C carrier key to the Harbor env var name.
func w3cToEnv(key string) (string, bool) {
	switch key {
	case headerTraceparent:
		return EnvTraceparent, true
	case headerTracestate:
		return EnvTracestate, true
	}
	return "", false
}

// envToW3C maps a Harbor env var name back to the W3C carrier key.
func envToW3C(name string) (string, bool) {
	switch name {
	case EnvTraceparent:
		return headerTraceparent, true
	case EnvTracestate:
		return headerTracestate, true
	}
	return "", false
}

func (c envCarrier) Get(key string) string {
	name, ok := w3cToEnv(key)
	if !ok {
		return ""
	}
	for _, entry := range *c.env {
		k, v, found := strings.Cut(entry, "=")
		if found && k == name {
			return v
		}
	}
	return ""
}

func (c envCarrier) Set(key, value string) {
	name, ok := w3cToEnv(key)
	if !ok {
		return
	}
	// Replace an existing entry for the same env var rather than
	// appending a duplicate — a child process reading os.Environ
	// would otherwise see two HARBOR_TRACEPARENT lines.
	entry := name + "=" + value
	for i, existing := range *c.env {
		if k, _, found := strings.Cut(existing, "="); found && k == name {
			(*c.env)[i] = entry
			return
		}
	}
	*c.env = append(*c.env, entry)
}

func (c envCarrier) Keys() []string {
	out := make([]string, 0, 2)
	for _, entry := range *c.env {
		k, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if w3cKey, ok := envToW3C(k); ok {
			out = append(out, w3cKey)
		}
	}
	return out
}

// InjectEnv appends HARBOR_TRACEPARENT (and HARBOR_TRACESTATE, when
// present) to a process environment slice and returns the extended
// slice. The input slice is not mutated in place beyond the append —
// callers pass the result to exec.Cmd.Env. A ctx with no active span
// returns env unchanged.
func InjectEnv(ctx context.Context, env []string) []string {
	out := env
	c := envCarrier{env: &out}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return out
}

// ExtractEnv returns a ctx carrying the remote span context decoded
// from HARBOR_TRACEPARENT (and HARBOR_TRACESTATE) in a process
// environment slice — the inverse of InjectEnv, called by a Harbor
// child process at startup to continue the parent's trace. A slice
// without a valid HARBOR_TRACEPARENT yields a ctx with no valid span
// context (best-effort).
func ExtractEnv(ctx context.Context, environ []string) context.Context {
	if len(environ) == 0 {
		return ctx
	}
	envCopy := environ
	c := envCarrier{env: &envCopy}
	return otel.GetTextMapPropagator().Extract(ctx, c)
}
