// Package inproc is Harbor's in-process tool driver. Operators
// register Go functions as Tools via RegisterFunc; the driver
// derives ArgsSchema / OutSchema from the function's input / output
// types via reflection (RFC §6.4 "Tool authors write a function and
// register it") and wires the call through the
// ToolPolicy reliability shell so the registered function gets
// production-resilient timeout + retry + validation for free. The
// derivation itself lives in the neutral internal/tools/schema
// package (§13) — this driver is one of its two consumers, alongside
// the typed embed binding (internal/runtime/assemble.RunTyped).
//
// Concurrent reuse: the driver itself is stateless — every
// RegisterFunc call builds a fresh ToolDescriptor and registers it
// in the catalog. The descriptor's Invoke closure captures the
// caller's `fn` (which the caller guarantees is safe for concurrent
// invocation); no mutable state lives in the driver.
package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/schema"
)

// ErrUnsupportedType — RegisterFunc rejected the input/output type
// at registration time because the reflection-based schema deriver
// cannot represent it (interfaces, channels, function-typed fields,
// cyclic structures). The error message names the offending Go
// field so the operator can fix it. Wraps via fmt.Errorf("%w: ...")
// pattern. Re-exported alias of schema.ErrUnsupportedType — the
// derivation now lives in the neutral internal/tools/schema package
// (§13; both this driver and RunTyped consume it), but the driver
// keeps its own sentinel name so existing callers' errors.Is checks
// are unaffected.
var ErrUnsupportedType = schema.ErrUnsupportedType

// ErrSchemaBuild — the schema compiler choked on the derived JSON
// Schema. Indicates a deriver bug; the operator should report it.
// Re-exported alias of schema.ErrSchemaBuild.
var ErrSchemaBuild = schema.ErrSchemaBuild

// RegisterFunc registers a Go function as a Tool. Input + output
// schemas are derived from the type parameters I and O via
// reflection.
//
// The function `fn` must be safe to invoke concurrently.
// The driver wraps it in a ToolPolicy shell — timeout + retry +
// validation — so a plain registration is production-resilient.
//
// `opts` configure the descriptor (policy, description, scopes,
// tags, examples). See DescriptorOption in the parent package for
// the full surface.
//
// Example:
//
//	type WeatherArgs struct {
//	    City string `json:"city"`
//	}
//	type WeatherOut struct {
//	    TempC float64 `json:"temp_c"`
//	    Summary string `json:"summary"`
//	}
//
//	err := inproc.RegisterFunc[WeatherArgs, WeatherOut](
//	    cat,
//	    "weather.lookup",
//	    func(ctx context.Context, in WeatherArgs) (WeatherOut, error) { ... },
//	    tools.WithDescription("Look up current weather by city name."),
//	    tools.WithAuthScopes("weather:read"),
//	    tools.WithSideEffect(tools.SideEffectExternal))
func RegisterFunc[I any, O any](
	cat tools.ToolCatalog,
	name string,
	fn func(ctx context.Context, in I) (O, error),
	opts ...tools.DescriptorOption,
) error {
	if cat == nil {
		return fmt.Errorf("inproc.RegisterFunc: catalog is nil")
	}
	if name == "" {
		return fmt.Errorf("inproc.RegisterFunc: name is empty")
	}
	if fn == nil {
		return fmt.Errorf("inproc.RegisterFunc: fn is nil for tool %q", name)
	}

	cfg := tools.ResolveOptions(opts...)

	// Derive schemas via reflection (the shared internal/tools/schema
	// deriver — one implementation, consumed here and by RunTyped).
	var zeroIn I
	var zeroOut O
	inSchema, err := schema.Derive(reflect.TypeOf(zeroIn))
	if err != nil {
		return fmt.Errorf("%w: input type for tool %q: %w", ErrUnsupportedType, name, err)
	}
	outSchema, err := schema.Derive(reflect.TypeOf(zeroOut))
	if err != nil {
		return fmt.Errorf("%w: output type for tool %q: %w", ErrUnsupportedType, name, err)
	}

	inSchemaBytes, err := json.Marshal(inSchema)
	if err != nil {
		return fmt.Errorf("%w: marshal input schema: %w", ErrSchemaBuild, err)
	}
	outSchemaBytes, err := json.Marshal(outSchema)
	if err != nil {
		return fmt.Errorf("%w: marshal output schema: %w", ErrSchemaBuild, err)
	}

	// Compile the input validator once; cache it in the closure.
	compiledIn, err := schema.Compile(inSchemaBytes)
	if err != nil {
		return fmt.Errorf("%w: compile input schema: %w", ErrSchemaBuild, err)
	}
	compiledOut, err := schema.Compile(outSchemaBytes)
	if err != nil {
		return fmt.Errorf("%w: compile output schema: %w", ErrSchemaBuild, err)
	}

	tool := tools.Tool{
		Name:        name,
		Description: chooseString(cfg.Description, name),
		ArgsSchema:  inSchemaBytes,
		OutSchema:   outSchemaBytes,
		SideEffects: chooseSideEffect(cfg.SideEffect),
		Tags:        cfg.Tags,
		AuthScopes:  cfg.AuthScopes,
		CostHint:    cfg.CostHint,
		LatencyHint: cfg.LatencyHint,
		SafetyNotes: cfg.SafetyNotes,
		Loading:     chooseLoading(cfg.Loading),
		Examples:    cfg.Examples,
		Source:      cfg.Source,
		Transport:   tools.TransportInProcess,
		Policy:      cfg.Policy,
	}

	bus := cfg.Bus
	toolName := tool.Name
	descriptor := tools.ToolDescriptor{
		Tool: tool,
		Validate: func(args json.RawMessage) error {
			return validateAgainst(compiledIn, args)
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			start := time.Now()
			publishToolInvoked(ctx, bus, toolName, start)
			result, err := invokeReflective[I, O](ctx, args, fn, tool.Policy, func(args json.RawMessage) error {
				return validateAgainst(compiledIn, args)
			}, func(result tools.ToolResult) error {
				return validateAgainstResult(compiledOut, result)
			})
			publishToolOutcome(ctx, bus, toolName, start, err)
			return result, err
		},
	}
	return cat.Register(descriptor)
}

// publishToolInvoked emits tool.invoked on the configured bus. A
// missing identity is treated as "publish skipped" — the tool
// boundary already rejects missing identity, so this branch is
// defensive (we don't want the publisher itself to panic on a nil
// identity slot during early-boot scenarios).
func publishToolInvoked(ctx context.Context, bus events.EventBus, name string, started time.Time) {
	if bus == nil {
		return
	}
	id, ok := identity.From(ctx)
	if !ok {
		return
	}
	q := identity.Quadruple{Identity: id}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; tool result is the source of truth
		Type:       tools.EventTypeToolInvoked,
		Identity:   q,
		OccurredAt: started,
		Payload: tools.ToolInvokedPayload{
			Identity:  q,
			ToolName:  name,
			Transport: tools.TransportInProcess,
			StartedAt: started,
		},
	})
}

// publishToolOutcome emits tool.completed on a successful invocation
// and tool.failed on a terminal failure. Error classification is
// best-effort; the inproc transport's class is permanent for any
// fn-returned error (the policy shell already classified before this
// point) and ErrToolPolicyExhausted maps to tool.policy_exhausted.
func publishToolOutcome(ctx context.Context, bus events.EventBus, name string, started time.Time, err error) {
	if bus == nil {
		return
	}
	id, ok := identity.From(ctx)
	if !ok {
		return
	}
	q := identity.Quadruple{Identity: id}
	dur := time.Since(started).Milliseconds()
	if err == nil {
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; tool result is the source of truth
			Type:       tools.EventTypeToolCompleted,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: tools.ToolCompletedPayload{
				Identity:   q,
				ToolName:   name,
				Transport:  tools.TransportInProcess,
				Attempts:   1,
				DurationMS: dur,
			},
		})
		return
	}
	evType := tools.EventTypeToolFailed
	class := tools.ErrClassPermanent
	if errors.Is(err, tools.ErrToolPolicyExhausted) {
		evType = tools.EventTypeToolPolicyExhausted
	}
	if errors.Is(err, tools.ErrToolInvalidArgs) {
		evType = tools.EventTypeToolInvalidArgs
	}
	switch evType {
	case tools.EventTypeToolInvalidArgs:
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; tool result is the source of truth
			Type:       evType,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: tools.ToolInvalidArgsPayload{
				Identity:        q,
				ToolName:        name,
				Transport:       tools.TransportInProcess,
				ValidationError: err.Error(),
			},
		})
	case tools.EventTypeToolPolicyExhausted:
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; tool result is the source of truth
			Type:       evType,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: tools.ToolPolicyExhaustedPayload{
				Identity:  q,
				ToolName:  name,
				Transport: tools.TransportInProcess,
				Attempts:  0,
				LastClass: class,
				LastError: err.Error(),
			},
		})
	default:
		_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit; tool result is the source of truth
			Type:       evType,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: tools.ToolFailedPayload{
				Identity:     q,
				ToolName:     name,
				Transport:    tools.TransportInProcess,
				Attempts:     1,
				ErrorClass:   class,
				ErrorMessage: err.Error(),
			},
		})
	}
}

// invokeReflective is the inner-most invocation: decode args into I,
// call fn(ctx, in), marshal the result as a tools.ToolResult, wrap
// the whole thing in the policy shell so retries / timeouts /
// validation all fire uniformly.
func invokeReflective[I any, O any](
	ctx context.Context,
	args json.RawMessage,
	fn func(ctx context.Context, in I) (O, error),
	policy tools.ToolPolicy,
	validateIn func(args json.RawMessage) error,
	validateOut func(result tools.ToolResult) error,
) (tools.ToolResult, error) {
	return tools.RunWithPolicyHooked(
		ctx,
		args,
		func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var in I
			if len(args) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
				dec := json.NewDecoder(bytes.NewReader(args))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&in); err != nil {
					return tools.ToolResult{}, fmt.Errorf("%w: decode args: %w", tools.ErrToolInvalidArgs, err)
				}
			}
			out, err := fn(ctx, in)
			if err != nil {
				return tools.ToolResult{}, err
			}
			return tools.ToolResult{Value: out}, nil
		},
		validateIn,
		validateOut,
		policy,
	)
}

// chooseString returns first when non-empty, else second.
func chooseString(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// chooseSideEffect normalises a zero-value SideEffect to the
// stateful default.
func chooseSideEffect(s tools.SideEffect) tools.SideEffect {
	if s == "" {
		return tools.SideEffectStateful
	}
	return s
}

// chooseLoading normalises a zero-value LoadingMode to Always.
func chooseLoading(m tools.LoadingMode) tools.LoadingMode {
	if m == "" {
		return tools.LoadingAlways
	}
	return m
}

// validateAgainst decodes args into a JSON value and validates it
// against the compiled schema. Thin wrapper over schema.Validate — the
// derivation + compile/validate machinery lives in the shared
// internal/tools/schema package (§13; both this driver and RunTyped
// consume it).
func validateAgainst(compiled *jsonschema.Schema, args json.RawMessage) error {
	return schema.Validate(compiled, args)
}

// validateAgainstResult marshals result.Value into JSON and
// validates it against the compiled schema. Used for output
// validation.
func validateAgainstResult(compiled *jsonschema.Schema, result tools.ToolResult) error {
	if compiled == nil {
		return nil
	}
	if result.Value == nil {
		return validateAgainst(compiled, json.RawMessage("null"))
	}
	buf, err := json.Marshal(result.Value)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	return validateAgainst(compiled, buf)
}
