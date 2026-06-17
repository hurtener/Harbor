# Phase 109h — mcp-apps-host-capability

## Summary

The MCP southbound driver reads a server's MCP App (`io.modelcontextprotocol/ui`) display-mode capability but never advertises its OWN — `ClientCapabilities.Extensions` ships empty, so a spec-conformant server cannot learn the Harbor host renders apps and cannot tailor the app references it returns. This phase makes the driver advertise the host's renderable display modes during the MCP initialize handshake (the symmetric write side of the existing `negotiateDisplayModes` read path), sourced from a new deployment-level `tools.mcp_app_host.display_modes` config field (defaulting to the inline baseline), while PRESERVING the roots advertisement the runtime makes today.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 14

## Brief findings incorporated

- brief 14 §2 row 31 ("Extension negotiation — Absent: `ClientCapabilities.Extensions` never populated"): the driver never advertised any client extension, so the `io.modelcontextprotocol/ui` host capability a real ext-apps server reads to tailor its app references was invisible. This phase populates `Extensions` with the UI extension carrying the host's `displayModes`.
- brief 14 §2 row 4 ("Capability negotiation — Partial: only `roots.listChanged` advertised — and dishonestly"): the SDK advertises `roots.listChanged` by default when `Capabilities` is nil. Setting `Capabilities` to add the UI extension OVERRIDES that default, so this phase explicitly replicates the current roots advertisement (`RootsV2.ListChanged`) — otherwise opting into the extension silently drops roots.
- brief 14 §3 ("The roots honesty violation"): the current roots advertisement is a known correctness defect, but FIXING it is the separate 85a stopgap scope, not this phase. This phase PRESERVES current behaviour exactly (the honesty fix is orthogonal and lands on its own track).
- brief 14 §9 (glossary, "MCP capability negotiation"): the initialize-time exchange where client and server each advertise only the capabilities they actually service. The host advertises only the display modes it can actually render (filtered against the closed valid-mode set), so the advertisement stays honest.

## Findings I'm departing from (if any)

None. The phase advertises only what the host renders today (the inline baseline by default) and does not touch the roots honesty defect (brief 14 §3 / 85a).

## Goals

- The MCP driver advertises the `io.modelcontextprotocol/ui` extension with the host's renderable `displayModes` during the initialize handshake, populating the previously-empty `ClientCapabilities.Extensions`.
- The advertised modes are operator-configurable via a deployment-level `tools.mcp_app_host.display_modes` field, defaulting to the inline baseline (`[inline]`), and are also settable programmatically by an embedder via `AttachDeps.HostDisplayModes`.
- The roots capability the runtime advertises today is PRESERVED after the change (the regression guard).
- Sampling / elicitation remain inferred from their handlers, unaffected.

## Non-goals

- No fix to the roots honesty defect (brief 14 §3 / 85a) — current behaviour is preserved, not corrected.
- No per-server display-mode override (the host's rendering ability does not vary per server — it is a single deployment-level capability).
- No `sampling` / `elicitation` / `tasks` capability advertisement (out of this phase's scope; tracked separately in the 85-band).
- No new Protocol method or REST endpoint — the capability is an OUTBOUND client→server advertisement on the MCP handshake.

## Acceptance criteria

- [ ] A provider built with configured host display modes advertises the `io.modelcontextprotocol/ui` extension carrying those modes; a server reads them from the real SDK `InitializeParams`.
- [ ] A provider built with NO host modes advertises NO UI extension (the backward-compatible default for an embedder that does not opt in), leaving the SDK's default capability advertisement in place.
- [ ] After opting into the UI extension, the provider STILL advertises roots (`roots.listChanged`) — the regression guard fails if `RootsV2` is dropped.
- [ ] The host-side mode list is filtered against the closed valid-mode set (`inline` / `fullscreen` / `pip`), deduplicated, advertised order preserved.
- [ ] `tools.mcp_app_host.display_modes` validates against the closed set; an unknown or duplicate mode fails at config load.
- [ ] `ToolsConfig.MCPAppHostDisplayModes()` resolves a nil / empty block to the inline baseline and passes configured modes through.
- [ ] The boot loader threads the resolved modes once into every attached MCP provider via `AttachDeps.HostDisplayModes`.

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` — `Config.HostDisplayModes`; `hostCapabilities` + `filterHostDisplayModes`; wire into `New`'s `ClientOptions.Capabilities`.
- `internal/tools/drivers/mcp/attach.go` — `AttachDeps.HostDisplayModes`, projected onto `Config.HostDisplayModes`.
- `internal/runtime/assemble/assemble.go` — resolve `cfg.Tools.MCPAppHostDisplayModes()` once; pass into `AttachDeps`.
- `internal/config/config.go` — `ToolsConfig.MCPAppHost *MCPAppHostConfig`; `MCPAppHostConfig`; `MCPAppHostDisplayModes()` resolver.
- `internal/config/validate.go` — `allowedMCPAppDisplayModes` mirror set; validate `tools.mcp_app_host.display_modes`.
- `internal/tools/drivers/mcp/host_capability_test.go` — unit (configured / baseline / filter / mirror) + integration (two providers, capability echo, roots preserved, identity propagation).
- `internal/config/validate_test.go` — config validation + resolver tests.
- `examples/harbor.yaml`, `scripts/smoke/phase-109h.sh`, `docs/plans/README.md`, `docs/decisions.md` (D-224), `docs/glossary.md`.

## Public API surface

- `mcp.Config.HostDisplayModes []string` — host-render display modes the provider advertises. Empty leaves the SDK default untouched.
- `mcp.AttachDeps.HostDisplayModes []string` — the programmatic boot seam (an embedder sets it without YAML).
- `config.MCPAppHostConfig{ DisplayModes []string }` + `config.ToolsConfig.MCPAppHostDisplayModes() []string`.

## Test plan

- **Unit:** `hostCapabilities` advertises configured modes + preserves roots; returns nil for no/invalid modes; `filterHostDisplayModes` filters/dedupes/order-preserves; the driver's valid-mode set mirrors the config validator's; config validation (valid / unknown / duplicate); resolver default + pass-through.
- **Integration:** `TestHostCapabilityAdvertisement_TwoProviders_EchoUIExtensionAndPreserveRoots` — two providers built from ONE resolved config value, paired to real SDK in-memory transports; each server's captured `InitializeParams.Capabilities` echoes the configured modes AND still advertises roots; identity propagates on a real tool call; the opt-out provider advertises roots with no UI extension (the failure mode). Real drivers on the seam (in-mem bus, real SDK transports); the capability fixture derives from the SDK's actual `InitializeParams` shape (§17.8), not a hand blob.
- **Conformance:** N/A — no new driver.
- **Concurrency / leak:** `HostDisplayModes` is read once at `New` and immutable thereafter; the Provider's existing concurrent-reuse coverage stands (no new mutable run-state).

## Smoke script additions

- `scripts/smoke/phase-109h.sh` (static-only): asserts the UI-extension advertisement code, the roots-preservation guard, the config → AttachDeps → driver threading, the config field + its validation, and the example-config documentation all exist. No new inbound Protocol surface to probe (the capability is outbound on the MCP handshake).

## Coverage target

- `internal/tools/drivers/mcp`: maintain the package baseline; the new `hostCapabilities` / `filterHostDisplayModes` paths are fully covered by the unit + integration tests.
- `internal/config`: maintain the package baseline; the new validation branch + resolver are fully covered.

## Dependencies

- Phase 109a (the MCP Apps runtime surface + `negotiateDisplayModes` read path this phase mirrors symmetrically).

## Risks / open questions

- The default `[inline]` baseline starts advertising the UI extension on every dev-stack MCP connection. This is honest — the Console renders inline apps today (109b) — and a conformant server treats an unknown extension as ignorable, so no server breaks.
- The roots advertisement preserved here is a known honesty defect (brief 14 §3). This phase deliberately does NOT fix it (85a's scope); it only ensures opting into the UI extension does not silently widen the regression by dropping roots entirely.

## Glossary additions

- UI-host capability advertisement — the client→server `initialize`-time advertisement of the `io.modelcontextprotocol/ui` extension carrying the host's renderable display modes.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: the integration test asserts identity propagation on a real tool call after the capability handshake
- [x] **Reusable artifact:** `HostDisplayModes` is read once at construction and immutable; the Provider's existing concurrent-reuse coverage stands (no new run-state).
- [x] **Consumes a shipped subsystem's surface:** the integration test wires real SDK transports + the in-mem bus, asserts the capability echo + identity propagation, and covers the opt-out failure mode under `-race`.
- [x] If new vocabulary: glossary updated (UI-host capability advertisement)
- [x] If a brief finding was departed from: N/A — none departed.
