# Phase 85e — mcp-roots-provider

## Summary

Ship a real MCP roots provider, replacing the honest-empty capability Phase 85a put in place. Roots define the filesystem / workspace boundaries an MCP server may operate within. Harbor exposes only operator-approved `file://` roots, answers `roots/list`, validates root URIs against path-traversal, and emits `notifications/roots/list_changed` when the approved set changes. The path-safety helper from `internal/skills/importer/path_safety.go` is reused — Harbor does not reinvent traversal checks.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §3: Phase 85a "advertises an explicit empty `Capabilities` (no roots) until Phase 85e ships the real provider." — this phase is that provider; it re-enables the capability honestly.
- brief 14 §2 (#26): "Capability advertised … but no `RootsListChangedHandler` / roots provider wired — Harbor cannot answer a `roots/list`." — closed.
- brief 14's matrix (Roots security row): "Validate root URIs, prevent path traversal, enforce access control … Clients must only expose roots with appropriate permissions." — the operator-approval gate + path-safety reuse implement this.
- AGENTS.md §7.5 (path traversal): "use the helper in `internal/skills/importer/path_safety.go`; don't reinvent." — binding; this phase reuses it.

## Findings I'm departing from (if any)

- None.

## Goals

- Harbor advertises the `roots` capability *with `listChanged`* — honestly, because a provider now backs it.
- Operators declare approved workspace roots via config; only those `file://` roots are exposed to MCP servers.
- `roots/list` returns the approved set; `notifications/roots/list_changed` fires when the set changes (config reload / operator action).
- Every exposed root URI is validated: it is a well-formed `file://` URI, it normalises (no `..` traversal escaping the declared root), and the path exists / is accessible.
- Roots are identity-aware: an operator can scope a root to a tenant / user so a server connected under one identity cannot enumerate another identity's workspace.

## Non-goals

- Non-`file://` root schemes — the spec allows other schemes but Harbor V1-of-this-band exposes filesystem roots only.
- A Console UI for picking roots interactively — operators declare roots in config; an interactive picker is a later Console enhancement.
- Write enforcement *inside* a root — roots declare boundaries; enforcing that a server's file operations stay within them is the server's obligation. Harbor exposes and validates the boundary; it does not police every server file op.

## Acceptance criteria

- [ ] `ClientOptions.Capabilities` advertises `roots` with `listChanged: true` — and a provider backs it (the 85a honesty stopgap is removed).
- [ ] A `RootsListChangedHandler` / roots provider answers `roots/list` with the operator-approved set.
- [ ] Each root URI passes validation: well-formed `file://`, `filepath.Clean`-normalised, `strings.HasPrefix(absPath, declaredRoot)` holds (path-safety helper reused, not reinvented).
- [ ] A config change to the approved-roots set emits `notifications/roots/list_changed`; a connected server re-fetching `roots/list` sees the new set.
- [ ] Identity scoping: a root scoped to tenant A is not returned to a server connected under tenant B — verified by a two-identity test.
- [ ] A malformed or traversal-escaping root in config fails validation **loudly at load** (fail-closed per AGENTS.md §5) — Harbor does not silently drop a bad root.
- [ ] Concurrent-reuse: `roots/list` is safe under N≥100 concurrent server queries against a single provider instance.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `roots.go`: the roots provider, `roots/list` handler, list-changed emission.
- `internal/tools/drivers/mcp/mcp.go` — advertise `roots` with `listChanged`; register the provider (removes the 85a empty-capability stopgap).
- `internal/config/config.go` — operator config for approved roots (`file://` URIs, optional identity scoping).
- `internal/skills/importer/path_safety.go` — reused (no change expected; if the helper needs a small generalisation, that is in-scope and documented).
- Test files — config fixtures with valid + traversal-escaping roots; mock MCP server querying `roots/list`.
- `examples/harbor.yaml` — document the roots config.
- `scripts/smoke/phase-85e.sh`.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/config (delta — illustrative)

type MCPRootConfig struct {
    URI    string // file:// URI
    Tenant string // optional identity scoping
    User   string // optional
}
```

No new exported MCP-driver surface; the roots provider is package-internal.

## Test plan

- **Unit:** URI validation (valid `file://`, non-`file` scheme, malformed, traversal-escaping); path-safety helper integration; identity-scope filtering.
- **Integration:** mock MCP server querying `roots/list`; config reload triggering `list_changed`; real path-safety helper; identity propagation; `-race`.
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** N≥100 concurrent `roots/list` queries; config reload during queries.
- **Failure modes:** traversal-escaping root in config (fail-closed at load); nonexistent path.

## Smoke script additions

- `scripts/smoke/phase-85e.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/roots.go` exists.
  - Assert `roots.go` imports the `internal/skills/importer` path-safety helper (reuse, not reinvention).
  - Assert the MCP driver advertises `roots` with `listChanged`.

## Coverage target

- `internal/tools/drivers/mcp`: 85%.

## Dependencies

- 28 (MCP driver).
- 85a (the honest-empty roots capability this phase replaces).

## Risks / open questions

- **Symlink escape.** A `file://` root that is itself a symlink, or contains symlinks pointing outside, can defeat a naive prefix check. The validation must resolve symlinks (`filepath.EvalSymlinks`) before the prefix check; this is called out as a test case.
- **Config-reload race.** `list_changed` emission during an in-flight `roots/list` must not corrupt the response; the provider's root set is swapped atomically (D-025 discipline).
- **Identity-scoping ergonomics.** Operators may find per-identity root config verbose; the phase ships a sensible default (unscoped roots visible to all identities of a tenant) and documents the scoping syntax.

## Glossary additions

- **MCP roots** — operator-approved `file://` filesystem boundaries Harbor exposes to MCP servers via `roots/list`. A server should confine its file operations to the declared roots. Harbor validates every root against path-traversal before exposing it.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Cross-isolation test passes** — identity-scoped roots do not leak across identities.
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `roots/list` under `-race`.
- [ ] **Integration test passes** — mock MCP server + config reload + real path-safety helper.
- [ ] Glossary updated.
- [ ] No brief departures.
