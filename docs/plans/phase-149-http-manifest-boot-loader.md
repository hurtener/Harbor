# Phase 149 — HTTP-manifest boot loader wiring

## Summary

Wires the documented-but-unwired boot path for `tools.http_manifests[]`: the assembly's catalog band loads each declared UTCP-style manifest via the HTTP driver's existing `LoadManifest` + `RegisterManifest` pair and registers its tools on the runtime catalog by name, and `config.Validate` flips from REJECTING a non-empty list (the SDK-friction-audit fail-loud guard) to VALIDATING it. Once manifest tools register by name, the EXISTING `tools.entries[].oauth` mechanism binds an OAuth provider to them with zero new OAuth machinery — closing the gap that no config-declarable tool could exercise catalog OAuth wrapping end-to-end, and giving adopters a black-box vehicle for runtime-initiated token exchange (the Phase 142 `tokenexchange` driver). Boot-time posture is fail-loud throughout: a listed manifest that is missing, unparseable, or path-unsafe fails the boot naming the file and the config key; a tool-name collision with an already-registered catalog tool fails the boot too.

## RFC anchor

- RFC §6.4 — "HTTP tool definitions: both inline (Go code: `RegisterHTTPTool(...)`) and out-of-process via UTCP-style manifest. Inline is the dev-loop ergonomic; **manifest is the operator deployment shape**." This phase makes the second half true at boot. Also the "Transports shipped at V1 / HTTP — UTCP-style manifest, static auth" bullet, and the tool-side OAuth paragraphs (`WrapWithOAuth` pre-check, D-271 token exchange) this phase's consumer exercises.
- RFC §3.4 — the fail-loudly principle governing the boot posture (never a silent skip of a declared manifest).

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- brief 03 §"Phase impact" T-2: "HTTP tools. UTCP-style manifest format; HTTP `ToolProvider` driver; static auth (API key, bearer, cookie); retry; rate-limit handling." The manifest format, driver, and static auth all shipped in Phase 27 (D-036); this phase delivers the last unshipped clause of T-2's operator shape — the manifest actually reaching the catalog from operator config.
- brief 03 §9 Q-3: "Do we ship inline HTTP tool definitions (Go code: `RegisterHTTPTool(...)`) or only out-of-process via a UTCP-style manifest file, or both?" — resolved "both" in RFC §6.4, with the manifest named the operator deployment shape. Today only the inline half works from a production path; a knob that is documented, exemplified, and rejected-at-validate is not "both."
- brief 03 §"Testing strategy" (integration list): "HTTP tool against `httptest.Server`" — the integration test's round-trip target is a fixture HTTP server, exactly the brief's posture; the manifest file is written at test time so its `url_template` carries the fixture's live address.
- brief 07 §1 (the elegance principle): the runtime — not the provider, not the caller — owns the protocol; a config-declared HTTP tool reaches the planner through the SAME catalog, dispatch trio, and reliability shell as every other tool. This phase adds a loader, not a dispatch path: after registration, a manifest tool is indistinguishable from a `RegisterHTTPTool` tool (same descriptor, same `ToolPolicy` shell, same `tools.entries[]` wrapping).

## Findings I'm departing from (if any)

None.

## Goals

- **Boot-path loading.** `assembleCatalogBand` (`internal/runtime/assemble/assemble.go`) walks `cfg.Tools.HTTPManifests`, calls the HTTP driver's `LoadManifest(path)` then `RegisterManifest(toolCat, m)` per entry, and registers every manifest tool on the catalog under its declared name. The slot is AFTER `builtin.RegisterWith` and BEFORE the catalog Builder's `Apply` — so `tools.entries[]` middleware (approval / OAuth / loading-mode) naming a manifest tool resolves cleanly, and a manifest tool colliding with a built-in fails loud. Both the binary (`cmd/harbor`) and `harbortest/devstack` are thin wrappers over `Assemble` (D-197), so the ONE wiring home serves both — no second projection, no config-duality (D-196).
- **Validate flip.** `internal/config/validate.go::validateTools` replaces the reject-populated-list guard (`config.go:773-780` documents it; `validate.go:1133-1138` enforces it) with structural validation: entries non-empty after trim, no duplicates after `filepath.Clean`, and the path-safety rule below. The `TestValidate_RejectsPopulatedHTTPManifests` test flips to its validating counterpart.
- **Path safety (§7 rule 5).** Manifest paths come from operator config. The loader normalization (in `config.Load`) resolves each entry: an absolute path is `filepath.Clean`ed and accepted (operator-declared, same trust posture as `artifacts.fs_root` — and the documented example shape `/etc/harbor/tools/weather.yaml` is absolute); a RELATIVE path is resolved against the config file's directory with the `internal/skills/importer/path_safety.go` posture — `filepath.Clean(filepath.Join(configDir, rel))` plus the canonical-prefix check — and a `../` escape outside the config directory is a loud `fieldError` naming `tools.http_manifests[i]`. The check is REPLICATED in `internal/config` (with a table test mirroring the importer helper's rejection cases), not imported from `internal/skills/importer` — config must not grow a skills dependency. The driver's own `LoadManifest` Clean+Abs remains as defense in depth.
- **Fail-loud boot.** A listed manifest that is missing, unreadable, unparseable, or invalid (the driver's `ErrManifestInvalid` taxonomy: literal secrets, `.Auth` template leaks, unknown fields, missing env refs) fails `Assemble` with an error naming BOTH the file path and the config key (`tools.http_manifests[i]`). A manifest tool name colliding with an existing catalog registration surfaces `tools.ErrToolDuplicateName` through `RegisterManifest` and fails the boot the same way. Never a silent skip (§13).
- **The §13 consumer: config-declared OAuth on a manifest tool.** With manifest tools registered by name, `tools.entries[].oauth` (the by-name binding home, D-090/D-095) wraps them via the existing `catalog.Builder.wrap` → `WrapWithOAuth` path — resolve-by-name at `catalog.go:276`, wrap at `:314-355`. Verified: this mechanism already works for ANY registered tool; nothing OAuth-shaped changes in this phase. The integration test binds a Phase 142 `tokenexchange` provider to a manifest tool, making the end-to-end black-box vehicle real: config in, brokered-credential pre-check + HTTP round-trip out.
- **Operator surface refresh, same PR.** `examples/harbor.yaml` (and `examples/dev.yaml`) rewrite the "not wired yet" comment blocks into working documentation, with a realistic `http_manifests` + `entries[].oauth` pairing and a checked-in example manifest (`examples/tools/http-weather.yaml`, secrets in `${ENV_VAR}` form). `docs/CONFIG.md` §`tools.http_manifests` drops the "Not yet wired at boot" paragraph and documents the load semantics, relative-path resolution, and failure posture (the docs-site page includes CONFIG.md via stub — no nav change needed). Stale "the boot loader is not wired" godoc in `internal/tools/drivers/http/manifest.go` and `internal/config/config.go` is rewritten. `docs/notes/sdk-friction-audit.md` §1's dead-knob entry gets a "closed by this phase" note.

## Non-goals

- **An `oauth` field ON `ManifestTool` itself.** The by-name `tools.entries[].oauth` path is THE OAuth binding home; a manifest-level field would be a second parallel implementation of the same conceptual feature (§13). A future decision may converge them; that is a new decisions entry against D-279, not an extension of this phase.
- **MCP / A2A manifest loaders.** `tools.mcp_servers` and `tools.a2a_peers` have their own boot paths; nothing here generalizes to them.
- **Hot reload of manifests.** Boot-only; `tools.http_manifests` stays restart-required (no `reload:"live"` tag), consistent with the rest of `ToolsConfig` (§10 default posture).
- **Any change to `WrapWithOAuth` semantics.** The wrapper's pre-check calls `prov.Token(ctx, source)` and DISCARDS the token (`catalog.go:492-494`) — availability gating, not injection. That stays as-is: southbound header injection is transport-specific and Phase 148 owns it for MCP; the HTTP driver's own static-auth (`auth_ref`) carries the request credential. The relationship is stated in the example config's comments so operators don't expect the brokered token in the HTTP request.
- **New OAuth machinery of any kind.** No new provider driver, no new wrapper, no new pause path.
- **A `tools.http_manifest_root` allowlist knob.** Absolute operator paths are accepted after Clean (matching every other operator-declared path in the config); the prefix check applies to relative entries against the config directory. If a deployment posture later wants a mandatory root, that is an additive knob in a future phase.

## Acceptance criteria

- [x] `Assemble` with a config declaring `tools.http_manifests: [<path>]` registers every tool in the manifest on `Stack.Catalog` under its declared name, with provenance `Source = manifest:<...>` intact; the registration happens before the catalog Builder applies `tools.entries[]`, so an entry naming a manifest tool resolves.
- [x] `config.Validate` accepts a populated `tools.http_manifests` list (structural checks only: non-empty entries, unique after Clean); an empty list stays valid; the old reject-because-unwired guard and its comment are gone.
- [x] `config.Load` resolves relative entries against the config file's directory and rejects a relative entry that escapes it (lexical Clean+prefix, §7 rule 5) with a `fieldError` naming `tools.http_manifests[i]`; absolute entries are Cleaned and accepted. (`harbor validate`'s own loader path — `LoadFromBytesAt`, added this phase — exercises the identical check instead of silently skipping it.)
- [x] Boot failure modes are loud and name file + config key: missing file, unparseable YAML, `ErrManifestInvalid` (literal secret, `.Auth` leak, missing env var), and duplicate tool name against an already-registered catalog tool (`tools.ErrToolDuplicateName` propagates).
- [x] Integration test: a stack assembled from a config declaring a temp manifest + an `entries[].oauth` binding (Phase 142 `tokenexchange` provider against the §17.8 RFC-8693 fixture broker) proves (a) the tool resolves, (b) invoking it round-trips against a fixture HTTP server with identity propagation and provenance asserted, (c) the OAuth-wrapped variant pre-checks the provider (exchange observed broker-side; identity triple asserted in the exchange), (d) missing-manifest and unknown-oauth-provider configs fail `Assemble` loudly.
- [x] Concurrent-reuse (D-025): N≥100 concurrent invocations of a manifest-registered tool through ONE shared catalog under `-race` — no races, no context bleed, no cancellation cross-talk, goroutine baseline restored.
- [x] `harbortest/devstack` boots the same config shape with no devstack-side change (inherits the wiring through `Assemble`) — asserted by the integration test using `Assemble` directly (both the binary and devstack are thin wrappers over it, D-196/D-197).
- [x] `examples/harbor.yaml` + `examples/dev.yaml` comment blocks rewritten; `examples/tools/http-weather.yaml` checked in; `docs/CONFIG.md` §`tools.http_manifests` updated; stale "not wired" godoc in `internal/tools/drivers/http/manifest.go` + `internal/config/config.go` rewritten; `docs/notes/sdk-friction-audit.md` §1 annotated closed.
- [x] §18 sweep: grep `docs/skills/` + `docs/recipes/` for `http_manifests` / the tools-config surface at implementation time and update any playbook documenting it in the same PR (pre-planning grep found zero skill/recipe hits — CONFIG.md is the load-bearing doc; re-verified at implementation: still zero hits, no playbook update needed).
- [x] `scripts/smoke/phase-149.sh` flips from skeleton to real assertions and passes; prior phases' smokes still pass.

## Files added or changed

- `internal/runtime/assemble/assemble.go` — manifest load+register loop in `assembleCatalogBand` (+ the HTTP driver import, following the settled `mcpdrv` / `searchcache` assembly-imports posture, D-197)
- `internal/config/validate.go` — reject-guard replaced with structural validation
- `internal/config/loader.go` — relative-path resolution against the config dir + §7 rule 5 prefix check (+ table test)
- `internal/config/config.go` — `HTTPManifests` godoc rewritten (no longer "NOT yet wired")
- `internal/config/validate_test.go` — validate-flip tests (accept-populated, reject-escape, reject-duplicate, reject-empty-entry)
- `internal/tools/drivers/http/manifest.go` — package/loader godoc rewritten; (only if needed) error-detail polish so boot errors carry the file path
- `test/integration/phase149_http_manifest_boot_test.go` — the E2E + failure modes + concurrent-reuse test
- `examples/harbor.yaml`, `examples/dev.yaml`, `examples/tools/http-weather.yaml`
- `docs/CONFIG.md` (§`tools.http_manifests`)
- `docs/notes/sdk-friction-audit.md` (§1 closed note)
- `scripts/smoke/phase-149.sh`
- `docs/glossary.md` (UTCP-manifest entry corrected + "HTTP-manifest boot loader"), `docs/decisions.md` (D-279), `docs/plans/README.md` (row + detail block)

## Public API surface

- None new. The load-bearing surfaces are pre-existing and unchanged: `http.LoadManifest(path string) (*Manifest, error)`, `http.RegisterManifest(cat tools.ToolCatalog, m *Manifest) error`, `config.ToolsConfig.HTTPManifests []string`, `tools.entries[].oauth`. The phase's deliverable is wiring + a validation-posture flip, not API.

## Test plan

- **Unit (config):** validate-flip table — previously-rejecting populated list now validates; empty list valid; empty-string entry rejected; duplicate (post-Clean) rejected; relative escape (`../outside.yaml`) rejected at Load with the field path in the error; absolute path accepted after Clean. Loader-side path-resolution table mirroring the importer helper's rejection cases.
- **Unit (assemble/http):** loader loop error wrapping — missing file / parse failure / `ErrManifestInvalid` / duplicate-name each produce an `Assemble` error containing the manifest path AND `tools.http_manifests`; partial failure closes the partial stack cleanly (the existing `Assemble` partial-stack contract).
- **Integration:** `test/integration/phase149_http_manifest_boot_test.go` — real drivers on every seam (inmem state/events/artifacts, real HTTP driver, real catalog Builder, real `tokenexchange` driver against the Phase 142 `httptest` broker fixture — §17.8: the fixture derives from RFC 8693's wire shape, reusing the shipped `p142Broker` pattern). Manifest written to `t.TempDir()` at test time with the fixture HTTP server's live URL in `url_template` and `${ENV_VAR}` static auth (dummy value via `t.Setenv`). Asserts: resolve-by-name; invoke round-trip (fixture server sees the request; result returns; identity triple propagated; provenance `ToolSourceID` asserted); OAuth-wrapped variant drives a broker exchange before dispatch and the broker-side assertion pins the identity triple; ≥2 failure modes (missing manifest file → loud `Assemble` error; unknown `entries[].oauth.provider` → loud `Assemble` error via the existing Builder path). Runs under `-race`.
- **Conformance:** N/A — no new driver seam; the HTTP driver's existing suite covers manifest parsing.
- **Concurrency / leak:** N≥100 concurrent invocations of one manifest-registered tool (plus interleaved `Resolve`/`List` calls) against the single shared catalog under `-race`; per-invocation identity assertions (no cross-talk); goroutine count returns to baseline after stack Close.

## Smoke script additions

`scripts/smoke/phase-149.sh` (`PREFLIGHT_REQUIRES: unit-tests`). Justification for the classification: the preflight dev server boots ONCE with a fixed config that carries `http_manifests: []`, and per-phase reboots are not part of the preflight contract — so the boot path is exercised by the `-race` integration test, and the validate flip by the built binary's `validate` subcommand against temp configs (the `phase-64a.sh` precedent: invokes `bin/harbor` but touches no network endpoint, so it stays in the parallel unit-tests batch).

- `go test -race` for `internal/config` (the flip tests) and the phase-149 integration test (`-run Phase149 ./test/integration/...`).
- `bin/harbor validate` ACCEPTS a temp config declaring `tools.http_manifests` pointing at a real temp manifest (with a degradation guard: if the built binary predates the flip, the leg SKIPs on the old rejection message rather than failing — the §4.2 coexistence convention).
- `bin/harbor validate` REJECTS a temp config whose relative manifest entry escapes the config directory, with `tools.http_manifests` in the output.
- Static grep: `internal/runtime/assemble/assemble.go` references `LoadManifest`/`RegisterManifest` (the wiring exists); `internal/config/validate.go` no longer contains the "not loaded at boot yet" rejection string.
- Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/tools/drivers/http`: ≥ 80% on touched lines (package already carries its suite; the target guards the godoc/error-detail touches)
- `internal/config`: no regression below current package coverage
- `internal/runtime/assemble` (touched lines): no regression below the package's post-144 coverage

## Dependencies

- 27 (the HTTP tool driver + `Manifest`/`LoadManifest`/`RegisterManifest`, D-036)
- 26 (the tool catalog + `ErrToolDuplicateName`)
- 64a (catalog Builder + `tools.entries[]` + `WrapWithOAuth`, D-090; the D-095 `tools.oauth_providers[]` registry)
- 110d (`assemble.Assemble` — the ONE config→stack home this phase extends, D-196/D-197)
- 142 (the `tokenexchange` driver + the §17.8 broker fixture — the test vehicle, D-271)

## Risks / open questions

- **Validator purity vs. `harbor validate` UX.** The validator stays I/O-free (structural checks only; precedent: `tools.mcp_servers` URLs are not probed at validate time), so `harbor validate` accepts a config whose manifest file does not exist yet — boot is the enforcement home for existence/parse. If operator feedback wants earlier detection, an opt-in `--deep` validate leg is a future additive; do not put filesystem reads in `Validate`.
- **Assembly imports a concrete driver.** §13's driver-import rule names `internal/drivers/prod`, `cmd/harbor`, and driver tests; `internal/runtime/assemble` already imports the concrete `mcp` and `searchcache` drivers as the settled D-197 production-assembly posture. This phase follows that precedent for the HTTP driver. If a wave-end audit wants the rule's text amended to name the assembly explicitly, that is a one-line CLAUDE.md follow-up — flagged here so it is deliberate, not drift.
- **Headless embedders constructing `Config` in Go** (no `config.Load`) bypass the loader's relative-path resolution; their entries resolve via the driver's Clean+Abs against the process CWD. Documented in the `HTTPManifests` godoc: embedders should pass absolute paths. Acceptable — the same is true of every other path field they set by hand.
- **Manifest env-var coupling at boot.** `LoadManifest` fails loud when an `auth.<name>.secret` env ref is unset — correct per §13, but it means a config with a declared manifest cannot boot without the secret env present (including in `harbor dev`). The example config's comments call this out; the smoke's validate legs do not require boot.
- **`entries[]` ordering constraint.** Manifest registration must precede the Builder apply; MCP attach currently runs AFTER the Builder (so `entries[]` cannot wrap MCP tools today). This phase deliberately slots manifests into the pre-Builder band and does not touch MCP ordering.

## Glossary additions

- **HTTP-manifest boot loader** — the assembly-owned boot path that loads each `tools.http_manifests[]` UTCP-style manifest and registers its tools on the runtime catalog by name, before `tools.entries[]` middleware applies (D-279). Fail-loud: a missing/unparseable/path-unsafe manifest or a tool-name collision fails the boot naming the file and the config key.
- (Correction, same PR) **UTCP manifest** — the existing entry's "Loaded at boot via `ToolsConfig.HTTPManifests`" clause becomes true with this phase; the entry gains the D-279 pointer.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` — SKIPPED locally with `HARBOR_PREFLIGHT_SKIP=1` (justified in the PR: `make vet`, `make lint`, the full `go test -race ./...`, the phase-149 smoke, and a manual spot-check of adjacent smokes — phase-64a/68/142/148 — all pass against the built binary; CI still runs the full preflight gate).
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (`internal/tools/drivers/http` 88.0%, `internal/config` 83.1% vs. an 82.9% baseline — no regression)
- [x] If multi-isolation paths changed: cross-session isolation test passes (the integration test's identity legs cover the invoke seam; no identity-scoped storage change)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (The catalog with manifest-registered tools is a compiled artifact — the test is mandatory.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It closes the config→http-driver→catalog→OAuth seam — mandatory.)
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
