# Phase 02 — Configuration loader

## Summary
Land `internal/config` — the strongly-typed configuration surface for Harbor. Loads YAML via `goccy/go-yaml`, applies env-var overrides, validates fail-loudly, and exposes a redacted marshal for boot-time logging. Establishes the slot layout (`Server`, `Identity`, `Telemetry`, `State`, `LLM`, `Governance`, plus reserved sub-structs that future phases fill in) so subsequent phases own their config slices without cross-package import cycles.

## RFC anchor
- RFC §10 (Stack decisions — YAML = `goccy/go-yaml`, Go 1.26+, the per-area sub-struct pattern).

## Briefs informing this phase
- brief 06

## Brief findings incorporated
- brief 06 (CLI / config patterns): configuration is loaded once at boot. Hot-reload is opt-in per field; default is restart-required. This matches AGENTS.md §10 ("Hot-reloadable fields documented in the phase plan that introduces them. Default: not hot-reloadable; restart-required.").
- brief 06: redaction in logs is non-negotiable. The redactor itself lives in `internal/audit` (Phase 03), but `Config.MarshalForLogging()` produces an already-redacted form so the boot-log can print configuration without leaking secrets — even before the audit subsystem is wired up.
- brief 06: validation errors must be **specific** (point to the offending source path), not generic. A misconfigured YAML file should tell the operator exactly which key in which section failed, not "validation failed."

## Findings I'm departing from (if any)
- None.

## Goals
- A single `Load(ctx, path)` entry point that produces a `*Config` ready for every subsystem, with overrides applied and validation passed.
- Per-area sub-structs (one per RFC §6 subsystem) so future phases extend their slice in isolation.
- Fail-loudly validation: missing required fields, invalid enum values, conflicting settings all surface with file path + key path.
- A `Reload` annotation system (struct tag `reload:"live"` vs default `reload:"restart"`) — Phase 02 ships the mechanism with zero `live` fields; future phases opt in per field.
- A redaction-aware marshal so the boot log can print the loaded config safely.

## Non-goals
- Hot-reload mechanics (no fsnotify watcher, no SIGHUP handler) — that lands when the first `live` field arrives in a later phase.
- CLI flag overrides — `harbor` flag layering lands in Phase 64 when the CLI scaffolds; the loader exposes a `WithOverrides(map[string]string)` seam so flags can layer in cleanly later.
- Live config push from Console — out of scope for V1; the seam is the same `WithOverrides`.
- Per-tenant configuration — Harbor V1 is a single-config runtime; multi-tenant isolation is identity-driven, not config-driven.
- Schema versioning machinery — additive evolution per AGENTS.md §10 is sufficient until V1.5.

## Acceptance criteria
- [ ] `Config` struct compiles; sub-structs exist and are slot-named per RFC §6 subsystems: `Server`, `Identity`, `Telemetry`, `State`, `LLM`, `Governance`. Reserved zero-value sub-structs for: `Runtime`, `Memory`, `Skills`, `Tasks`, `Sessions`, `Artifacts`, `Events`, `Audit`, `Protocol`, `CLI` (filled by their owning phases).
- [ ] `Load(ctx, path)` returns `*Config` from a YAML file, applies env-var overrides via the convention `HARBOR_<SECTION>_<FIELD>` (case-insensitive matching), and runs `Validate` before return.
- [ ] `LoadFromBytes(ctx, data)` is the testable shape; `Load` is a thin wrapper that reads the file and forwards.
- [ ] `Validate()` fails loudly on missing required fields with descriptive errors of the shape `config.<section>.<field>: <reason> (source: <path>:<line>)` — line info preserved from `goccy/go-yaml`'s position tracking.
- [ ] No silent defaults for security-relevant fields. Required-and-no-default: JWT algorithm allowlist, audit redaction patterns, identity-mandatory flags. Documented defaults exist for non-security fields (e.g. `Server.BindAddr` default `127.0.0.1:8080`).
- [ ] `MarshalForLogging()` returns YAML bytes with secret-shaped fields replaced by `"***"`. Field detection: struct tag `secret:"true"` plus a name-based fallback list (`api_key`, `apikey`, `token`, `password`, `secret`, `client_secret`, `private_key`, `signing_key`).
- [ ] `Config` is immutable after `Load`. Subsystems hold `*Config` references; mutation is prohibited (no setters, no exported pointers into mutable fields).
- [ ] `Reload` annotations system: struct tag `reload:"live"` marks a field hot-reloadable; default is `restart`. `(*Config).LiveReloadable() []string` returns dotted key paths of `live` fields. Phase 02 returns empty slice — the mechanism is in place; no field opts in yet.
- [ ] `examples/harbor.yaml` exists with a minimal but valid skeleton; round-trips through `Load → MarshalForLogging` cleanly.
- [ ] `internal/config` ≥ 85% test coverage.
- [ ] `make drift-audit` passes; `make preflight` passes.

## Files added or changed
- `internal/config/config.go` — `Config` + sub-struct types, `Reload` tag conventions.
- `internal/config/loader.go` — `Load`, `LoadFromBytes`, env-var application, `WithOverrides` seam.
- `internal/config/validate.go` — `Validate` and per-section validators (each section validates its own fields; a top-level `Validate` orchestrates them).
- `internal/config/redact.go` — `MarshalForLogging` and the secret-detection helper.
- `internal/config/config_test.go`, `loader_test.go`, `validate_test.go`, `redact_test.go`.
- `internal/config/testdata/` — fixtures (valid minimal, invalid-missing-required, invalid-enum, env-var-override).
- `examples/harbor.yaml` — minimal skeleton.

## Public API surface
```go
package config

type Config struct {
    Server     ServerConfig     `yaml:"server"`
    Identity   IdentityConfig   `yaml:"identity"`
    Telemetry  TelemetryConfig  `yaml:"telemetry"`
    State      StateConfig      `yaml:"state"`
    LLM        LLMConfig        `yaml:"llm"`
    Governance GovernanceConfig `yaml:"governance"`
    // Reserved slots for future phases (zero values are valid until owners fill them).
    Runtime    RuntimeConfig    `yaml:"runtime,omitempty"`
    Memory     MemoryConfig     `yaml:"memory,omitempty"`
    Skills     SkillsConfig     `yaml:"skills,omitempty"`
    Tasks      TasksConfig      `yaml:"tasks,omitempty"`
    Sessions   SessionsConfig   `yaml:"sessions,omitempty"`
    Artifacts  ArtifactsConfig  `yaml:"artifacts,omitempty"`
    Events     EventsConfig     `yaml:"events,omitempty"`
    Audit      AuditConfig      `yaml:"audit,omitempty"`
    Protocol   ProtocolConfig   `yaml:"protocol,omitempty"`
    CLI        CLIConfig        `yaml:"cli,omitempty"`
}

func Load(ctx context.Context, path string) (*Config, error)
func LoadFromBytes(ctx context.Context, data []byte) (*Config, error)
func WithOverrides(c *Config, overrides map[string]string) (*Config, error) // for CLI flags later
func (c *Config) Validate() error
func (c *Config) MarshalForLogging() ([]byte, error)
func (c *Config) LiveReloadable() []string
```

## Test plan
- **Unit:** `Load`, `LoadFromBytes`, env-var override (precedence: env > YAML), `Validate` happy path + failure modes, `MarshalForLogging` golden test (input config → expected redacted YAML), `LiveReloadable` returns empty in Phase 02.
- **Integration:** `examples/harbor.yaml` loads cleanly; round-trips through `MarshalForLogging`; the result is valid YAML that re-parses (without secrets, used for boot-log only).
- **Conformance:** N/A (single implementation).
- **Concurrency / leak (D-025 concurrent-reuse contract):** `*Config` is a canonical reusable artifact (built once at boot, shared across every subsystem). Test runs N≥100 goroutines reading multiple `*Config` field paths concurrently under `-race`; goroutine count returns to baseline. The test exists as much to *enforce* the "immutable after Load" claim (a future PR that adds a write path will fail under `-race`) as to verify it. Per AGENTS.md §5 + §11 + RFC §3.5.
- **Failure-mode tests:** missing file (`os.IsNotExist`), invalid YAML (parse error includes file:line), missing required field (error includes section.field path), invalid enum value (error lists allowed values).

## Smoke script additions
`scripts/smoke/phase-02.sh` — Phase 02 has no HTTP surface and no runtime-side effects. Smoke is a single `skip "phase 02: config package validated by go test (no HTTP surface)"` so the per-phase smoke entry exists for the drift-audit's plan↔smoke pairing rule and for `make preflight` accounting.

## Coverage target
- `internal/config`: 85%.

## Dependencies
- None. Wave 1 — independent of phases 01 and 03; can be implemented in parallel.

## Risks / open questions
- Position tracking through `goccy/go-yaml` for line-accurate validation errors is library-dependent. Risk: if the library's `ast` package doesn't expose enough position info, the error format degrades to "field path only." Mitigation: validation error format includes file path even when line is unavailable; `(source: <path>)` is the minimum.
- The reserved sub-struct slots are a forward-compatibility bet. If a future phase needs a slot we haven't reserved, it adds the field — but that is a breaking change for any existing deployment unless the field is `omitempty`. Mitigation: every reserved sub-struct is `omitempty` by default; new fields within sub-structs follow the AGENTS.md §10 backward-compat rule.
- Env-var override semantics for nested structs: the convention `HARBOR_<SECTION>_<FIELD>` works for one level of nesting. Two-level nesting (`Config.LLM.Provider.Default`) needs `HARBOR_LLM_PROVIDER_DEFAULT`. Documented; testable; not a blocker.
- No RFC §11 open questions block this phase.

## Glossary additions
- None.

## Pre-merge checklist
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/config` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A for Phase 02 (config is process-singleton).
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departures).
