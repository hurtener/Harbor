# Phase 170 — Same-origin MCP OAuth-discovery dial (HA-19)

## Summary

Phase 164 (D-297) shipped the report-only MCP OAuth-requirement discovery
walker, but it can NEVER complete against an MCP server on localhost, a
container-compose network, or a private VPC — the ordinary self-hosted
posture. The walker enforces SSRF policy in two places that DISAGREE: the
per-hop policy (`validateHop`) correctly PERMITS the same-origin
protected-resource-metadata hop even to a private-address server, but the
dial-time backstop (`net.Dialer.Control` in `NewDiscoverer`) UNCONDITIONALLY
refuses every resolved private/loopback IP, with no same-origin exemption and
no knowledge of which hop it is serving. Discovery therefore dies on the FIRST
hop — the RFC 9728 fetch back to the very MCP server the runtime is already
dialing successfully for tool calls — so the feature's whole point (reaching
`needs_allowance` so an operator can grant the authorization-server origin) is
unreachable. This phase RELAXES that backstop for exactly ONE hop
(protected-resource), ONE origin (the connection's own operator-declared
server), pinned to the connection's actual dial target — while every
cross-origin hop keeps the full private-IP refusal AND the DNS-rebinding
defence. This is a SECURITY-SENSITIVE relaxation; it is planned with the
DNS-rebinding subtlety as the crux and every preserved property pinned by a
named test. No production `allowPrivate` knob is introduced; the test-only
`WithPrivateNetworkAccessForTest` escape hatch stays test-only.

## RFC anchor

- RFC §6.4
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- brief 09 §"What to lift from bifrost (concrete)" item 3 (the
  OAuth-discovery option): discovery is what "reduces operator config burden."
  HA-19 is that promise broken for the self-hosted posture — the discovery
  that removes hand-registration toil is exactly the flow that can never
  complete against a private/loopback MCP server. This phase restores the
  discovery-completes property for the ordinary topology without weakening the
  SSRF posture on the attacker-influenceable legs.
- brief 09 §"Risks / open questions" ("The discovery input is adversarial"
  framing carried into D-297): the whole feature parses attacker-influenceable
  documents fetched from attacker-influenceable URLs. The relaxation therefore
  pins to the CONNECTION'S dial target (an operator-declared boot-config
  origin), never to an attacker-chosen value, and re-validates on every
  redirect — the same "report, don't follow" discipline read one layer down
  into the dialer.
- brief 14 §item 9 ("OAuth for HTTP servers"): the MCP HTTP auth edge is the
  home of RFC 9728 / 8414 discovery. This phase does not add a new surface —
  it corrects the dial policy on the surface Phase 164 shipped so the RFC 9728
  hop can reach a same-origin private host.

## Findings I'm departing from (if any)

None. This phase corrects an as-built defect in Phase 164; it does not depart
from any brief finding or from D-297. It STRENGTHENS D-297 point (5): the
per-hop SSRF specification was correct; the dial backstop was stricter than
the specified policy and this phase aligns the backstop to the policy the
decision already authored.

## Goals

- **Align the two SSRF guards.** Make the dial-time backstop obey the same
  per-hop policy `validateHop` already computes, instead of applying one
  global private-IP refusal that overrides the policy for the same-origin
  protected-resource hop.
- **Permit exactly one hop, one origin, pinned to the dial target.** The
  same-origin RFC 9728 protected-resource-metadata hop to the host the runtime
  is already configured to dial for THIS connection's tool calls (the
  operator-declared boot-config `ServerURL`) may reach a private/loopback
  resolved address. Nothing else relaxes.
- **Keep every cross-origin hop fully guarded.** The RFC 8414
  authorization-server hop stays behind the `oauth_discovery_allowed_origins`
  allowlist AND the private-IP refusal AND the DNS-rebinding defence. Its
  intended outcome remains `needs_allowance`.
- **Close the DNS-rebinding vector on the relaxed hop (the crux).**
  "Same-origin" is computed on the ORIGIN STRING, but the SSRF defence is by
  RESOLVED IP. A naive "if same-origin, allow private" reopens DNS rebinding:
  an attacker-influenced metadata URL (from the `WWW-Authenticate` challenge)
  whose host matches the connection's origin string but whose resolution — or
  a redirect mid-walk — reaches a DIFFERENT private IP (a cloud metadata
  service, another internal host) must STILL be refused. The relaxation is
  pinned to the connection's actual dial target (host:port AND the resolved IP
  set the connection legitimately uses), not to "any private IP when the
  origin string matches."
- **No production private-network knob.** The production path becomes reachable
  through the same-origin-pinned mechanism ONLY. No `allowPrivate` config/env
  is exposed; `WithPrivateNetworkAccessForTest` stays test-only and still
  panics outside a test binary.

## Non-goals

- No OAuth flow execution, no token custody, no refresh — D-271 PULL custody
  and the D-297 report-don't-follow boundary are UNCHANGED. This phase touches
  only which dial the discovery fetcher may complete.
- No change to the authorization-server hop's allowance gate — the AS hop still
  requires an `oauth_discovery_allowed_origins` entry and still refuses
  private/IP-literal destinations. `needs_allowance` stays the intended halt
  (that is HA-15 / Phase 168's surface, not this phase's).
- No new config field, wire type, method, or event. This is a dial-policy fix
  internal to `internal/tools/auth`; `ProtocolVersion` is unbumped and there is
  no D-223 / D-209 churn.
- No relaxation of any other guardrail: bounded redirects (3), body-size cap
  (8 KiB), per-fetch timeout (5 s), no-proxy, credential-stripping on redirect,
  RFC 7591 registration-endpoint report-don't-invoke — all unchanged.
- No stdio-transport change; the challenge is an HTTP-auth construct.
- **A granted allowance still does NOT let the AS-metadata hop reach a
  private-address authorization server.** The RFC 8414 authorization-server hop
  is inherently cross-origin, so even with an `oauth_discovery_allowed_origins`
  entry it stays subject to the private-IP refusal (the relaxation is
  protected-resource-hop-only). For a private-address AS, what surfaces is
  `needs_allowance` plus the AS issuer read from the RFC 9728 document — not the
  fetched AS metadata. An operator who grants the origin and re-probes should
  not be surprised that a private AS still does not auto-populate; that is the
  intended custody/SSRF posture, not a regression. (Recorded so the UX is
  honest.)

## The two guards, and why they disagree (root cause)

The walker enforces SSRF policy at two layers of `internal/tools/auth/discovery.go`:

1. **The per-hop policy — correct, kept.** `validateHop` (`:427`) computes
   `sameOrigin := targetOrigin == serverOrigin` (`:435`). For the
   protected-resource hop (`case StepProtectedResource:` at `:442`) it permits
   same-origin and refuses only cross-origin-without-allowance (`:444-446`); it
   accommodates loopback in the scheme check (https-only OFF loopback, `:449`,
   `:451`). The authorization-server hop (`case StepAuthorizationServer:` at
   `:437`) always requires an explicit allowance (`ReasonNeedsAllowance`,
   `:440`). The cross-origin IP-literal refusal fires only
   `if !sameOrigin && !d.allowPrivate` (`:455`). So by its OWN policy a
   same-origin protected-resource hop to a private-address MCP server is
   PERMITTED and passes `validateHop`.

2. **The dial-time backstop — unconditional, stricter than the policy above.**
   `NewDiscoverer` (`:245`) installs `net.Dialer.Control` (`:260-281`) that
   refuses ANY resolved private/loopback IP via `isPrivateIP` (`:276-278`
   returning `ErrDiscoveryRefused`) — with NO same-origin exemption and NO
   reference to which hop/step is being served. So the same-origin hop
   `validateHop` just approved dies at connect. The only relaxation,
   `WithPrivateNetworkAccessForTest` (`:232`), PANICS outside a test binary
   (`:234-235`), so there is NO production config/env/per-connection path to
   the working path.

Consequence: in the ordinary self-hosted topology (runtime + MCP servers on
the same private/compose/loopback network) discovery fails on the FIRST hop;
the runtime never learns the requirement; the consumer's consent gate has
nothing to render. The green test suite masked this because EVERY positive-path
chain-walk test constructs the discoverer with `WithPrivateNetworkAccessForTest`
(`discovery_test.go` :103/:204/:277/:307/:328/:350/:374/:504/:543;
`mcpconsole/oauth_discovery_test.go:63`;
`test/integration/phase164_mcp_oauth_discovery_test.go:114`) — the escape hatch
that hides the exact dial the production path cannot make. `validateHop`'s
same-origin branch was consequently never exercised end-to-end against a real
dial.

## The fix (align the backstop to the per-hop policy, pinned to the dial target)

The dialer must know WHICH hop it is serving and WHICH target the connection is
pinned to, rather than applying one global refusal to both hops. TWO guards
disagree with the per-hop policy today and BOTH must be aligned — the dial-time
`net.Dialer.Control` private-IP refusal (the one HA-19 named) AND, for the
common compose/k8s posture, `validateHop`'s https-off-loopback refusal (a
SECOND guard that also blocks the target posture; see "The second guard" below).

1. **Resolve the pinned IP:port ONCE per walk.** `Discover` resolves the
   operator-declared `DiscoveryInput.ServerURL` host to its full resolved IP
   SET (`pinnedIPs` — all A/AAAA records, so a legitimately multi-homed private
   server is not spuriously refused) and records the pinned PORT
   (`pinnedPort`) at the START of the walk. Both are sourced from the TRUSTED
   boot-config `ServerURL` — NEVER the attacker-influenceable
   `ResourceMetadataURL`. This is a single resolution, threaded into every
   `fetchHop`; it is NOT re-resolved per hop (that would reopen a per-hop TOCTOU
   window and cost extra lookups).

2. **Per-fetch dial pin carried on the fetch context.** `fetchHop` attaches a
   `dialPin` value to the per-fetch `ctx` describing THIS hop:
   - `step` (`StepProtectedResource` | `StepAuthorizationServer`);
   - `sameOrigin` (as `validateHop` computes it);
   - `pinnedIPs` — the walk-start resolved IP set (from step 1);
   - `pinnedPort` — the walk-start pinned port (from step 1).

   The pin rides `ctx` (per-run state), never the shared `Discoverer` struct —
   D-025-clean: the compiled artifact stays immutable and safe to share across
   concurrent `Discover` calls. `DiscoveryInput` / `Discover` / `NewDiscoverer`
   exported signatures are unchanged; the pin is internal.

3. **Dialer reads the pin post-resolution via `ControlContext`.** Replace
   `net.Dialer.Control(network, address, RawConn)` with
   `net.Dialer.ControlContext(ctx, network, address, RawConn)` (Go 1.20+; the
   module is Go 1.26). `ControlContext` runs AFTER DNS resolution (it sees the
   resolved `address` = `ip:port` — the load-bearing property the current
   `Control` relies on) AND receives the `ctx` carrying the pin. Note
   `ControlContext` sees the resolved IP:port, NOT the hostname — so the gate
   is expressed on the RESOLVED address, not a hostname compare. The closure
   enforces, in order:
   - `d.allowPrivate` (test-only) → return nil, UNCHANGED.
   - Resolved IP is public → return nil (as today).
   - Resolved IP is private AND the pin is the same-origin protected-resource
     hop AND the resolved IP is a member of `pinnedIPs` AND the resolved port
     equals `pinnedPort` → return nil (the relaxation).
   - Otherwise → refuse `ErrDiscoveryRefused`, UNCHANGED. Cross-origin and
     authorization-server hops therefore keep the full private-IP refusal.

   **The port is part of the pin (SSRF).** IP-set membership ALONE is
   insufficient: a same-origin `302 → 127.0.0.1:22` (SSH) or `:6379` (Redis) on
   the pinned host would otherwise slip through — intra-host PORT SSRF, newly
   reachable because the fix enables the private dial. Pinning the port refuses
   any dial to a port other than the one the operator's `ServerURL` declared.

4. **Disable connection reuse on the discovery transport
   (`DisableKeepAlives: true`).** The fix newly enables private-address dials;
   a pooled private connection reused on a LATER hop to the same host:port
   would NOT re-enter `ControlContext`, bypassing the step-gate (so the "AS hop
   stays fully guarded" property could be defeated by a same-host reuse).
   Discovery does a handful of one-shot metadata fetches; keep-alive buys
   nothing. Every dial therefore re-runs the gate.

### The second guard (WARN-1 — the gating correctness fix)

`validateHop` carries a SECOND refusal that also blocks the target posture:
https-off-loopback (`discovery.go:449-452` — `if u.Scheme != "https" &&
!loopback { return ReasonNotHTTPS }`, where `isLoopbackHost` is true ONLY for
`localhost` / `127.x` / `::1`). This refuses the same-origin protected-resource
hop for a plain-HTTP NON-loopback MCP server — a Docker-compose service name
(`http://mcp:8080`), a k8s in-cluster address (`http://mcp-svc.ns:8080`). Harbor
config PERMITS plain-HTTP MCP URLs (unlike A2A peers, the MCP-server config
validation imposes no https requirement). So two of the four postures HA-19
names ("container-compose / private VPC") would stay INERT after the dial fix,
and every loopback-bound fixture is exempt from this check — the suite would go
green while compose/k8s stayed broken. **The fix EXTENDS the same-origin-pinned
relaxation to this check too:** for the same-origin protected-resource hop to
the pinned target (the identical `pinnedIPs`/`pinnedPort` predicate),
plain-HTTP is permitted. A non-pinned or cross-origin plain-HTTP target still
returns `ReasonNotHTTPS`.

*Why extend rather than narrow (the call).* Harbor is self-hosted-first and
compose/k8s is THE deployment story, so relaxing only "loopback-HTTP +
private-HTTPS" would half-fix the feature. The safety argument is identical to
the dial relaxation: the connection ALREADY dials that plaintext host for tool
calls, unguarded, and discovery carries NO credentials of any kind — the https
check was protecting against plaintext CREDENTIAL leakage on the wire, and a
credential-free metadata hop to the same operator-declared pinned target has no
credential to leak. So the https-off-loopback refusal earns nothing on the
pinned same-origin hop while blocking the dominant posture. The cross-origin AS
hop keeps https-off-loopback in full (it is neither same-origin nor pinned).

### The DNS-rebinding subtlety (the crux — pin to the resolved IP, not the string)

Pinning to host:port alone does NOT close rebinding: the `ResourceMetadataURL`
is attacker-controlled (it arrives on the server's `WWW-Authenticate`
challenge), and same-origin only constrains the origin STRING. An attacker
whose server the operator declared at `http://mcp-host:9000` can (a) point
`resource_metadata` at `http://mcp-host:9000/…` (same origin string, legit
case) but rely on DNS to move `mcp-host` to `169.254.169.254`, or (b) return an
HTTP redirect that stays same-origin by string but resolves elsewhere.

The relaxation therefore pins to the RESOLVED IP:port the connection
legitimately uses. The discoverer resolves the pinned `ServerURL` host ONCE at
the start of the walk into `pinnedIPs` and records `pinnedPort`, carries both
on the fetch ctx, and the `ControlContext` closure permits a private resolved
address for the same-origin hop ONLY when its IP is a member of `pinnedIPs` AND
its port equals `pinnedPort`. A rebind — or a same-origin-by-string redirect —
that resolves to a DIFFERENT private IP, or to a DIFFERENT port on the pinned
IP, is refused because it fails set/port membership. Both are provable in unit
tests (two names resolving to two different private addresses; a same-host
redirect to a different port).

Enforcement of a redirect is by `ControlContext` (which fires on EVERY dial,
including each redirect hop), NOT by a `validateHop` re-entry — `validateHop`
runs once on the initial target and `CheckRedirect` only bounds the redirect
count and strips inherited credentials. So a redirect leaving the pinned target
(different IP, or different port on the pinned IP) is refused at the redirect's
dial by pin-membership, and `DisableKeepAlives` guarantees the redirect dial is
a fresh connection that actually re-enters the gate.

**Documented residual boundary (honest scoping).** Pinning to the
walk-start resolution closes the discovery-specific rebinding vector (the
attacker-influenced metadata URL / redirect within the walk). It does NOT close
a TOCTOU rebind of the operator's OWN declared `ServerURL` host between the
tool-call connect and the discovery walk — but that same rebind also
compromises the connection's tool calls (which dial the same host with no SSRF
backstop today), so it is outside the discovery-specific threat boundary and is
recorded as such. A stronger future option — capturing the resolved IP at the
MCP connection's established transport and pinning discovery to THAT set — is
noted as a follow-up, not built here (it needs IP capture plumbed out of the
MCP driver's dialer; larger blast radius, no additional protection for the
self-hosted posture this phase targets). The plan records this so the boundary
is explicit rather than silently narrower than "rebinding is impossible."

## Acceptance criteria

- [ ] The same-origin RFC 9728 protected-resource-metadata hop to the
  connection's operator-declared `ServerURL` COMPLETES on the PRODUCTION
  construction path (`NewDiscoverer()` with NO
  `WithPrivateNetworkAccessForTest`) when that server resolves to a
  private/loopback address — pinned to `pinnedIPs` + `pinnedPort`. Proven for
  ALL THREE self-hosted postures, each a named subtest, NOT just a
  loopback-hostname fixture:
  - a loopback-hostname `ServerURL` (`http://localhost:PORT`);
  - **an IP-literal `ServerURL` (`http://127.0.0.1:PORT`) — the canonical
    compose/localhost form** (the flip of today's
    `TestDiscoverer_Discover_DialTimeIPLiteralPrivateRefusal`, `discovery_test.go:433`);
  - **a plain-HTTP NON-loopback service-name `ServerURL` (`http://mcp:PORT`
    resolving to a private IP) — the compose/k8s posture** (proves the
    WARN-1 https-off-loopback extension: same-origin pinned plain-HTTP hop
    completes).
  Named test: `TestDiscoverer_SameOriginPrivateResourceHop_CompletesOnProductionPath`
  (with the three posture subtests).
- [ ] A NON-pinned / cross-origin plain-HTTP target STILL returns
  `ReasonNotHTTPS` — the https-off-loopback relaxation is pinned-target-only.
  Named test: `TestDiscoverer_NonPinnedPlainHTTP_StillNotHTTPS`.
- [ ] A cross-origin protected-resource or authorization-server hop that
  resolves to a private IP is STILL refused on the production path (the
  relaxation never applies off the pinned origin). Named test:
  `TestDiscoverer_CrossOriginPrivateHop_StillRefused`.
- [ ] **Step-gate discriminator:** an authorization-server hop whose target IP
  is IN `pinnedIPs` (an attacker sets `authorization_servers[]` to the
  operator's OWN origin) is STILL refused — proving the relaxation is gated by
  `step == StepProtectedResource`, not merely by origin-difference. Named test:
  `TestDiscoverer_AuthServerHopToPinnedIP_StillRefused`.
- [ ] **DNS-rebinding crux:** a same-origin-by-string metadata URL / redirect
  whose host resolves to a DIFFERENT private IP than `pinnedIPs` is STILL
  refused. Named test:
  `TestDiscoverer_SameOriginRebindToDifferentPrivateIP_StillRefused`
  (proven to FAIL — i.e. wrongly permit — under an origin-string-only pin with
  no resolved-IP membership check; the test is the discriminator).
- [ ] **Intra-host PORT SSRF:** a same-origin redirect to a DIFFERENT port on
  the pinned IP (e.g. `302 → {pinnedIP}:6379`) is STILL refused (port is part
  of the pin). Named test:
  `TestDiscoverer_SameOriginRedirectToDifferentPort_StillRefused`.
- [ ] **No keep-alive step-gate bypass:** a multi-hop sequence to the SAME
  host:port re-enters `ControlContext` on every dial (`DisableKeepAlives`), so
  a later cross-origin/AS hop cannot ride a pooled private connection. Named
  test: `TestDiscoverer_SameHostMultiHop_ReValidatesEachDial`.
- [ ] A redirect that leaves the pinned target (different IP, or different port
  on the pinned IP) is refused AT THE REDIRECT'S DIAL by `ControlContext`
  pin-membership — NOT by a `validateHop` re-entry (`validateHop` runs once on
  the initial target; `CheckRedirect` only bounds count + strips credentials).
  Named test: `TestDiscoverer_RedirectOffPinnedTarget_StillRefused`.
- [ ] The authorization-server hop still requires an
  `oauth_discovery_allowed_origins` entry and still surfaces the typed
  `needs_allowance` partial status without one — the relaxation does not touch
  the AS gate. Named test:
  `TestDiscoverer_AuthServerHop_StillNeedsAllowance`.
- [ ] All other guardrails unchanged and still pinned by their existing tests:
  bounded redirects (3), body-size cap (8 KiB), per-fetch timeout (5 s),
  no-proxy, credential-stripping on redirect, no Authorization/Cookie header on
  any discovery fetch. (Existing negative tests retained; the fix must not
  regress them.)
- [ ] No production `allowPrivate` path exists: a grep asserts no
  config/env/option sets `allowPrivate` outside the test-only
  `WithPrivateNetworkAccessForTest`, which still panics outside a test binary.
  Named test: `TestWithPrivateNetworkAccessForTest_PanicsOutsideTestBinary`
  (retained/strengthened) + the drift grep in the smoke.
- [ ] RFC 7591 dynamic client registration stays REPORTED, never INVOKED;
  discovered endpoints stay inert data (D-297 report-don't-follow). Named
  test: the retained zero-non-metadata-fetch recording assertion.
- [ ] The runtime still NEVER runs the OAuth flow and never holds a token
  (D-271) — asserted structurally: this phase touches only the dial policy;
  no token-custody surface is added (reviewer-verified + no new field on the
  custody boundary).
- [ ] §17.1 integration test: against a spec-derived RFC 9728/8414 fixture
  bound to loopback and reached via a same-origin hostname, the PRODUCTION-path
  discoverer completes the protected-resource hop and reaches the AS hop's
  `needs_allowance` — the self-hosted posture end-to-end — plus the rebind
  refusal and the cross-origin refusal, under `-race`.
- [ ] Concurrent-reuse: N≥100 concurrent `Discover` calls against a single
  shared `Discoverer` — each pinning a DIFFERENT `ServerURL` — never bleed a
  pin across walks (a walk pinned to origin A must never permit a private dial
  pinned by concurrent walk B), under `-race`.
- [ ] `scripts/smoke/phase-170.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race`; coverage ≥ 85% on `internal/tools/auth`.

## Files added or changed

- `internal/tools/auth/discovery.go`:
  - `NewDiscoverer` — swaps `net.Dialer.Control` → `ControlContext`; the
    closure reads the per-fetch `dialPin` from ctx and applies the
    same-origin-pinned relaxation (private IP allowed only for the
    protected-resource step when `resolvedIP ∈ pinnedIPs && resolvedPort ==
    pinnedPort`); sets `http.Transport.DisableKeepAlives = true` so every dial
    (incl. each redirect) re-enters the gate.
  - `Discover` — resolves the operator-declared `ServerURL` host ONCE into the
    `pinnedIPs` set + `pinnedPort` at walk start, threads them into `fetchHop`.
  - `fetchHop` — builds the `dialPin` (step + `sameOrigin` + `pinnedIPs` +
    `pinnedPort`) and threads it onto the fetch ctx (a small unexported
    `dialPin` ctx-key type + helper).
  - `validateHop` — the https-off-loopback refusal (`:449-452`) is EXTENDED to
    permit the same-origin protected-resource hop to the pinned target
    (plain-HTTP allowed for the pinned same-origin PR hop; non-pinned /
    cross-origin plain-HTTP still `ReasonNotHTTPS`). Its same-origin
    computation is reused to set the pin's `sameOrigin`.
  - `WithPrivateNetworkAccessForTest` / `allowPrivate` unchanged (test-only,
    still panics outside a test binary).
- `internal/tools/auth/discovery_test.go` — migrate the protected-resource
  positive path off `WithPrivateNetworkAccessForTest` to the production pin;
  add the three-posture completion subtests (loopback-hostname, IP-literal,
  plain-HTTP non-loopback service name), the non-pinned-plain-HTTP-still-NotHTTPS
  test, the crux rebind test, the port-SSRF redirect test, the step-gate
  AS-to-pinned-IP test, the cross-origin-still-refused test, the
  redirect-off-target test, the keep-alive multi-hop test, the
  AS-still-needs-allowance test, the concurrent pin-isolation test; retain the
  existing guardrail negatives. Rework the two tests that today ASSERT the bug:
  `TestDiscoverer_Discover_DialTimeHostnameToPrivateRefusal` (`:400`, same-origin
  hostname→loopback refused) and
  `TestDiscoverer_Discover_DialTimeIPLiteralPrivateRefusal` (`:433`, same-origin
  IP-literal `http://127.0.0.1:PORT` refused) — both become pinned-relaxation
  POSITIVES (they are the canonical self-hosted postures), with their negative
  role preserved by the new rebind / different-port / cross-origin tests.
- `test/integration/phase170_mcp_oauth_discovery_dial_test.go` (new) — the
  §17.1 production-path self-hosted-posture end-to-end.
- `scripts/smoke/phase-170.sh` (new).
- `docs/plans/README.md` (row + detail block), `docs/decisions.md` (D-304),
  `docs/glossary.md` (the pinned-dial term).

## Public API surface

- No wire change, no method, no event, no config field, no exported-signature
  change. `NewDiscoverer` / `Discover` / `DiscoveryInput` signatures are
  unchanged; the pin is internal to the fetch path. The one implementation the
  flow-executing siblings (85b / parked 92p) reuse — `auth.Discoverer` →
  `auth.OAuthRequirement` — gains the corrected dial policy for free.

## Test plan

- **Unit:** the named security tests above — the three-posture completion
  (loopback-hostname / IP-literal / plain-HTTP non-loopback service name),
  non-pinned-plain-HTTP-still-NotHTTPS, cross-origin still-refused, step-gate
  AS-to-pinned-IP still-refused, rebind-to-different-private-IP crux,
  intra-host different-port SSRF, keep-alive multi-hop re-validation,
  redirect-off-target, AS-still-needs-allowance; the retained guardrail
  negatives (redirect bound, size cap, timeout, no-credential); the
  `WithPrivateNetworkAccessForTest`-panics test; a `dialPin` unit table
  (public IP allowed; same-origin private in-pinned-set + matching-port
  allowed; same-origin private in-set but WRONG port refused; same-origin
  private out-of-set refused; cross-origin private refused; AS-step private
  refused even when IP in-set; missing pin → fail-closed refuse).
- **Integration (`test/integration/phase170_mcp_oauth_discovery_dial_test.go`):**
  a spec-derived RFC 9728/8414 fixture (the Phase 164 §17.8 committed testdata:
  RFC 9728 §3.2 + RFC 8414 §3.2 example documents) bound to loopback. Two
  production-path posture legs (NO test-only escape), each: dial → challenge
  captured → probe → protected-resource hop COMPLETES → AS hop surfaces
  `needs_allowance`:
  1. an IP-literal `ServerURL` (`http://127.0.0.1:PORT`) — the canonical
     compose/localhost posture;
  2. a plain-HTTP non-loopback service-name `ServerURL` — reached via a
     hostname that resolves to the loopback fixture (proving the WARN-1
     https-off-loopback extension end-to-end through the real MCP http
     transport + `mcpconsole` probe path).
  Plus: the rebind negative (a second name resolving to a different private IP
  refused), the different-port negative, the cross-origin negative, identity
  propagation on the probe/read path + a cross-tenant read refusal (≥1 failure
  mode), `-race`. An env-gated live leg (`HARBOR_LIVE_MCP_OAUTH`) against a real
  OAuth-protected MCP server on a private address is the wave's
  live-verification step, not CI (§17.8): the CI fixture proves the dial
  policy; the live leg proves it against a real server the fixture can only
  approximate.
- **Conformance:** the discovery fixtures ARE the spec-conformance artifacts
  (Phase 164's committed spec-derived testdata, reused). No driver-seam suite
  change.
- **Concurrency / leak:** N≥100 concurrent `Discover` calls, each pinning a
  distinct `ServerURL`, against one shared `Discoverer` under `-race` — no pin
  bleed, no goroutine leak (fetches bounded by timeout, joined on return).

## Smoke script additions

- Class `unit-tests` (the dial policy needs the spec-derived fixture server,
  which the live dev boot does not run): `go test -race` on
  `internal/tools/auth` (the discovery walker + the new dial-pin tests) and the
  `test/integration` phase-170 test; plus a static invariant guard for the "no
  production private-network knob" property. The guard must NOT be a naive
  `grep -v 'WithPrivateNetworkAccessForTest'` — the real assignment is
  `d.allowPrivate = true` INSIDE the option body (`discovery.go:237`), a line
  that does NOT contain the option name, so `grep -v` would not exclude it and
  the guard would false-FAIL. Instead: enumerate every `allowPrivate = true`
  assignment, assert NONE lives outside `internal/tools/auth/discovery.go`, and
  assert EXACTLY ONE lives inside it (the test-only option's body) — so a new
  production setter OR a second in-file setter both trip the guard.
- Done-definition: `OK ≥ 2, FAIL = 0`; SKIP until the phase ships.

## Coverage target

- `internal/tools/auth`: 85% (maintains the Phase 164 target on the touched
  package; the new dial-pin branches are all covered by the named tests).

## Dependencies

- 164 (D-297 — the discovery walker this fixes; the code, fixtures, and view
  are all Phase 164's). 28 (the MCP southbound driver whose `ServerURL` is the
  pinned target), 30 (tools/auth home). Related, NOT structural deps: 168
  (HA-15 — the `oauth_discovery_allowed_origins` allowance WRITE surface; this
  phase preserves the AS-hop allowance gate 168 feeds but does not depend on
  it) and the flow-executing siblings 85b / parked 92p (reserved D-246) that
  reuse the corrected walker. NOTE: the Track A plans (166–169) are on a
  separate unmerged branch; 170 is independent of them structurally and
  cross-references 168 conceptually only.

## Risks / open questions

- **This is a security relaxation.** The adversarial review must attack the
  rebind crux first: confirm the resolved-IP pin (not just host:port) is what
  refuses a same-origin-by-string rebind, and confirm the pin never leaks
  across concurrent walks. The named rebind test is the load-bearing gate; it
  MUST be shown to wrongly-permit under a host:port-only pin before the
  resolved-IP membership check is added (the §17.8 "prove the discriminator
  fails the wrong implementation" discipline applied to a security invariant).
- **`ControlContext` vs `Control` semantics.** `ControlContext` must retain the
  post-resolution property (`address` is the resolved `ip:port`) so the
  DNS-rebinding defence still sees the real IP. Verify Go's dial path invokes
  `ControlContext` (not `Control`) post-resolution and that setting both is not
  required; the implementor confirms with a hostname→private test that still
  refuses cross-origin. `ControlContext` sees only the resolved IP:port, NOT the
  hostname — the pin gate is therefore expressed on the resolved address
  (IP-set membership + port), not a hostname compare.
- **Port SSRF (baked in).** IP-set membership ALONE would let a same-origin
  `302 → {pinnedIP}:22`/`:6379` through — intra-host port SSRF newly reachable
  because the fix enables the private dial. The pin includes `pinnedPort`; the
  different-port refusal is a named test.
- **Keep-alive step-gate bypass (baked in).** A pooled private connection reused
  on a later hop would skip `ControlContext` and defeat the step-gate. The
  discovery transport sets `DisableKeepAlives: true` so every dial (incl.
  redirects) re-enters the gate; the same-host multi-hop re-validation is a
  named test.
- **The https-off-loopback extension is the gating correctness fix.** Without
  it, the compose/k8s plain-HTTP posture (two of HA-19's four named postures)
  stays inert while a loopback-bound suite goes green — the same §17.8
  rubber-stamp the original bug shipped under. The three-posture completion
  test (loopback-hostname + IP-literal + plain-HTTP non-loopback) is the guard;
  the non-pinned-plain-HTTP-still-`ReasonNotHTTPS` test bounds the extension.
- **Walk-start resolution TOCTOU.** `pinnedIPs` is resolved once at walk start;
  a rebind between that resolution and the hop dial is a residual (documented
  above). It does not widen the self-hosted posture's safety because the pinned
  host is operator-declared and already dialed for tool calls; recorded as a
  boundary, with connect-time IP capture noted as the stronger follow-up.
- **Multiple A-records / IPv4+IPv6.** `pinnedIPs` must be the FULL resolved set
  so a legitimate multi-homed private server is not spuriously refused when the
  dial picks a different address from the set; the membership check is over the
  set, not the first address.
- **Pin absence must fail closed.** If a fetch ctx somehow carries no pin
  (defensive), `ControlContext` refuses any private IP — never a silent allow
  (§13 fail-loudly).

## Operator-surface note (§18)

This is a runtime-internal dial-policy correction — no `harbor` CLI verb,
config field, wire type, or Console route changes. The
`observe-with-the-console` skill's "MCP Connections" section already describes
`mcp.servers.probe` triggering requirement discovery and the detail rail
showing the discovered authorization server(s) marked "unverified"; this fix
makes that documented behavior REACHABLE for the self-hosted posture (localhost
/ compose / VPC) where it previously died at the protected-resource fetch — it
does not change any operator STEP or the rendered shape. No skill edit is
required (the skill describes the intended, now-restored behavior). Recorded
explicitly per §18's "if you change the surface an operator follows, update the
playbook" rule: the operator's steps are unchanged; only the previously-blocked
data path becomes reachable. If the implementor finds the skill implies
something now false, it is updated in the same PR.

## Glossary additions

- "Same-origin discovery-dial pin" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on `internal/tools/auth` ≥ 85%
- [ ] If multi-isolation paths changed: N/A — the fix is dial policy; the
      probe/read identity path is unchanged from Phase 164 (its cross-tenant
      read refusal is retained in the integration test).
- [ ] **Reusable-artifact concurrent-reuse:** N≥100 concurrent `Discover`
      calls against one shared `Discoverer`, each pinning a distinct origin,
      under `-race` — no pin bleed, no goroutine leak.
- [ ] **Integration test wires real drivers end-to-end (the real MCP http
      transport + spec-derived fixture on loopback), asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`** (§17.1 + §17.8).
- [ ] Wire changes: NONE — `ProtocolVersion` unbumped; no D-223 / D-209 churn
      (zero-diff on both gates).
- [ ] If new vocabulary: glossary updated (the pinned-dial term)
- [ ] If a brief finding was departed from: N/A — none departed
