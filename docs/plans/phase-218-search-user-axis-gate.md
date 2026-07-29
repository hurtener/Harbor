# Phase 218 — the search cluster's user axis becomes a scoped boundary

## Summary

`internal/search` gates its four searchers on the TENANT axis alone and passes `req.Filter.UserIDs` through to storage unexamined, so a same-tenant caller with no admin-tier claim reads another user's sessions, tasks, events and artifacts. This phase gives the user axis the shape the tenant axis already has — an effective-set computation plus the same elevation predicate — using the settled per-axis posture D-294 / D-299 / D-356 established for `events.list` and `artifacts.list` rather than a fifth dialect. The session axis stays a wildcard within a user, deliberately. Zero-wire: no method, error code, event or wire type moves.

## RFC anchor

- RFC §4 — the identity & isolation contract. The triple is the boundary; a read surface that filters on one component of it filters on a third of it.
- RFC §4.2 — mandatory identity. The runtime fails closed; an elided component is not an invitation to fan out.
- RFC §5.2 — what the Protocol exposes. `search.*` is one of the exposed read surfaces, so its scoping rule is a Protocol-visible contract even though this phase changes no wire type.
- RFC §5.5 — authentication. The claim that widens a search is the one the transport verified, never a body field.
- RFC §6.10 — artifacts. The artifact searcher reads `ArtifactStore.List`, whose wildcard semantics are the mechanism of one of the four leaks.
- RFC §6.13 — the typed event bus. The event searcher builds an `events.Filter` by hand, bypassing the shared `FilterFromWire` helper that already holds the cross-user gate.
- RFC §7 — the Console as a Protocol client. The single shipped consumer of this surface is the Console's ⌘K palette, and its behaviour changes here.

## Briefs informing this phase

- brief 05 — state, tasks, artifacts, sessions.
- brief 06 — events, observability, devx.
- brief 11 — Console feature surface.

## Brief findings incorporated

- **brief 06 §4 ("Isolation-triple filtering by default"):** *"Subscribe ignores any filter that elides `TenantID`/`UserID`/`SessionID` unless the caller has `admin` scope. Cross-tenant subscriptions are an explicit, audited operation."* The load-bearing half for this phase is the FIRST sentence, and it is the half the search cluster never implemented. The brief's rule is about ELISION, not about a named foreign value — an elided `UserID` is to be ignored (i.e. folded), not honoured as "every user." Three of the four searchers do the opposite today.
- **brief 05 §"Artifact dedup and content addressing":** *"Listing by scope treats `nil` fields as wildcards."* This is the exact mechanism of the artifact leak, and it is correct AT THE STORE — `ArtifactScope.ValidateFilter` (`internal/artifacts/artifacts.go:107-113`) states the wildcard deliberately and D-352 part 3 re-affirmed it against a stricter proposal. The wildcard is a store property; deciding what an elided component MEANS is the calling surface's job. This phase supplies that decision for `internal/search/artifacts`, exactly as D-356 supplied it for `artifacts.list`.
- **brief 05 §"Testing"** (*"N concurrent sessions × M concurrent tasks each, asserting no cross-talk in events, memory, artifacts, or task results"*) and (*"Cross-tenant isolation. Storing an artifact under tenant A and attempting to read under tenant B fails. Same for tasks, sessions, memory, trajectories."*). Both are adopted with the user axis added: the isolation test runs two users under ONE tenant as well as two tenants, because a test that only crosses tenants passes today against code that leaks across users — which is precisely how this survived from phase 72c to now.
- **brief 06 §"Testing"** (*"Cross-tenant isolation tests: subscriber for tenant A receives zero events emitted by tenant B; `admin` scope can bypass; assertion on the audit event for the bypass."*). The three-part shape — refusal, bypass-under-claim, and evidence of the bypass — is adopted per axis. The third part is where this phase is deliberately thinner than the brief; see "Findings I'm departing from."
- **brief 11 §"Top bar — global search + status":** *"Global search (⌘K) across sessions / events / artifacts / tools / agents / flows."* The brief describes an operator's own palette, not a fleet view. `web/console/src/lib/components/ui/GlobalSearch.svelte:89` sends `{query, page_size: 8}` and no filter, so the intended experience and the fixed behaviour agree; it is the CURRENT behaviour (tenant-wide rows for an ordinary user) that the brief never asked for.

## Findings I'm departing from (if any)

**One, named rather than absorbed.** brief 06's testing finding asks for *"assertion on the audit event for the bypass"*, and D-349 made the permission and the accountability structurally inseparable at the Protocol edge — a policy that permits a crossing must be handed a non-nil `Auditor`. This phase gates the user axis but emits **no** `audit.admin_scope_used` event for a granted cross-user search, because the tenant axis in this same package does not emit one either: `search.Deps` carries a `Redactor` and a `ScopeChecker` and no audit sink (`internal/search/search.go:143-150`), and the four searchers are constructed below the Protocol edge where D-349's gate runs. Adding a sink for the user axis alone would leave one package with an audited widening on one axis and an unaudited widening on the other — the §13 two-postures-for-one-concept shape this phase exists to avoid. The honest disposition: the cluster's audit gap is **pre-existing, axis-symmetric, and is not made worse here**, and closing it is a `search.Deps` change touching all four searchers plus their construction sites. It is recorded in "Risks / open questions" with the precision §17.6 demands of a deferral, not waved at.

No departure from the RFC, and none from D-294 / D-299 / D-308 / D-356 — this phase is their fourth application, not a new decision about elevation.

## Goals

- A caller with no admin-tier claim reads only their own rows from every one of the four searchers and from the aggregate dispatcher, under both leak shapes: an elided `user_ids` and a `user_ids` naming somebody else.
- The user axis is gated by the SAME predicate, the SAME effective-set shape and the SAME package as the tenant axis — one mechanism parameterised by axis, never two.
- A legitimately cross-user read (a fleet operator, an admin surface) stays possible under `auth.ScopeAdmin` or `auth.ScopeConsoleFleet`, on the same claim that already widens the tenant axis and the same claim `events.list` / `artifacts.list` already accept.
- The session axis is NOT gated on a single foreign value, so the everyday "search my own other sessions" flow keeps working.
- The behaviour change to the shipped Console palette is stated, tested and recorded — not discovered by an operator.
- Every guard is mutation-verified to turn a smoke `OK` into a `FAIL`, never a `SKIP`.

## Non-goals

- **A new scope.** D-108 closed this for the search cluster explicitly: cross-tenant gating reuses the D-079 closed two-scope set, and *"Minting a per-subsystem scope per cross-tenant call site would re-fragment the auth surface into N entitlements where one suffices."* A `search.crossuser` scope would be the same mistake one axis over.
- **A new `search.Deps` field.** The predicate the user axis needs is the predicate `Deps.AdminScope` already carries: `server.SearchAdminScopeFromAuth` (`internal/server/search_scope.go:38-41`) is `ScopeAdmin || ScopeConsoleFleet`, which is exactly the admin-tier set D-356 named for `artifacts.list`. No construction site changes.
- **Auditing the granted crossing.** See "Findings I'm departing from" and "Risks".
- **Re-litigating the store-level wildcard.** `ArtifactScope.ValidateFilter`'s tenant-only precondition and `SessionListFilter`'s empty-set-allows-all are correct at their layer (D-352 part 3, `internal/sessions/registry.go:942-960`). This phase changes what the SEARCHER asks for, never what the store permits.
- **Gating the session axis on a foreign single value.** D-299 and D-356 part 2 both settled this; re-deciding it here would break the Console Sessions / Playground history flow for nothing.
- **The `RunIDs` axis.** `types.SearchFilter` carries no run axis; `search.events` reads a run id from the `events.run` facet and it is a narrowing within an already-scoped triple.
- **Any wire change.** No method, error code, canonical event, or wire type moves; `ProtocolVersion` holds at `0.1.0`; no D-223 / D-209 regeneration.

## Acceptance criteria

- [ ] `search.CrossUserRequested(callerUser, req)` and `search.EffectiveUserSet(callerUser, req)` exist beside their tenant twins in `internal/search/search.go`, with the same signature shape and the same pure-read contract.
- [ ] `CrossUserRequested` is true when `Filter.UserIDs` names any user other than the caller AND when `len(Filter.UserIDs) > 1`; it is false for an empty filter and for a filter naming exactly the caller. The `len>1` trigger is the D-299 multi-value fan-in rule, and it fires even when every named user is the caller repeated.
- [ ] `EffectiveUserSet` returns `{callerUser}` for an empty filter — the FOLD, not a wildcard — and the named set (deduplicated, sorted) otherwise, mirroring `EffectiveTenantSet` (`internal/search/search.go:286-308`) including its empty-string skip and its fall-back to the caller when every entry was empty.
- [ ] All four searchers and the aggregate dispatcher refuse a cross-user request with `search.ErrCrossUserRequiresAdmin` when `Deps.AdminScope(ctx)` is false, at the same call site and in the same order as the existing cross-tenant refusal.
- [ ] `internal/search/sessions/index.go` passes `EffectiveUserSet(...)` to `SessionListFilter.UserIDs` instead of `req.Filter.UserIDs`.
- [ ] `internal/search/tasks/index.go` does the same, and `rowScopedCtx` (`:191-204`) gains the user axis: a session whose `(tenant, user)` differs from the verified caller's on EITHER component takes the admin-tier re-check, where today only a tenant difference does.
- [ ] `internal/search/artifacts/index.go` iterates the effective USER set as it iterates the effective tenant set, instead of reading `req.Filter.UserIDs[0]` and leaving `ArtifactScope.UserID` empty when the filter is absent. An elided user no longer reaches `ArtifactStore.List` as a wildcard.
- [ ] `internal/search/events/index.go` derives `scopeUser` from `EffectiveUserSet` rather than from `req.Filter.UserIDs[0]`, and `Filter.Admin` is set when EITHER axis widened — today it is `crossTenant` alone (`:89`), so a granted cross-user read would be handed to the bus with `Admin: false` and silently return nothing.
- [ ] A caller naming exactly their own user id is indistinguishable from a caller naming nobody: no claim required, same rows.
- [ ] A caller naming one of their OWN other sessions still needs no claim; only a multi-value session set elevates, via the same fan-in rule.
- [ ] Under `auth.ScopeAdmin` or `auth.ScopeConsoleFleet`, both widenings pass through untouched: a named foreign user reads that user, and an ELIDED user fans across the tenant rather than folding — the D-308 rule that a widened read does not fold its elided axes.
- [ ] `mapSearchError` maps `ErrCrossUserRequiresAdmin` to `CodeScopeMismatch`, the code the tenant axis already publishes on this surface.
- [ ] `types.SearchFilter.UserIDs`' godoc stops claiming "empty defaults to the caller's authenticated user" as a description of current behaviour and states the actual rule; `types.SearchFilter`'s struct godoc names the user axis alongside the tenant axis.
- [ ] `internal/search/artifacts/index.go:98` binds the `heavy` bool its three siblings bind, and the row shape is made consistent with them.
- [ ] `test/integration/phase218_search_user_axis_test.go` runs real drivers, asserts isolation across users within one tenant AND across tenants, covers ≥1 failure mode, and runs under `-race`.
- [ ] A concurrent-reuse test runs N≥128 concurrent searches by two different users against ONE shared Searcher per index under `-race`, asserting every returned row's `UserID` equals the requesting caller's.
- [ ] `scripts/smoke/phase-218.sh` shows `OK ≥ 10`, `SKIP = 0`, `FAIL = 0` against a live preflight build, and FAILS (never SKIPs) when any guard is removed.
- [ ] `ProtocolVersion` unchanged; `make protocol-ts-gen-check` and `make protocol-docs-gen-check` regenerate to a clean diff.

## Files added or changed

```text
internal/search/
  search.go                                  # CrossUserRequested + EffectiveUserSet;
                                             #   ErrCrossUserRequiresAdmin; package godoc
  aggregate.go                               # the aggregate-edge user gate + the hard-error set
  search_user_axis_test.go                   # NEW — the helpers' whole contract, table-driven
  concurrent_reuse_test.go                   # two-principal N=128 arm
  artifacts/index.go                         # effective user set; the bound `heavy`
  events/index.go                            # effective user set; Admin on either widening
  sessions/index.go                          # effective user set
  tasks/index.go                             # effective user set; rowScopedCtx user axis
  {artifacts,events,sessions,tasks}/index_test.go   # per-searcher cross-user arms
internal/protocol/
  search.go                                  # mapSearchError: ErrCrossUserRequiresAdmin
  search_test.go                             # the wire code for the new refusal
internal/protocol/types/
  search.go                                  # SearchFilter godoc — the user axis stated
test/integration/
  phase218_search_user_axis_test.go          # NEW
scripts/smoke/phase-218.sh                   # NEW
docs/plans/phase-218-search-user-axis-gate.md  # NEW
docs/plans/README.md, docs/decisions.md      # the master-plan row; D-363
docs/glossary.md                             # the two new terms
docs/site/protocol/auth-and-identity.md      # §18 — the search rows' crossing policy
web/console/src/lib/sessions/types.ts        # a stale comment corrected (see below)
```

No new package, no new top-level directory, no Console behaviour change.

## Public API surface

```go
// internal/search

// ErrCrossUserRequiresAdmin — the request's filter expands the query
// outside the caller's own user, either by naming another user or by
// naming more than one. Maps to CodeScopeMismatch, the same code the
// tenant axis publishes: one widening, one refusal vocabulary.
var ErrCrossUserRequiresAdmin = errors.New(
    "search: cross-user search requires the auth.ScopeAdmin or auth.ScopeConsoleFleet claim")

// CrossUserRequested reports whether the request's filter targets users
// OTHER than the caller. Pure read; the caller gates on the admin-scope
// predicate. Empty -> false. Exactly the caller -> false. Any other name,
// or len>1, -> true (D-299's cross-principal OR fan-in trigger).
func CrossUserRequested(callerUser string, req types.SearchRequest) bool

// EffectiveUserSet returns the users the request is scoped to AFTER
// gating. Empty filter -> {callerUser} (the FOLD — an elided user is the
// caller's own, never a wildcard). Named -> those users, deduplicated and
// sorted. Mirrors EffectiveTenantSet field for field.
func EffectiveUserSet(callerUser string, req types.SearchRequest) []string
```

`Searcher`, `Deps`, `SearcherRegistry`, `Query`, and every constructor signature are **unchanged**. The predicate that grants the widening is the `Deps.AdminScope` the searchers already hold.

## The defect, established by grep and by execution

**The tenant axis already folds; the user axis wildcards.** `EffectiveTenantSet` (`internal/search/search.go:286-289`) returns `{callerTenant}` on an empty filter. There is no `EffectiveUserSet`, and `grep -rn "CrossUserRequested\|EffectiveUserSet" internal/` returns nothing. So "mirror the tenant axis" is not an analogy here — it is literally the missing half of a symmetric pair.

Per searcher, precisely, and **they are not equally broken**:

| searcher | elided `user_ids` | named foreign `user_ids` | mechanism |
|---|---|---|---|
| `sessions/index.go:79` | **LEAKS** — every user in the tenant | **LEAKS** | `SessionListFilter.UserIDs` empty ⇒ `newStringSet(nil).allow(x)` is true for all x (`internal/sessions/registry.go:944,956`) |
| `tasks/index.go:77` | **LEAKS** — same lister, then walks every returned session's tasks | **LEAKS** | as above; `rowScopedCtx:193` compares TENANT only, so a same-tenant foreign-user session takes the unelevated `identity.With` path at `:194` |
| `artifacts/index.go:73-79` | **LEAKS** — `ArtifactScope{TenantID: tenant}` with `UserID` unset | **LEAKS** | `ValidateFilter` requires only the tenant and documents the empty user as a wildcard (`internal/artifacts/artifacts.go:107-113`) |
| `events/index.go:69-70,78-79` | **safe** — `scopeUser` defaults to `callerID.UserID` at `:70` | **LEAKS** | `:78-79` overwrites the default with `req.Filter.UserIDs[0]` and `:89` still passes `Admin: crossTenant` (false), so the bus honours the foreign user as an ordinary scoped read |

So the surface has **two** distinct defects, not one: an elision defect on three searchers and a named-value defect on all four. A fix that only examined `UserIDs` when populated — the shape the phrase "passes `UserIDs` through unexamined" suggests — would close the named-value defect and leave the elision defect, which is the wider of the two: it fires on the DEFAULT request, with no attacker input at all.

**Executed, not inferred.** A throwaway test against the real `sessions.Registry` and in-memory `StateStore`, two users in one tenant, `AdminScope` returning false throughout:

```text
(A) default, no filter        -> 2 rows
    row user="attacker" session="attacker-sess"
    row user="victim"   session="victim-sess"     <- another user's row, no claim
(B) filter user_ids=[victim]  -> 1 rows
    row user="victim"   session="victim-sess"     <- targeted, no claim
```

Both leak shapes reproduce. The preview string is `"session victim-sess (open) tenant=t1 user=victim"` — the disclosure carries the victim's user id and session id, which are isolation-principal identifiers, and §6 forbids resting on their unguessability. The implementor should re-run this reproduction first and keep it as the shape of the per-searcher elision test.

**This is a recorded, queued follow-up, not a discovery.** D-356 ("Follow-up", `docs/decisions.md:10164`) names the defect with the same four file:line citations this plan uses and says: *"Making the user axis a scoped boundary there is one shared helper plus four call sites plus the aggregate dispatcher, and it should be decided for the subsystem in one pass rather than for one searcher inside a checkpoint PR — a single searcher changed alone would be the §13 two-postures-for-one-concept shape. It is queued as the next wave's first item."* This phase is that pass. The site count holds; the aggregate dispatcher is the fifth site and is where the refusal must fire first, because `Query` rewrites the sub-request (`aggregate.go:76-85`) and a per-index-only gate would be five chances to forget instead of one.

## The mechanism, and what was rejected

**Mirror, don't invent (§13).** Four candidate shapes were considered:

1. **Route `search.events` through `events.FilterFromWire`.** That helper (`internal/events/filter.go:110-124`) already holds the exact gate — *"a single FOREIGN user is a cross-user read that requires the elevated scope (mirroring the tenant axis above)"* — and D-294 records the identical bug being fixed there. **Rejected as the general answer**: it fixes one of four searchers and imports the event subsystem's wire-conversion vocabulary into three packages that have no events in them. Adopted as the *semantic* source: `CrossUserRequested` / `EffectiveUserSet` reproduce `FilterFromWire`'s three-branch switch (`0` folds, `1` gates on inequality, `default` gates) so the two helpers agree by construction and a reviewer can diff them.
2. **Gate at the Protocol edge in `internal/protocol/search.go::Dispatch`.** Rejected: `Dispatch` (`:60-103`) never inspects `Filter`, and the searchers are also reachable in-process by an embedder. The tenant gate lives in the searchers; putting the user gate one layer up would mean the two axes are enforced at different layers, which is the drift shape that produced this bug — phase 72c enforced one axis where it was looking and never enumerated the others.
3. **A new `Deps.UserScope ScopeChecker`.** Rejected: `SearchAdminScopeFromAuth` is already `ScopeAdmin || ScopeConsoleFleet` (`internal/server/search_scope.go:38-41`), which is exactly the admin-tier set D-356 part 1 named for the identical decision on `artifacts.list`. A second predicate would be a second entitlement in all but name, against D-108's closed-scope finding, and would change every construction site for no behaviour.
4. **Reuse `ErrCrossTenantRequiresAdmin` for both axes.** Rejected: a caller who gets a refusal deserves to know which axis refused them, and `errors.Is` branching is the repo's idiom. A distinct sentinel mapped to the SAME wire code gives operators the message without giving clients a new code to branch on.

**On the refusal code, an inconsistency stated rather than smoothed over.** D-356 chose `403 identity_scope_required` for `artifacts.list`, deliberately reusing `events.list`'s vocabulary. This phase chooses `CodeScopeMismatch` — because that is what `search.*`'s *tenant* axis already publishes (`internal/protocol/search.go:134-145`), and one surface answering two codes for the same class of refusal is worse than two surfaces answering different codes. Changing the tenant axis to match D-356 would be a wire-visible break to a shipped surface, which this phase has no mandate to make. The cross-surface divergence is real and is recorded in "Risks" rather than silently inherited.

**Why the session axis stays open.** D-299: *"a SINGLE own-session read — the caller's current session OR one of their own other sessions — needs no elevation."* D-356 part 2 gives the reason that applies here verbatim: *"the user gate above it already decides whose rows are in play."* Once the user axis folds, a foreign session resolves to nothing under the folded filter, so gating it buys nothing and breaks the Playground history flow.

## Legitimate cross-user callers — surveyed, not assumed

**No production caller sets `Filter.UserIDs`.** Every `types.SearchFilter{...}` literal in the module is in a `_test.go` file and every one sets `TenantIDs` only (15 sites; `internal/protocol/search_test.go:101`, `internal/search/aggregate_test.go:92,107`, the four `index_test.go` files, `test/integration/search_cluster_test.go:291,325`, `test/integration/verified_anchor_search_test.go:58`, plus three `reflect.TypeOf` registry entries in the codegen binaries). The only place a search request is built from untrusted input is `internal/protocol/transports/control/search_handler.go:40-47`, a bare `json.Unmarshal` into `types.SearchRequest` with no filter sanitisation between the wire and the searchers.

**One Console caller exists, and it sends no filter.** `web/console/src/lib/protocol/client.ts:1507-1514` exposes `search.query` and nothing else — `search.sessions` / `.tasks` / `.events` / `.artifacts` have no typed client method and no Console caller at all. The sole call site is the ⌘K palette, `web/console/src/lib/components/ui/GlobalSearch.svelte:89`, sending `{query, page_size: 8}`. The wire type at `web/console/src/lib/protocol/search.ts:15-26` declares `user_ids?: string[]`; nothing populates it.

**The two Console `user_ids` setters feed different methods** and are unaffected: `web/console/src/lib/events/filters.ts:72` builds an `EventFilter` for `events.subscribe` / `events.aggregate` / `events.list`; `web/console/src/lib/components/sessions/SessionFacetChips.svelte:105-110` builds a `SessionFilter` for `sessions.list`, consumed at `web/console/src/routes/(console)/sessions/+page.svelte:481`. Both already run under their own D-294 / D-299 gates. **Note a stale comment**: `web/console/src/lib/sessions/types.ts:55` claims the session filter is *"forwarded to `search.sessions`"*; it is not — `internal/sessions/` has zero imports of `internal/search` and `sessions.list` handles its own query field. Correcting that comment is in scope for this PR, because it is precisely the sentence that would send the next auditor of this question down the wrong path.

**So no shipped admin or fleet Console page reads this surface, and the fix breaks no page.** That is a genuinely fortunate finding and it is the reason this can ship as a straight tightening rather than as a claim-plumbing exercise. It is also, read the other way, the reason the bug survived: the surface has one consumer that never exercised the leaking parameter, so no test and no page ever looked at the user axis.

**The behaviour change that IS visible, stated plainly.** For an ordinary user with no admin-tier claim, the ⌘K palette today returns session, task and artifact rows belonging to **every user in the tenant**; after this phase it returns only their own. That is the security fix and it is a user-visible change to a shipped page. It requires no Console code change and no claim — the palette sends no filter, so it lands on the folded path. A deployment that was relying on the palette as an unofficial fleet view will notice; that reliance was on a disclosure bug and there is no compatibility flag for it (§13 forbids identity-downgrading knobs).

**Grep contract to keep this true.** `internal/search/*/index.go` must contain no `req.Filter.UserIDs` reference after this phase; the effective-set helper is the only reader. The smoke asserts this mechanically.

## Interaction with the artifacts list contract (D-352 / D-356)

This is the constraint that makes "just add a `WHERE` clause" wrong, so it is stated before the fix rather than discovered during it.

`ArtifactScope.ValidateFilter` (`internal/artifacts/artifacts.go:107-113`) requires **only** the tenant, and its godoc says why: *"a list filter is a predicate over a result set rather than an identity, so an empty `UserID` / `SessionID` / `TaskID` stays a wildcard."* D-352 part 3 (`docs/decisions.md:9893-9895`) reached that deliberately and recorded the departure from D-347's stricter proposal, naming `internal/search/artifacts/index.go` as one of the two live surfaces a full-triple precondition would break: *"Requiring the full triple would silently turn both into refusals: the Console's artifacts page and the artifact search index would answer nothing for a tenant-scoped query."*

D-356 then decided the same question one layer up for `artifacts.list` and reached the answer this phase adopts — *"`user` is an isolation principal, so the listing folds to the caller's own"*, with an elided user folded rather than fanned, both widenings taking the same admin-tier claim, and the session axis left a wildcard. D-356's blast-radius note is explicit that it did NOT reach here: *"`internal/search/artifacts` does NOT route through this surface; it calls `ArtifactStore.List` directly and is unaffected by this change."*

**Three consequences bind this phase:**

1. **The fix must not touch `ArtifactStore.List` or its precondition.** Narrowing the store would re-open the exact Protocol behaviour change D-352 refused, and would break `artifacts.list`'s published contract (`types.ArtifactsListRequest.Scope`) for every Protocol client, not only search. The searcher changes what it ASKS for; the store keeps permitting what it permits.
2. **The searcher must ITERATE the effective user set, not narrow a single scope.** The current code reads `req.Filter.UserIDs[0]` (`:74-76`) — a one-element read that silently drops users 2..N. A folded set is one element, but a widened admin set is N, and D-308 says a widened read fans in rather than folding. The fix therefore nests a user loop inside the existing tenant loop (`:69`), keeping the store call shape identical and calling it once per `(tenant, user)` pair.
3. **The widened path must still be able to express "every user in this tenant."** Under an admin-tier claim with an elided user, `EffectiveUserSet` must NOT fold — the searcher passes an empty `ArtifactScope.UserID` and lets the store's wildcard do its job, which is the one case where the D-352 wildcard is the correct answer and not a leak. This is why the elevation check is read at the same site as the effective-set computation rather than the set being computed blind.

The same asymmetry governs `sessions` and `tasks` via `SessionListFilter`, whose empty-set-allows-all is the identical deliberate wildcard one layer down (`internal/sessions/registry.go:942-960`), and whose hydration loop at `:917-930` already iterates `TenantIDs × UserIDs` — so a folded user set makes that loop *more* effective, not less. `search.events` is the exception: its filter is single-valued per call by design (`internal/search/events/index.go:72-77`), so a multi-user widened read there returns the first user's rows only. That pre-existing single-value limitation is unchanged by this phase and is recorded in "Risks".

## The sibling defect at `artifacts/index.go:98` — verified, and smaller than reported

`internal/search/artifacts/index.go:98` reads `out, _, rerr := search.RedactAndCapPreview(...)`, discarding the `heavy` bool that its three siblings bind (`events/index.go:136`, `sessions/index.go:124`, `tasks/index.go:127`). **The discard is real. The consequence commonly attributed to it is not**, and the plan records the verification rather than the assumption:

- `RedactAndCapPreview` returns `("", true, nil)` when over threshold (`internal/search/search.go:423-425`) — the empty string is returned WITH the flag, so a discarded `heavy` cannot leak bytes.
- The artifact searcher populates `Ref` **unconditionally** at `:109-116`, from real `ArtifactRef` metadata, because artifacts are by-reference by construction (stated in the package godoc, `:6-8`). So the claim that this index "never populates the ref" is **false**; it is the one index that always does.
- The reachable consequence is therefore narrow: when a preview exceeds the threshold the row ships `Preview: ""` with a correct `Ref`, which is the right wire shape arrived at by accident. And the preview is `fmt.Sprintf("artifact %s mime=%s size=%d filename=%s", ...)` over an id, a mime type, an int and a filename — reaching the threshold takes a pathological filename.

**It is still fixed here, under §17.6**, for the reason that survives the correction: the discard means this call site cannot tell a capped preview from an empty one, it is the only one of four that differs, and the row shape is correct by coincidence rather than by construction — so a later change to `Ref` population (phase 213 is editing this constant's neighbourhood; a future phase making `Ref` conditional is entirely plausible) would turn a cosmetic inconsistency into a silent-empty-preview bug with nothing to catch it. Binding the bool and making the row shape explicit costs three lines. **It is not a security fix and is not represented as one** — a plan that inflated it into one would be the kind of drift this repo's hygiene exists to prevent.

## Test plan

- **Unit:**
  - `TestCrossUserRequested_Table` — empty ⇒ false; exactly the caller ⇒ false; one foreign ⇒ true; `len>1` including `{caller, caller}` ⇒ true; empty-string entries skipped consistently with the tenant twin.
  - `TestEffectiveUserSet_FoldsEmptyToCaller` — the fold, asserted as a value, not as a row count.
  - `TestEffectiveUserSet_DeduplicatesAndSorts` / `TestEffectiveUserSet_AllEmptyFallsBackToCaller` — parity with `EffectiveTenantSet`.
  - `TestEffectiveUserSet_MirrorsEffectiveTenantSet` — the two helpers driven over one shared table so a future edit to one that is not made to the other fails.
  - Per searcher (×4): `TestXSearcher_ElidedUserFoldsToCaller` (the elision defect — seeds two users in ONE tenant and asserts every row's `UserID` is the caller's, never a row count); `TestXSearcher_NamedForeignUserRefused` (`ErrCrossUserRequiresAdmin`); `TestXSearcher_OwnUserNamedNeedsNoClaim`; `TestXSearcher_MultiUserFanInRefused`; `TestXSearcher_AdminClaimReopensBothWidenings` (named-foreign AND elided-fans-in, under each of the two claims).
  - `TestSessionsSearcher_OwnOtherSessionNeedsNoClaim` and `TestSessionsSearcher_MultiSessionFanInRefused` — the session axis stays open on a single value and elevates on a set.
  - `TestTasksSearcher_RowScopedCtx_ForeignUserSameTenantElevates` — called directly against `rowScopedCtx`, because the guard is otherwise reachable only through the fan-in and an untested guard is how an inert guard survives (the phase 211 idiom).
  - `TestEventsSearcher_WidenedReadSetsBusAdmin` — a granted cross-user read reaches the bus with `Admin: true`; without it the widened read returns empty, which would look like a working gate.
  - `TestArtifactsSearcher_IteratesEffectiveUserSet` — a widened two-user read returns both users' rows, pinning D-308's no-fold-when-widened rule at the one searcher whose loop shape changes.
  - `TestArtifactsSearcher_HeavyPreviewBindsFlag` — the `:98` fix, asserting the row shape rather than the discard.
  - `TestQuery_CrossUserRefusedAtAggregateEdge` — the refusal fires in `Query` before fan-out, and a per-index hard error propagates rather than degrading (`aggregate.go:129`).
  - `TestMapSearchError_CrossUserIsScopeMismatch` — the wire code.
- **Integration:** `test/integration/phase218_search_user_axis_test.go`. Real drivers on every seam (`state/drivers/inmem`, `events/drivers/inmem`, `audit/drivers/patterns`, the real `sessions.Registry`, the real `TaskRegistry`, a real `ArtifactStore`) — no fakes at the boundary. Seeds two tenants × two users × two sessions each, drives all five methods through `internal/protocol`'s dispatcher so identity flows from ctx through the surface into the searchers, and asserts: no row belongs to another user without a claim; no row belongs to another tenant without a claim; both claims reopen both axes; and the aggregate dispatcher agrees with each per-index searcher row-for-row. **Failure modes (≥1 required, three shipped):** a redactor forced to error (the row must not ship), a request naming a foreign user (`CodeScopeMismatch`, not an empty page — an empty page would be the D-311 false-absence shape), and a closed session registry (`ErrRegistryClosed` propagates rather than degrading to zero rows).
- **Conformance:** N/A — `search` has one implementation per index by design (package godoc, `internal/search/search.go:19-22`); there is no driver pluralism to run a conformance suite over. The `TestEffectiveUserSet_MirrorsEffectiveTenantSet` table is the axis-parity analogue.
- **Concurrency / leak:** `internal/search/concurrent_reuse_test.go` gains a two-principal arm — N=128 goroutines split between two users in one tenant, all against ONE shared Searcher per index and one shared registry, under `-race`, asserting (a) no data race, (b) every row returned to caller X has `UserID == X` (context bleed — the assertion that would have caught this bug in 72c), (c) cancelling one caller's ctx leaves the other's rows intact, (d) `runtime.NumGoroutine()` returns to baseline after teardown. The existing single-principal N≥100 stress stays.

## Smoke script additions

`scripts/smoke/phase-218.sh` (`PREFLIGHT_REQUIRES: live-server`), every assertion FAILing rather than SKIPping when its guard is removed:

- **static:** `internal/search/search.go` declares `CrossUserRequested`, `EffectiveUserSet` and `ErrCrossUserRequiresAdmin`.
- **static:** each of the four `index.go` files calls `CrossUserRequested` and `EffectiveUserSet`.
- **static:** `aggregate.go` gates the user axis at the aggregate edge.
- **static (the regression trip-wire):** `assert_grep_absent` for `req.Filter.UserIDs` across `internal/search/*/index.go` — the raw-filter read is the bug's fingerprint, so a re-introduction fails preflight rather than waiting for a reviewer.
- **static:** `internal/search/tasks/index.go`'s `rowScopedCtx` compares the user component, not the tenant alone.
- **static:** `internal/search/artifacts/index.go` binds the `heavy` bool (no `, _,` at the `RedactAndCapPreview` call).
- **static:** `internal/protocol/search.go` maps `ErrCrossUserRequiresAdmin`.
- **live:** the five `search.*` methods still answer 200 for an ordinary caller (the fix must not turn a working surface into a refusal — the D-352 failure mode).
- **live:** a `search.query` naming a foreign `user_ids` answers 403 with `scope_mismatch` for a token carrying no admin-tier claim — an exact code compare, never a 2xx-or-4xx range.
- **unit-tests:** `go test -race -count=1` over the five search packages plus `test/integration -run TestE2E_Phase218`.

**Mutation verification is an acceptance criterion, not a habit.** Each of these is run against the real tree with its guard reverted and the transition recorded in D-363: reverting the fold to `req.Filter.UserIDs` must turn `OK → FAIL` (never `→ SKIP`); deleting the refusal branch must fail its live assertion; and reverting `rowScopedCtx` to the tenant-only compare must fail its unit leg. §4.2 item 5 is the reason this is spelled out — a shipped phase's own guard reporting SKIP is how the 109k gap survived four phases.

## Coverage target

Measured on the current tree with `go test -cover ./internal/search/...`, so the targets are deltas from a real baseline rather than aspirations:

| package | current | target |
|---|---|---|
| `internal/search` | 59.3% | **80%** |
| `internal/search/artifacts` | 69.6% | **85%** |
| `internal/search/events` | 70.4% | **85%** |
| `internal/search/sessions` | 67.7% | **85%** |
| `internal/search/tasks` | 71.4% | **85%** |
| `internal/protocol` | unchanged | no regression |

The 80% floor on `internal/search` is deliberately below the 85% siblings: the package holds `Paginate`, `SortRowsByOccurredAtDesc` and `MatchesQuery`, whose branch tails this phase does not touch, and raising the number by testing unrelated helpers would be coverage theatre. **Note a drift signal for the wave**: `docs/plans/phase-213-heavy-threshold-rebalance.md:150` states an `internal/search: 85%` target against a package measuring 59.3%. Phase 213 lands first and touches this package; whichever phase ships second should reconcile the two rows rather than both quietly claiming a number.

## Dependencies

- **213** — heavy-content threshold rebalance. Phase 213 splits `HeavyPreviewThreshold` out of `config.DefaultHeavyOutputThresholdBytes` and edits `internal/search/search.go:70` plus that file's godoc; this phase adds functions and a sentinel to the same file. **213 is Stage 1 and lands first; this phase is Stage 2 and rebases on it.** The overlap is textual, not semantic — 213 owns the constant block (`:63-77`), this phase owns the sentinel block (`:80-102`) and appends beside `EffectiveTenantSet` (`:286-308`) — so the rebase is mechanical, but it is a rebase and not a merge, and the `search_test.go` assertions 213 retargets must be re-run after it.
- **72c** — the search cluster this phase repairs (D-108).
- **205** — the body-scope reconciler (D-349): supplies `identity.FromVerified` and `identity.WithElevated`, which `tasks/index.go::rowScopedCtx` already uses and which this phase extends to the user axis.
- **208** — the reconciled artifact read key (D-352): its `List` tenant-only precondition is the constraint this phase must not break.

**The wave-end E2E is NOT claimed here.** `test/integration/wave_v124_test.go` is claimed by `docs/plans/phase-216-run-start-connection-attach.md` (its "Files added or changed" and its checklist both name it). Phase 218 is the other Stage 2 phase, so if 216 slips or 218 is the last Stage 2 phase to merge, this phase inherits the wave E2E per the coordination file's "rides the last Stage-2 phase to merge" rule. Stated so the two Stage 2 plans do not both assume the other carries it — a wave E2E that each phase believes the other owns is the §17.1 wiring-gap shape one level up.

## Risks / open questions

1. **The Console palette's result set narrows for every non-admin operator.** Stated in "Legitimate cross-user callers" and pinned by an integration assertion. There is deliberately no compatibility flag: §13 forbids identity-downgrading knobs, and a knob restoring tenant-wide rows for a non-admin caller would be exactly that. **Mitigation:** the CHANGELOG entry names the behaviour change in operator-facing terms, and `docs/site/protocol/auth-and-identity.md` gains the search rows' crossing policy in the same PR (§18).
2. **The refusal code diverges from D-356's choice for the same class of refusal.** `search.*` answers `CodeScopeMismatch` (403); `artifacts.list` and `events.list` answer `identity_scope_required` (403). Both are 403 and the divergence is in the code string, not the status. Reconciling them means changing a shipped surface's published code and belongs to a Protocol-consistency phase with a deprecation window (RFC §5.3), not to a security fix. **Recorded, not resolved.**
3. **The granted cross-user crossing is not audited.** The full disposition is under "Findings I'm departing from". Concretely: closing it means adding an `Auditor` to `search.Deps`, threading it through every construction site, and emitting `audit.admin_scope_used` on BOTH axes — a change whose blast radius exceeds this phase and which would be wrong to do for one axis only. **The pre-existing tenant-axis gap is not widened here; the user axis simply joins it.** A follow-up issue is filed and linked from D-363.
4. **`search.events` remains single-valued on the user axis.** A widened admin read naming three users returns the first user's rows (`internal/search/events/index.go:72-79`, the pre-existing "V1 admits only a single-session-targeted replay per call" limitation extended to users by the same code shape). This phase makes the read CORRECT (folded when unwidened, refused when unentitled) but not COMPLETE (a widened multi-user read under-reports). Under-reporting a widened admin read is the safe direction of the two, but it is a false-absence shape (D-311) and is named here rather than left for an operator to find. Fixing it means fanning `Replay` per user, which is a performance question the events subsystem should answer, not the search cluster.
5. **`rowScopedCtx`'s elevation is file-granular in the D-349 minting allow-list.** `internal/search/tasks/index.go` is already registered (`internal/protocol/bodyscope/gate_test.go:55`), so widening its guard from tenant to `(tenant, user)` passes the minting scan silently — the scan authorises the FILE, not the call. That is correct for this change (the guard gets strictly tighter) but it means the scan would equally not notice a future LOOSENING at the same site. Out of scope; noted because this phase is the first to lean on that entry since it was written.
6. **The `search_handler.go` unmarshal remains unsanitised.** `internal/protocol/transports/control/search_handler.go:40-47` decodes the wire request whole with no filter validation, so this phase's gate is the FIRST place caller-supplied filter values are examined at all. That is the design (the gate lives with the tenant gate, per "what was rejected" item 2), but it means the searchers are load-bearing for request validation in a way the transport's godoc does not say. Worth a docs line; not worth a layer.
7. **Not resolved: whether an agent-shaped fan-in exists on this surface.** `types.SearchFilter` carries no agent axis, and CLAUDE.md §6's clarifying note says the agent identity is not an isolation principal, so there is nothing to gate. Recorded so the next auditor does not re-derive it.

## Glossary additions

- **Effective user set** — the users a search request resolves to after the user-axis gate: the caller's own when the filter elides the axis, the named set when an admin-tier claim permitted the widening. The user-axis twin of the effective tenant set.
- **Axis fold** — resolving an elided identity component on a read filter to the caller's own value rather than treating it as a wildcard. The counterpart of a widening; the two are decided per axis (D-299, D-356).

Both land in `docs/glossary.md` in the same PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — **and the cross-USER test under one tenant, which is this phase's whole point**
- [ ] Concurrent-reuse test passes — N=128 two-principal invocations against one shared Searcher per index under `-race`, asserting no data races, no context bleed (every row's `UserID` equals its requester's), no cancellation cross-talk, no goroutine leaks. See AGENTS.md §5 + §11 + D-025.
- [ ] Integration test exists (`test/integration/phase218_search_user_axis_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode (three shipped), runs under `-race`. See AGENTS.md §17.
- [ ] Every smoke guard mutation-verified `OK → FAIL` (never `→ SKIP`), transitions recorded in D-363
- [ ] Rebased on phase 213; `internal/search/search.go` merges cleanly and 213's retargeted `search_test.go` assertions re-run green
- [ ] `ProtocolVersion` unchanged; `make protocol-ts-gen-check` + `make protocol-docs-gen-check` clean
- [ ] Glossary updated with both new terms
- [ ] D-363 filed in `docs/decisions.md` (blank lines around `---` and the `## D-363` heading; `markdownlint-cli2` clean repo-wide)
- [ ] `docs/plans/README.md` row for 218 present and accurate
- [ ] §18: `docs/site/protocol/auth-and-identity.md` carries the search rows' crossing policy; no `docs/skills/` playbook demonstrates `search.*`, so no skill body changes (verified by grep, not assumed)
- [ ] Follow-up issue filed for the unaudited crossing (risk 3) and linked from D-363
