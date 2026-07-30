# Phase 217 — `meta_annotations` honour `_meta` path nesting

## Summary

Two mechanisms write into the same outbound MCP `_meta` map on the same call and they
disagree about what a dotted key means. `injection.meta_key` is a dot-separated PATH —
`injectMeta` (`internal/tools/drivers/mcp/mcp.go:1525-1545`) walks it creating
intermediate maps, so `vendor.api_key` lands at `_meta.vendor.api_key`.
`meta_annotations` merge FLAT — `buildIdentityMeta`
(`internal/tools/drivers/mcp/mcp.go:1391-1395`) does `meta[k] = v` verbatim, so
`vendor.account_id` lands as the single literal key `_meta["vendor.account_id"]`. A
receiver-style server reading one nested namespace can therefore be handed a credential
but not its non-secret companion value, by any route. This phase makes the annotation
merge honour the same nesting through the SAME helper, tightens the reserved-key guard
at all four doors, and closes the silent scalar/map collision that the widened path
would otherwise make reachable.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the southbound MCP driver and its outbound
  request surface).
- RFC §3.4 — The fail-loudly principle (the scalar/map collision this phase converts
  from a silent overwrite into a validation error).
- RFC §4.2 — Mandatory identity (the triple + `agent_id` stamps that a nested annotation
  must not be able to shadow).

**Departure from the first draft's anchors:** the draft cited `RFC §7` (Console layer).
Verified: this phase changes no wire type, no Protocol method, and no Console file —
`MetaAnnotations` already exists on `internal/protocol/types/agentconfig.go:119`, on
`web/console/src/lib/protocol/agentconfig.ts:81`, and in
`web/console/src/lib/protocol/wire-manifest.gen.json:584`, and its Go type
(`map[string]string`) does not change. `RFC §7` was an unearned citation and is dropped;
there is no D-223 lockstep regeneration and no D-209 docs regeneration in this phase.

## Briefs informing this phase

- brief 14
- brief 09

## Brief findings incorporated

- brief 14 (MCP client/host compliance): `_meta` is a single namespace shared by
  everything the host stamps. Two writers into one namespace with divergent key
  semantics is a host-side inconsistency the server cannot compensate for, because the
  server sees only the merged result. This is the whole motivation.
- brief 14: the spec-reserved `io.modelcontextprotocol/` namespace is off-limits to host
  annotations. This finding is the direct reason the guard must remain a WHOLE-KEY check
  in addition to becoming per-segment — see the guard rule below.
- brief 09 (MCP OAuth from bifrost): credential-target validation must be one predicate
  consulted by every door, so a key an operator may declare is exactly a key the
  redactor holds to `***`. This phase must not widen that predicate — the companion
  value is not a secret and does not belong in the channel built for secrets.

## Findings I'm departing from (if any)

None. (Every finding above is adopted; the first draft's departures were errors, not
deliberate departures, and are corrected in-line below.)

## Goals

- An annotation key containing `.` writes into a nested `_meta` map rather than becoming
  a literal flat key, matching what the injection mapping already does.
- Both mechanisms share one helper (`injectMeta`) and one reserved-key guard
  (`config.IsReservedMCPMetaKey`).
- The reserved-key guard becomes **per-segment AND whole-key** — strictly tighter than
  today, in both arms.
- One depth cap constant governs every `_meta` key path — annotation and injection, at
  every door — which requires hoisting the constant to break an import-direction
  problem.
- A path collision between two declared `_meta` paths on one connection fails LOUD at
  validation instead of silently discarding one of them.

## Non-goals

- **Widening `receiverInjectionCredentialSegments`** (`internal/config/validate.go:2545-2552`).
  It is a security control — the guarantee that any key an operator may declare for an
  injected value is a key the audit redactor holds to `***`
  (`internal/audit/rules.go:133` and `internal/config/validate.go:1915-1919` consult the
  same predicate). This phase changes only the annotation merge's key semantics, on a
  path that never carries a pulled credential.
- A second injection mapping per connection. That would reopen "one auth mode per
  connection" (`internal/runtime/agentcfg/protocol/addconnection.go:332-334`), which is
  load-bearing.
- Persisting attach-time `Headers`. Explicitly rejected: it would put a
  documented-as-secret field into a durable record.
- **Re-litigating the depth cap itself (D-346).** The cap stays at 16. Only its stated
  failure mode is corrected (see AC 6) and its home package moves. That is a doc +
  location fix, not a decision change.
- Redaction-rule changes in `internal/audit/`. The over-redaction described below is
  characterised and pinned by a test; changing `walkRedactKeys`' non-recursive
  replace-on-match behaviour would alter every rule's semantics and needs its own RFC
  entry.

## The guard rule — per-segment AND whole-key (the first draft's central error)

`config.IsReservedMCPMetaKey` (`internal/config/validate.go:2527-2534`) has **two arms**:

```go
func IsReservedMCPMetaKey(k string) bool {
    if _, ok := reservedMCPMetaAnnotationKeys[k]; ok {   // exact-match set
        return true
    }
    return strings.HasPrefix(k, mcpSpecMetaAnnotationPrefix)  // "io.modelcontextprotocol/"
}
```

Splitting `io.modelcontextprotocol/ui` on `.` yields `["io", "modelcontextprotocol/ui"]`.
Neither segment carries the `io.modelcontextprotocol/` prefix, so a **per-segment-ONLY**
check ADMITS a spec-reserved annotation that is refused today. The first draft's
acceptance criterion said "applied PER SEGMENT rather than to the whole key" and claimed
the result was "strictly tighter"; that claim is **false as written** and the change
would LOOSEN the guard.

Verified by grep — a per-segment-only guard breaks these shipped tests:

- `internal/config/validate_mcp_oauth_test.go:110-120` — "spec-prefixed
  meta_annotations key rejected", `MetaAnnotations: {"io.modelcontextprotocol/ui": "x"}`,
  expecting a `reserved` error on `tools.mcp_servers[0].meta_annotations`.
- `internal/runtime/agentcfg/protocol/setrevision_connections_test.go:55` —
  "spec-reserved meta annotation prefix", expecting `ErrInvalidConnection`.
- `internal/tools/drivers/mcp/oauth_test.go:372-378` — "spec-prefixed annotation key
  rejected", expecting `ErrOAuthBinding` from `resolveOAuthBinding`.

A fourth would also regress its assertion:
`internal/tools/drivers/mcp/oauth_test.go:126-139` asserts
`io.modelcontextprotocol/something` never reaches the merged `_meta`.

**The binding rule:** an annotation key is refused when
`config.IsReservedMCPMetaKey(wholeKey)` is true **OR** `config.IsReservedMCPMetaKey(seg)`
is true for any dot-segment `seg`. This is a strict superset of today's check in both
arms, so every test above keeps passing unchanged and `tenant.foo` becomes newly
refused. This is exactly the shape the injection path already uses at
`internal/config/validate.go:1903-1912` and
`internal/runtime/agentcfg/protocol/wireinjectiondescriptor.go:171-181` — except that
those apply the segment arm only, which is safe there because `meta_key` reaching the
spec namespace is separately impossible (its leaf must be a credential token), and
becomes uniform once this phase applies whole-key + per-segment everywhere.

## The four validation doors (the draft listed three)

Every door consults the one authority, `config.IsReservedMCPMetaKey`. All four must
gain the whole-key-AND-per-segment rule, the depth cap, and the collision rule, or the
guard is not a guard.

1. **Boot config** — `internal/config/validate.go:1780-1789` (`validateTools`, the
   `for k := range s.MetaAnnotations` loop). Whole-key only today.
2. **The wire door** — `internal/runtime/agentcfg/protocol/addconnection.go:745-755`
   (`validateConnectionAnnotations`), reached from `validateConnection`
   (`addconnection.go:323`, called at `:331`), which serves BOTH `add_mcp_connection`
   and `agent_config.set_revision` (the latter via `validateConnectionsSection`,
   `addconnection.go:421`). Whole-key only today.
3. **The attach door — MISSED BY THE DRAFT** —
   `internal/tools/drivers/mcp/attach.go:430-440` (`resolveOAuthBinding`), called at
   `attach.go:183` on the shared boot + runtime-set attach path. It runs its own
   whole-key annotation check and returns `ErrOAuthBinding`. The draft's "one guard"
   framing was false with this door missing; it is now in the file list and the test
   matrix.
4. **The merge-time re-check** — `internal/tools/drivers/mcp/mcp.go:1392-1394`
   (`isReservedMetaKey`, delegating to the same authority at `mcp.go:1413`). This one
   **skips** a reserved key (`continue`) rather than erroring, and that stays: it is a
   last-resort defence behind three loud doors, and a reserved annotation key cannot
   reach it from any door that has shipped. It gains the per-segment arm so it stays in
   lockstep. The **collision** case (below) is different and DOES error here, because a
   colliding annotation pair CAN sit in a revision persisted before this phase.

## The scalar/map collision — silent today, must fail loud

`injectMeta` (`internal/tools/drivers/mcp/mcp.go:1533-1540`):

```go
next, ok := cur[seg].(map[string]any)
if !ok {
    next = map[string]any{}
    cur[seg] = next
}
```

A non-map intermediate is **overwritten with no error and no log**. Annotations merge
first (`buildIdentityMeta` at `mcp.go:856`, `:1089`, `:1146`, `:1192`, `:1269`),
injection second (`injectMeta` from the `InjectionFormMeta` case at `mcp.go:1510`). So a
flat `vendor` annotation plus `injection.meta_key = vendor.api_key` **silently discards
the operator's annotation today**. That is the §13 silent-degradation shape on the exact
path this phase widens — after this phase, annotations can also collide with each other
(`{"a": "1", "a.b": "2"}`), multiplying the reachable cases.

**The rule this phase adds.** For one connection, the set of DECLARED `_meta` paths is
every `meta_annotations` key split on `.`, plus the `injection.meta_key` path when
`form: meta`. No two declared paths may be equal, and no declared path may be a proper
PREFIX of another. Both inputs are declared config on the same object, so the check is
fully decidable at validation time and belongs at all four doors, fail-loud.

**Why this also settles determinism by construction.** `buildIdentityMeta` merges with
`for k, v := range annotations` (`mcp.go:1391`) and Go randomises map iteration. Under
nesting, `{"a": "1", "a.b": "2"}` produces DIFFERENT wire bytes per RPC depending on
which key is visited first (one overwrites the other's node). With prefix-collisions
refused, the merged result is order-independent: distinct non-prefixing paths write
disjoint leaves. Determinism becomes a consequence of the validation rule rather than
of a sort, and the test asserts it rather than assuming it.

**Why the merge must still error.** A revision persisted before this phase can carry a
colliding pair — nothing rejects one today. `buildIdentityMeta` already returns
`(mcpsdk.Meta, error)` (`mcp.go:1380`), so the merge re-check returns a new typed
sentinel (`ErrMetaPathCollision`) and the call fails loud. No silent winner.

## The map-type identity hazard

`injectMeta` type-asserts `cur[seg].(map[string]any)` (`mcp.go:1535`). `mcpsdk.Meta` is
a NAMED type over `map[string]any`
(`$GOMODCACHE/github.com/modelcontextprotocol/go-sdk@v1.6.1/mcp/shared.go:431`), and a
Go type assertion to `map[string]any` fails on a dynamic type of `mcpsdk.Meta`. If the
annotation nesting path builds `mcpsdk.Meta` intermediates instead of `map[string]any`,
the assertion misses, `ok` is false, and the branch **silently REPLACES the map**,
wiping every sibling annotation in that namespace.

`injectMeta` handles the top level correctly (`cur := map[string]any(meta)` at
`mcp.go:1534` converts once). The hazard is entirely in what a NEW nesting call site
constructs. The mitigation is the §13 rule the draft already stated but did not test:
**adapt the call site to `injectMeta`, never fork the helper.** The test asserts the
concrete type of every intermediate node.

## Nesting causes OVER-redaction (not a leak) — state it, pin it, do not "fix" it

`internal/audit/rules.go:165-190` (`walkRedactKeys`): on a key match it replaces the
WHOLE value with `Placeholder` and does **not** recurse into it. The matching predicate
for the injection rule is `config.IsReceiverInjectionCredentialKey`
(`internal/audit/rules.go:133`), which matches on the LAST `-`/`_`/`.`-separated segment
(`internal/config/validate.go:2564-2577`).

Consequence for an annotation key `token.env`:

- **Today (flat):** the literal key is `token.env`; its last segment is `env`; `env` is
  not in `receiverInjectionCredentialSegments` → **not redacted**.
- **After nesting:** the node key is `token`; last segment `token` → matches → **the
  ENTIRE subtree collapses to `***`**, including any sibling under the same namespace —
  an injection credential's non-secret companion among them.

**Redaction COVERAGE is preserved** (nothing that was redacted stops being redacted; a
credential leaf under a matching node is still `***` because its whole parent is). The
defect is **over-redaction** of non-secret siblings, which degrades audit usefulness,
not audit safety. It is a consequence an operator must be told about, not a blocker.

**Honesty note on reachability — verified.** No production call site redacts the
outbound MCP `_meta` map. `grep -rn "Redact(" --include="*.go" internal/ | grep -v
_test.go | grep -v internal/audit/` returns 20 call sites and none of them is on the
southbound MCP outbound path; `internal/tools/drivers/mcp/` contains no `audit.` usage
outside `mcp_test.go:70`. So the over-redaction is **latent**: it becomes observable the
moment any surface audits the outbound `_meta`, which D-341 and D-346 both presuppose
when they justify the redaction-coverage predicate. The phase therefore pins it with a
DIRECT redactor unit test over a nested `_meta`-shaped payload, and does NOT claim an
end-to-end audit capture that does not exist. Documented in D-362 and in
`docs/CONFIG.md`.

## The depth cap — an import cycle, and a hoist

**Verified import direction:**

- `internal/runtime/agentcfg/protocol` imports `internal/config`
  (`go list -f '{{join .Imports "\n"}}' ./internal/runtime/agentcfg/protocol` →
  `github.com/hurtener/Harbor/internal/config`; used at
  `wireinjectiondescriptor.go:178` and `addconnection.go:750`).
- `internal/config` does NOT import `internal/runtime/agentcfg/protocol`
  (`go list -deps ./internal/config | grep -c agentcfg/protocol` → `0`).

`maxInjectionMetaKeyDepth = 16` lives at
`internal/runtime/agentcfg/protocol/wireinjectiondescriptor.go:62`. The first draft put
annotation depth enforcement in `internal/config/validate.go` while referencing that
constant — an **import cycle**. The constant is hoisted into `internal/config` as an
exported `MaxMCPMetaKeyDepth`, and `wireinjectiondescriptor.go` becomes a consumer
(`:62` deleted, `:171-172` reads `config.MaxMCPMetaKeyDepth`). That file was not in the
draft's list and is now.

**A second asymmetry the hoist closes.** Verified by grep
(`grep -rn maxInjectionMetaKeyDepth --include="*.go" .`): the depth cap exists **only**
at the wire door. Boot config validation (`internal/config/validate.go:1903-1920`) and
the driver's own `Config.validate()` (`internal/tools/drivers/mcp/mcp.go:383-392`) apply
the reserved-segment and credential-leaf rules to `injection.meta_key` but **no depth
cap**. So a boot-declared 20-segment `meta_key` is accepted today where the identical
wire-declared one is refused. Hoisting the constant lets both apply it, which is the
§17.6 "fix what the test finds" shape.

**§10 note on that closure:** it is a behaviour change to a shipped config surface. Blast
radius verified zero — the only `meta_key` in `examples/` is
`examples/harbor.yaml:889` (`# meta_key: vendor.api_key`, 2 segments, and commented
out), and the only deep fixture is `wireinjectiondescriptor_test.go:437`, which already
expects rejection. No real receiver key nests past two segments.

## Backward compatibility (§10) — the draft's claim was FALSE

The draft asserted "no in-tree caller populates it" and made that survey an acceptance
criterion. **The criterion was already failed at authoring time.** Full survey —
`grep -rn MetaAnnotations --include="*.go" .` — 45 hits; the POPULATING call sites are:

| Location | Value | Dotted? |
| --- | --- | --- |
| `internal/runtime/agentcfg/protocol/setrevision_connections_test.go:274` | `{"vendor.tag": "blue"}` | **YES** |
| `test/integration/wave_v110_test.go:751` | `{"deployment": "wave-v110"}` | no |
| `test/integration/phase148_mcp_southbound_oauth_test.go:229` | `{"deployment": "prod"}` | no |
| `test/integration/phase148_mcp_southbound_oauth_test.go:398` | `{"deployment": "prod"}` | no |
| `internal/config/validate_mcp_oauth_test.go:220` | `{"deployment": "prod", "team": "search"}` | no |
| `internal/tools/drivers/mcp/oauth_test.go:~110` | `{"env": ..., "team": ...}` | no |
| `internal/config/validate_mcp_oauth_test.go:104,115,126` | reserved/empty negatives | n/a |
| `internal/runtime/agentcfg/protocol/setrevision_connections_test.go:54,55,56` | reserved/empty negatives | n/a |
| `internal/tools/drivers/mcp/oauth_test.go:366,374,382` | reserved/empty negatives | n/a |

Non-Go surfaces carrying the field but populating nothing:
`internal/protocol/types/agentconfig.go:119`, `internal/agentcfg/agentcfg.go:242`,
`internal/runtime/agentcfg/protocol/addconnection.go:98`,
`internal/tools/drivers/mcp/mcp.go:194`, `internal/config/config.go:1624`,
`internal/runtime/serve/mcp_attacher.go:140`,
`web/console/src/lib/protocol/agentconfig.ts:81`,
`examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts:469`,
`examples/dev.yaml:295` (commented, flat: `deployment` / `fleet`).

**The honest §10 finding, in three parts:**

1. **A dotted annotation key is a deliberately-supported shape on the shipped wire
   surface, not an accident.**
   `setrevision_connections_test.go:274` puts `{"vendor.tag": "blue"}` inside the
   canonical happy-path `set_revision` round-trip and asserts at `:294-296` that it
   round-trips. So the surface treats a dotted key as WELL-FORMED, and any persisted
   revision may already carry one. The draft's "plausibly zero" was wrong about the
   fact, even though it happened to be right about the operator-facing consequence.
2. **Semantic blast radius on operator CONFIG is nonetheless zero.** Every other
   populating caller uses flat keys, and the only shipped example
   (`examples/dev.yaml:295-297`) is flat. No in-tree config, example, or integration
   fixture changes meaning.
3. **`setrevision_connections_test.go:274` does NOT break, and that is the subtle part.**
   Nesting is a MERGE-time semantic in `buildIdentityMeta`, not a STORAGE-time one: the
   revision persists `map[string]string` with the literal key `"vendor.tag"` unchanged,
   which is exactly what `:294` asserts. `vendor` and `tag` are non-reserved, so the
   tightened guard admits it. The test keeps passing untouched — but it is the proof
   that persisted dotted keys exist and will silently change WIRE meaning on the next
   call after upgrade.

**Migration path (required by part 3, since a populating caller exists).**

- The change is a MINOR-version behaviour change to `_meta` wire shape for connections
  that declared a dotted annotation. It is announced in `CHANGELOG.md` under the release
  that ships this phase, in `docs/CONFIG.md`, in `examples/dev.yaml`, and in
  `docs/glossary.md` — all four of which say "merged **verbatim**" today and become
  false.
- No config rewrite is required, because the new shape is the shape the operator was
  already asking for by writing a dotted key against a `_meta` map whose sibling
  mechanism has always treated dots as paths.
- The two newly-refused declarations are surfaced at BOOT (door 1) or at the wire call
  (doors 2/3), never silently: (a) a segment that is reserved (`tenant.foo`); (b) a
  path collision (`{"vendor": "x", "vendor.id": "y"}`, or a flat `vendor` annotation
  alongside `injection.meta_key: vendor.api_key`). Both error messages name the offending
  key and the rule.
- No migration for the depth cap: verified zero configs exceed 2 segments.

## Acceptance criteria

- [x] An annotation key containing `.` produces a nested `_meta` map, byte-identical in
      shape to what the same path produces through `injection.meta_key`.
- [x] The nesting uses the SAME helper the injection path uses (`injectMeta`) — not a
      second implementation (§13) — and every intermediate node it creates is of dynamic
      type `map[string]any`, NOT `mcpsdk.Meta`, asserted directly so the
      `mcp.go:1535` type assertion can never miss and silently replace a sibling-bearing
      map.
- [x] The reserved-key guard refuses a key when `config.IsReservedMCPMetaKey` is true for
      the WHOLE key **OR** for ANY dot-segment. `tenant.foo` becomes refused (newly);
      `io.modelcontextprotocol/ui` stays refused (regression guard). All four shipped
      spec-prefix tests named in "The guard rule" above pass UNCHANGED.
- [x] The guard, the depth cap, and the collision rule are applied at all FOUR doors:
      `config/validate.go:1780-1789`, `addconnection.go:745-755` (serving both
      `add_mcp_connection` and `set_revision`), `attach.go:430-440`, and the merge-time
      re-check at `mcp.go:1392-1394`.
- [x] The identity triple and agent provenance are still stamped LAST
      (`mcp.go:1396-1401`), so no annotation can shadow identity — asserted by a test
      that attempts exactly that, including via a nested path.
- [x] `config.MaxMCPMetaKeyDepth` (hoisted from
      `wireinjectiondescriptor.go:62`) caps annotation key segment count, and
      `wireinjectiondescriptor.go` consumes it rather than defining its own.
      **Corrected rationale:** the cap is NOT because a too-deep key would be emitted
      unredacted — `internal/audit/rules.go:167` returns `ErrRedactionDepthExceeded`, so
      the audit FAILS LOUD. The cap exists so a declared `_meta` path can never push an
      audit payload past `audit.MaxDepth` (= 64, `internal/audit/rules.go:34`) and turn
      every audit emit for that connection into a hard redaction failure. Same cap, same
      value (16), correct reason. Both the constant's godoc and the error message at
      `wireinjectiondescriptor.go:172` are corrected to say this.
- [x] The depth cap additionally applies to `injection.meta_key` at boot config
      validation (`validate.go:1903-1920`) and in the driver's `Config.validate()`
      (`mcp.go:383-392`), closing the wire-only asymmetry.
- [x] An annotation key with no `.` behaves exactly as today.
- [x] **Path collision fails LOUD.** Within one connection, no two declared `_meta`
      paths (annotation keys split on `.`, plus `injection.meta_key` when `form: meta`)
      may be equal or in a proper-prefix relationship. Refused at all four doors with an
      error naming both colliding keys. Pinned specifically:
      `{"vendor": "x", "vendor.id": "y"}` is refused, and a flat `vendor` annotation
      alongside `injection.meta_key: vendor.api_key` is refused — the case that today
      silently discards the operator's annotation at `mcp.go:1535-1538`.
- [x] **The merge fails loud on a legacy collision.** A connection whose persisted
      revision carries a colliding annotation pair (possible: nothing rejects it today)
      makes `buildIdentityMeta` return a new typed `ErrMetaPathCollision`, aborting the
      call. No silent winner, no order-dependent result.
- [x] **The merge is deterministic.** Repeated `buildIdentityMeta` calls over a
      non-colliding annotation set containing shared namespace prefixes produce
      byte-identical marshalled `_meta` across N≥1000 iterations, despite Go's randomised
      map iteration at `mcp.go:1391`.
- [x] An annotation and an injection mapping writing into the SAME namespace on one call
      compose into one nested map without either clobbering the other's siblings.
- [x] **The §10 survey above is reproduced in D-362**, including the
      `setrevision_connections_test.go:274` finding that a dotted key is a supported
      shape, and the migration path.
- [x] **The over-redaction consequence is pinned by a direct redactor test**: a nested
      `_meta`-shaped payload with an annotation node key `token` collapses its whole
      subtree to `audit.Placeholder`, while the flat `token.env` key does not — proving
      coverage is preserved and the change is over-redaction. Documented in
      `docs/CONFIG.md` and D-362. No `internal/audit/` rule change.
- [x] Mutation-verified: reverting the whole-key arm of the guard (leaving per-segment
      only) turns a smoke `OK` into a `FAIL`, and reverting the collision rule likewise.

## Files added or changed

- `internal/config/validate.go` — the whole-key-AND-per-segment annotation guard
  (`:1780-1789`); the annotation depth cap; the path-collision rule; the depth cap
  extended to `injection.meta_key` (`:1903-1920`); the hoisted exported
  `MaxMCPMetaKeyDepth`.
- `internal/runtime/agentcfg/protocol/wireinjectiondescriptor.go` — **NEW TO THE LIST**:
  delete the local `maxInjectionMetaKeyDepth` (`:62`), consume
  `config.MaxMCPMetaKeyDepth` (`:171-172`), correct the constant's godoc + the error
  message's stated failure mode.
- `internal/runtime/agentcfg/protocol/addconnection.go` — `validateConnectionAnnotations`
  (`:745-755`) gains per-segment + depth + collision; the collision check needs the
  injection descriptor's `meta_key`, so it is invoked from `validateConnection`
  (`:323-331`) after the injection mapping is in hand.
- `internal/tools/drivers/mcp/attach.go` — **NEW TO THE LIST**: `resolveOAuthBinding`
  (`:430-440`) gains the same three rules.
- `internal/tools/drivers/mcp/mcp.go` — `buildIdentityMeta` (`:1380-1404`) nests through
  `injectMeta`, re-checks per-segment reserved keys, and returns the new
  `ErrMetaPathCollision`; `Config.validate()` (`:383-392`) gains the depth cap;
  `injectMeta` (`:1525-1545`) unchanged in behaviour, its godoc updated to name its
  second caller and the `map[string]any` intermediate-type contract.
- `internal/tools/drivers/mcp/mcp_test.go`, `internal/tools/drivers/mcp/oauth_test.go`
- `internal/config/validate_test.go`, `internal/config/validate_mcp_oauth_test.go`
- `internal/runtime/agentcfg/protocol/wireinjectiondescriptor_test.go` (the `:437`
  comment references the renamed constant),
  `internal/runtime/agentcfg/protocol/setrevision_connections_test.go` (add collision +
  per-segment cases alongside the existing `:274` dotted round-trip, which stays)
- `internal/audit/rules_test.go` — the over-redaction characterisation test (test-only;
  no rule change).
- `test/integration/mcp_meta_nesting_test.go` — new.
- `examples/dev.yaml` — **CORRECTED FROM `examples/harbor.yaml`**: `meta_annotations` is
  documented at `examples/dev.yaml:280-297`; `examples/harbor.yaml` contains no
  `meta_annotations` at all (verified: `grep -c` → 0). The `:280` "merged verbatim"
  wording and the `:295-297` stanza gain the nesting, collision, and depth semantics.
  `examples/harbor.yaml:889`'s commented `meta_key: vendor.api_key` is 2 segments and
  needs no change.
- `docs/CONFIG.md` — **NEW TO THE LIST**: `:973-980` says "merged verbatim", which
  becomes false; add nesting, the collision rule, the depth cap, and the over-redaction
  note. `:1000` (`meta_key`) gains the now-uniform depth cap.
- `docs/glossary.md` — **NEW TO THE LIST**: `:673` ("Meta annotations") also says
  "merged verbatim"; update it and add the new **Annotation path** term.
- `docs/skills/add-an-in-process-tool/SKILL.md` — §18 sweep: `metadata.surface: tools`
  (`:7`), and `:285` describes `meta_annotations` riding into `_meta`. Updated in the
  same PR.
- `docs/plans/README.md` — flip the Phase 217 row (`:360`) to `Shipped`; the row's
  current text asserts "per-segment (strictly tighter)" and "no in-tree caller populates
  it", both corrected here, so the detail block is rewritten too (§4.2 item 11).
- `scripts/smoke/phase-217.sh` — exists as a skeleton; replace the `skip` with real
  assertions.
- `docs/decisions.md` — D-362.

**Overlap note for the coordinator:** correcting `examples/harbor.yaml` →
`examples/dev.yaml` dissolves the 213/217 file overlap flagged at wave level. The
`validate.go` / `mcp.go` / `addconnection.go` overlaps with 214 remain real.

## Public API surface

- `config.MaxMCPMetaKeyDepth` — NEW exported const (value 16), hoisted from
  `internal/runtime/agentcfg/protocol`. This is the only new exported identifier.
- `mcp.ErrMetaPathCollision` — NEW exported sentinel in `internal/tools/drivers/mcp`,
  comparable with `errors.Is`.
- No wire-type change, no Protocol method change, no Console change. `ProtocolVersion`
  unchanged. No D-223 lockstep regeneration, no D-209 docs regeneration.
- The behaviour change is in the merge semantics of an existing config field,
  documented on `config.MCPServerConfig.MetaAnnotations`
  (`internal/config/config.go:1617-1624`) and its three mirrors
  (`internal/tools/drivers/mcp/mcp.go:185-194`,
  `internal/agentcfg/agentcfg.go:238-242`,
  `internal/protocol/types/agentconfig.go:116-119`).

## Test plan

- **Unit:**
  - Flat key unchanged; dotted key nests; deep key nests to depth; depth cap refuses
    beyond `config.MaxMCPMetaKeyDepth`.
  - Reserved refusal per position: each reserved key as the whole key, as the first
    segment, as a middle segment, as the last segment — AND
    `io.modelcontextprotocol/ui` as the whole key (the arm per-segment-only would lose).
  - The four named shipped tests
    (`validate_mcp_oauth_test.go:110-120`, `setrevision_connections_test.go:55`,
    `oauth_test.go:372-378`, `oauth_test.go:126-139`) pass with **no edits** — a
    regression gate on the guard rule, not new coverage.
  - Identity stamps win over a colliding annotation, flat and nested.
  - **Determinism:** N≥1000 repeated `buildIdentityMeta` calls over
    `{"vendor.tag": "blue", "vendor.region": "eu", "fleet": "west"}` marshal to
    byte-identical JSON. Mutation check: the test must FAIL if the merge is made
    order-sensitive.
  - **Map-type identity:** after nesting an annotation and then injecting into the same
    namespace, assert every intermediate node's dynamic type is `map[string]any` (a
    direct `.(map[string]any)` assertion, mirroring `mcp.go:1535`) and that both the
    annotation leaf and the credential leaf are present. Mutation check: constructing an
    intermediate as `mcpsdk.Meta` must fail this test.
  - **Collision:** validation refuses `{"vendor": "x", "vendor.id": "y"}` and
    `{"vendor": "x"} + meta_key vendor.api_key`, at each of the four doors, with the
    offending keys in the message; `buildIdentityMeta` returns `ErrMetaPathCollision`
    for a legacy pair that no door saw.
  - **Over-redaction characterisation** (`internal/audit/rules_test.go`): `Redact` over
    `{"token": {"env": "prod", "api_key": "s"}}` collapses the whole `token` node to
    `audit.Placeholder`; over `{"token.env": "prod"}` it does not — proving coverage is
    preserved and the delta is over-redaction.
  - Depth-cap parity: boot config and `mcp.Config.validate()` now refuse the same
    over-deep `injection.meta_key` the wire door already refuses
    (`wireinjectiondescriptor_test.go:437`'s 17-segment fixture, reused).
- **Integration:** `test/integration/mcp_meta_nesting_test.go` — real drivers on the
  seam (`audit/drivers/patterns`, `events/drivers/inmem`, `state/drivers/inmem`), a
  spec-derived MCP fixture server (§17.8, `_meta` shape taken from
  `github.com/modelcontextprotocol/go-sdk@v1.6.1` `mcp.Meta`, not hand-authored)
  receiving a call carrying both an injected credential and a nested annotation in ONE
  namespace; assert the server observes the exact nested JSON shape and that both leaves
  survive. Identity propagation asserted through every layer (two tenants, no
  cross-talk). Failure modes (≥1 required, three provided): an over-deep annotation
  refused at the door; a colliding declaration refused; a legacy colliding revision
  failing the call with `ErrMetaPathCollision`.
- **Conformance:** N/A — no multi-driver interface changes.
- **Concurrency / leak:** **The draft's named hazard is unreachable and its test would
  have been INERT — replaced.** Verified: `p.cfg.MetaAnnotations` is `map[string]string`
  (`mcp.go:194`) and cannot hold nested maps, and `buildIdentityMeta` allocates a fresh
  `meta` per call (`mcp.go:1388`), so an N≥128 `-race` test asserting "the source
  annotation map is unmodified" would pass trivially against any implementation and
  prove nothing. What ships instead:
  - **N≥128 concurrent `buildIdentityMeta` calls against one shared `*Provider` under
    `-race`**, asserting (a) every call's marshalled `_meta` is byte-identical — the
    determinism property, which under concurrency is where randomised iteration actually
    bites; (b) each call's `_meta` is a distinct object graph, i.e. mutating one call's
    nested node is invisible to another's (the D-025 no-shared-structure property, which
    IS reachable because `injectMeta` allocates intermediates); (c) each call's identity
    stamps match ITS OWN ctx triple across N concurrent distinct identities (no context
    bleed).
  - Goroutine baseline unchanged before/after (`runtime.NumGoroutine`).

## Smoke script additions

`scripts/smoke/phase-217.sh` exists as a skeleton (currently a single `skip`). Its
`# PREFLIGHT_REQUIRES:` header becomes `live-server`. Assertions, all wrapped in the
404/405/501 → SKIP convention via `skip_if_404`:

- Attach a connection with a dotted annotation; drive a call against the fixture server;
  assert the observed `_meta` is nested (`assert_json_path`).
- Assert a flat annotation is unchanged.
- Assert a reserved FIRST SEGMENT (`tenant.foo`) is refused at the attach door — the
  newly-tightened arm.
- Assert a WHOLE-KEY spec-reserved annotation (`io.modelcontextprotocol/ui`) is still
  refused — the regression guard for the arm a per-segment-only rule would have lost.
  **This is the mutation-verified assertion:** reverting the whole-key arm turns this
  `OK` into a `FAIL`.
- Assert a colliding declaration (`vendor` + `vendor.id`) is refused, naming both keys.
- Assert an over-deep annotation key is refused.

## Coverage target

- `internal/tools/drivers/mcp`: 85% (no regression)
- `internal/config`: **84.0% as built, no regression.** *(Corrected by the
  wave-v1.24 §17.5 checkpoint audit. This row read "90% (no regression)", a
  number the package has never carried and which contradicted its two sibling
  plans — `phase-213-heavy-threshold-rebalance.md:257` and
  `phase-214-mcp-pass-by-reference-egress.md:703` both measure the same package
  at 82.9%. "No regression" is the real bar this phase set; the 90% was a
  target nobody measured, so it read as a coverage gate that had silently
  failed. Measured with `go test -count=1 -cover ./internal/config/...` on the
  merged wave: 84.0% — above both siblings' 82.9% baseline, because this phase
  added the `_meta` path validators with their tests.)*
- `internal/runtime/agentcfg/protocol`: 85% (as built: 85.5%)
- `internal/audit`: no regression (test-only additions)

## Dependencies

- 206, 211 — the owner-scoped connection mutators whose attach door this validation
  joins (`docs/plans/phase-206-owner-scoped-registry.md`,
  `docs/plans/phase-211-owner-scoped-registry-mutators.md`).

## Risks / open questions

- **The guard rule is the phase's highest-risk line.** "Per-segment RATHER THAN
  whole-key" loosens security; "per-segment AND whole-key" tightens it. The four shipped
  tests named above are the mechanical trip-wire, and the smoke's whole-key assertion is
  the mutation gate. An implementor who reads only the goals section and not this
  paragraph will get it backwards — which is why the rule is stated three times.
- **Nesting is a silent semantic change to a shipped, validated wire surface.** A
  persisted revision carrying `{"vendor.tag": "blue"}` (the exact shape at
  `setrevision_connections_test.go:274`) changes wire meaning on the next call after
  upgrade, with no config edit and no error. This is the §10 cost the phase pays
  deliberately; it is paid down by the CHANGELOG / CONFIG.md / glossary / example
  announcements, not by a code guard.
- **Over-redaction is a real regression in audit usefulness** for any operator who
  nests an annotation under a credential-named node. It is latent today (nothing audits
  `_meta` — verified) and becomes observable when something does. Accepted, documented,
  and pinned by test rather than fixed, because fixing it means changing
  `walkRedactKeys`' replace-on-match semantics for every rule.
- **Two mechanisms sharing one helper is the point, and it must not become two helpers
  again.** If `injectMeta`'s signature does not fit the annotation path directly, the
  correct move is to adapt the call site, not to fork the helper (§13). The
  `mcpsdk.Meta` vs `map[string]any` type-assertion trap at `mcp.go:1535` is exactly how
  a fork would announce itself — silently, by wiping siblings — so the intermediate-type
  assertion is an acceptance criterion, not a nit.
- **Open question for the coordinator, not for this phase:** 214 also edits
  `validate.go`, `mcp.go`, and `addconnection.go` with no dependency edge to 217. Both
  add a new rule at the same attach door. Sequencing is a wave-level decision.

## Glossary additions

- **Annotation path** — a dot-separated `meta_annotations` key, interpreted as a nested
  `_meta` location rather than a literal key. Shares its interpretation and its helper
  with `injection.meta_key`, so one namespace has one meaning regardless of which
  mechanism wrote into it. Declared paths on one connection may not collide (equal or
  proper-prefix), which makes the merge order-independent.
- **Meta annotations** — the existing entry at `docs/glossary.md:673` is AMENDED, not
  added: "merged verbatim" becomes "merged as annotation paths", and the reserved-key
  sentence gains the per-segment arm.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (N concurrent
      distinct identities, each call's `_meta` carrying its OWN triple)
- [x] Concurrent-reuse test passes — N≥128 concurrent `buildIdentityMeta` calls against
      one shared `*Provider` under `-race`, asserting determinism, distinct object
      graphs, and no identity bleed. **Not** the draft's inert "source map unmodified"
      assertion.
- [x] Integration test wires real drivers and a spec-derived MCP fixture (§17.8),
      asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [x] The §10 survey (including `setrevision_connections_test.go:274`) and the migration
      path are recorded in D-362
- [x] The four shipped spec-prefix tests pass with NO edits
- [x] `docs/CONFIG.md`, `docs/glossary.md`, `examples/dev.yaml`, and the `surface: tools`
      skill are updated in this PR (§18)
- [x] `docs/plans/README.md:360` detail block corrected and status flipped (§4.2 item 11)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
