// Package builtin ships the small set of opt-in tools that travel
// with the Harbor binary. Built-ins exist to give a freshly-scaffolded
// agent a zero-dependency way to prove the planner → executor →
// trajectory loop without forcing an operator to author Go code or
// attach an MCP server first.
//
// Phase 83n / D-153. V1.1 ships two built-ins:
//
//   - `clock.now` — returns the current UTC time as RFC 3339 +
//     epoch milliseconds. Useful as a heartbeat / sanity-check tool.
//   - `text.echo` — returns its `text` input verbatim. Useful as a
//     smoke-test action the planner can call without side effects.
//
// Built-ins are NEVER registered implicitly. The operator opts in
// via the `tools.built_in` yaml field, which the dev binary's
// `bootDevStack` passes to `builtin.Register(cat, names)`. An empty
// list registers nothing — the registry is purely additive and
// opt-in by design.
//
// The §4.4 seam pattern applies in the same shape as OAuth drivers
// (`internal/tools/auth/drivers/oauth2`) and planner drivers
// (`internal/planner/react`): the `internal/config` validator
// mirrors `KnownNames()` so a typo in the yaml fails at validation
// time rather than at boot. A drift test (`builtin_test.go`)
// asserts the two surfaces stay in lockstep.
//
// Concurrent reuse (D-025). Built-in tools are registered through
// `inproc.RegisterFunc`, which captures the closure into a fresh
// `ToolDescriptor` per call. The functions themselves
// (`clock.Now`, `text.Echo`) hold no per-invocation state and are
// safe for concurrent use; D-025 is trivially satisfied through the
// existing inproc driver's contract.
package builtin

import (
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// Sentinel errors. Callers (`cmd/harbor/cmd_dev.go::bootDevStack`,
// the devstack mirror, the config validator) compare via errors.Is.
var (
	// ErrUnknownBuiltIn is returned when a name in `tools.built_in`
	// is not in the registered set. The wrapped message lists every
	// known name so an operator sees the typo immediately.
	ErrUnknownBuiltIn = errors.New("builtin: unknown built-in tool")
	// ErrRegisterFailed wraps an underlying `inproc.RegisterFunc`
	// failure. Should be impossible at runtime (the inproc deriver
	// has unit tests against all built-in payload types) but is
	// surfaced loudly per §13 fail-loud posture.
	ErrRegisterFailed = errors.New("builtin: failed to register built-in tool")
)

// registrar binds a built-in name to the function that registers it
// against a catalog. Package-private — callers use RegisterWith, never
// touch the table directly.
type registrar func(rc RegistryContext) error

// registry holds the V1.1+ built-in surface. Each entry self-describes
// its name and a registration thunk that calls `inproc.RegisterFunc`
// with the right typed signature.
var registry = map[string]registrar{
	"clock.now": func(rc RegistryContext) error {
		return inproc.RegisterFunc[ClockNowArgs, ClockNowOut](
			rc.Catalog, "clock.now", ClockNow,
			tools.WithDescription("Return the current UTC time as RFC 3339 + epoch milliseconds."),
			tools.WithSideEffect(tools.SideEffectPure),
		)
	},
	"text.echo": func(rc RegistryContext) error {
		return inproc.RegisterFunc[TextEchoArgs, TextEchoOut](
			rc.Catalog, "text.echo", TextEcho,
			tools.WithDescription("Echo the input text back verbatim. Useful for smoke-testing the planner/executor loop."),
			tools.WithSideEffect(tools.SideEffectPure),
		)
	},
	// Phase 107c / D-167 — meta-tools for tool + skill discovery.
	"tool_search": func(rc RegistryContext) error {
		return registerToolSearch(rc.Catalog)
	},
	"tool_get": func(rc RegistryContext) error {
		return registerToolGet(rc.Catalog)
	},
	"skill_search": func(rc RegistryContext) error {
		return registerSkillSearch(rc.Catalog, rc.SkillStore)
	},
	"skill_get": func(rc RegistryContext) error {
		return registerSkillGet(rc.Catalog, rc.SkillStore)
	},
	"declarative_action": func(rc RegistryContext) error {
		return registerDeclarativeAction(rc.Catalog)
	},
	// Phase 107c follow-up (slice-of-C) — the escape hatch the LLM
	// uses to pull the full bytes of a heavy-content artifact ref the
	// prompt-builder inlined as a truncated preview. Always-loaded so
	// the LLM has the recovery path without needing tool_search.
	"artifact_fetch": func(rc RegistryContext) error {
		return registerArtifactFetch(rc.Catalog, rc.ArtifactStore)
	},
}

// KnownNames returns the sorted list of built-in tool names the
// binary ships with. The `internal/config` validator's
// `allowedBuiltInTools` allowlist mirrors this slice; the
// `TestKnownNames_MirrorsConfigAllowlist` test enforces no drift.
func KnownNames() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RegistryContext carries the dependencies builtins may need at
// registration time. All fields are optional — a builtin that
// doesn't use a field ignores it. Builtins that REQUIRE a field
// (e.g. `skill_search` needs `SkillStore`; `artifact_fetch` needs
// `ArtifactStore`) fail loud at invoke time with an operator-readable
// message when the dependency is nil.
type RegistryContext struct {
	Catalog       tools.ToolCatalog
	SkillStore    skills.SkillStore
	ArtifactStore artifacts.ArtifactStore
}

// Register attaches each named built-in to the catalog. Equivalent to
// RegisterWith(ctx, names) with a zero RegistryContext — use when no
// builtins need the skill store.
//
// Deprecated: prefer RegisterWith for new call sites. Kept for
// backward compatibility with existing tests + devstack wiring.
func Register(cat tools.ToolCatalog, names []string) error {
	return RegisterWith(RegistryContext{Catalog: cat}, names)
}

// RegisterWith attaches each named built-in to the catalog, passing
// the full RegistryContext so builtins that need the SkillStore
// (skill_search, skill_get) can reach it. Builtins that don't use
// the store ignore it.
func RegisterWith(rc RegistryContext, names []string) error {
	if rc.Catalog == nil {
		return fmt.Errorf("%w: catalog is nil", ErrRegisterFailed)
	}
	for _, name := range names {
		reg, ok := registry[name]
		if !ok {
			return fmt.Errorf("%w: %q (known: %v)", ErrUnknownBuiltIn, name, KnownNames())
		}
		if err := reg(rc); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrRegisterFailed, name, err)
		}
	}
	return nil
}
