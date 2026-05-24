# Phase 83v — runtime-cors

## Summary

Closes Bug **F4** from the round-2 walkthrough — the showstopper that
breaks the D-091 multi-process Console+Runtime posture. The Runtime's
HTTP transports never emit any CORS headers. The Console (one origin)
cannot fetch / SSE-subscribe to a remote Runtime (different origin)
because the browser blocks every cross-origin request at the
preflight stage. `grep -rn 'Access-Control\|cors' --include='*.go'`
over the entire repo returns ZERO matches.

83v adds operator-configurable CORS to every HTTP wire transport.
Security posture: default to **deny everything** — no operator-side
yaml means same-origin only (the current behavior). The operator
opts in by listing exact origins. No wildcards in production; the
middleware rejects `*` outside a documented dev-only flag.

## RFC anchor

- RFC §5 — Console as Protocol client (the multi-process posture
  this fixes).
- RFC §7 — Wire transports (where the middleware lives).

## Briefs informing this phase

- brief 06
- brief 12

## Brief findings incorporated

- brief 06 §1 — DevX is binding: documented multi-process posture
  must actually work. The pre-83v state advertises "Console can
  attach to any remote Runtime" but blocks it at the wire.
- brief 12 §2 — The Console is a Protocol client; the contract is
  the HTTP+SSE transport. CORS is the browser-side enforcement of
  cross-origin access — Harbor's wire transport must speak it.

## Findings I'm departing from (if any)

None.

## Goals

- New yaml field: `server.allowed_origins []string`. Each entry is
  an exact origin (`https://console.example.com:443` shape) that the
  Runtime accepts cross-origin requests from. Empty list (the
  default) = no CORS = same-origin only. No wildcard support unless
  the operator sets a documented dev-only override (`server.cors_dev_allow_any:
  true`) that prints a stderr banner.
- A small CORS middleware in `internal/protocol/transports/` that:
  - Handles `OPTIONS` preflight requests against the allow list.
    Emits `Access-Control-Allow-Methods` (`GET, POST, PUT, DELETE,
    PATCH, OPTIONS`), `Access-Control-Allow-Headers` (`Authorization,
    Content-Type, Last-Event-ID`), `Access-Control-Max-Age` (24h).
  - Emits `Access-Control-Allow-Origin: <exact-origin>` (NOT `*`)
    AND `Access-Control-Allow-Credentials: true` on matching requests
    (we send the JWT in the `Authorization` header on REST; for SSE
    the access_token query-param works without credentials, but the
    REST surface needs credentials enabled to let the operator
    optionally switch to cookie auth later).
  - Rejects requests from non-allowed origins by simply omitting
    the headers — the browser then blocks the response per the
    standard CORS contract.
- The middleware wraps both the REST control surface (`/v1/control/*`,
  `/v1/tasks/*`, `/v1/runs/*`, etc.) and the SSE surface (`/v1/events`).
- Validator update: `server.allowed_origins[i]` must be a valid
  `scheme://host[:port]` shape. The `*` wildcard is rejected unless
  `server.cors_dev_allow_any` is true.
- Smoke + Playwright test: spawn a Runtime on `:18080`, an
  httptest origin on `:18790`, verify a CORS-preflight from the
  test origin reaches the allow path; verify a non-allowed origin
  is blocked.

## Non-goals

- **Cookie auth.** The Bearer-token-in-Authorization-header posture
  stays. Cookie-based auth is a separate phase if it ever lands.
- **WebSocket CORS.** Wave 18 Protocol surface; out of scope.
- **A wildcard prod path.** Production deployments declare explicit
  origins. The dev-only `cors_dev_allow_any` is for `harbor dev`
  iterative testing only.

## Acceptance criteria

- [ ] New `config.ServerConfig.AllowedOrigins []string` field +
      validator (each entry parses as a URL with non-empty scheme +
      host; `*` rejected unless dev-only flag set).
- [ ] New `config.ServerConfig.CORSDevAllowAny bool` field with
      explicit godoc warning + stderr banner on boot when enabled.
- [ ] Middleware in `internal/protocol/transports/cors/` (or similar)
      that:
      - Handles `OPTIONS` preflight; returns 204 with allow headers
        on match, 204 with no headers on miss.
      - Emits per-origin `Access-Control-Allow-Origin` (echoes the
        request's Origin header verbatim after allowlist check).
      - Emits `Access-Control-Allow-Credentials: true`,
        `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`,
        `Access-Control-Max-Age`.
- [ ] The middleware wraps both REST + SSE handlers in
      `cmd/harbor/cmd_dev.go::bootDevStack` (production wire-up).
- [ ] Devstack mirror per D-094 in `harbortest/devstack/devstack.go`.
- [ ] Unit tests for the middleware: allowed origin → headers
      emitted; non-allowed → no headers; preflight OPTIONS → 204;
      wildcard rejected by validator unless dev flag.
- [ ] Integration test: bring up two httptest servers on different
      ports + assert cross-origin preflight succeeds when the
      Runtime's `allowed_origins` lists the test origin.
- [ ] `docs/CONFIG.md` documents `server.allowed_origins` +
      `server.cors_dev_allow_any` with a security note ("never
      wildcard in production").
- [ ] `scripts/smoke/phase-83v.sh` asserts the static surface.

## Files added or changed

- `internal/config/config.go` — `ServerConfig.AllowedOrigins` +
  `ServerConfig.CORSDevAllowAny`.
- `internal/config/validate.go` — origin validation.
- `internal/protocol/transports/cors/cors.go` — NEW; the middleware.
- `internal/protocol/transports/cors/cors_test.go` — NEW.
- `cmd/harbor/cmd_dev.go` — wire the middleware around the mux.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `test/integration/phase83v_cors_test.go` — NEW; cross-origin
  preflight integration test.
- `docs/CONFIG.md` — `server.allowed_origins` +
  `server.cors_dev_allow_any` sections.
- `docs/plans/README.md` — Phase 83v row.
- `docs/decisions.md` — D-162.
- `docs/plans/phase-83v-runtime-cors.md` — this plan.
- `scripts/smoke/phase-83v.sh`.

## Public API surface

- `config.ServerConfig` gains two fields.
- New package `internal/protocol/transports/cors/` exposing
  `Wrap(next http.Handler, cfg Config) http.Handler` (or similar
  middleware factory).

## Test plan

- **Unit:** middleware unit tests cover the four header cases
  (allowed, non-allowed, preflight, dev-wildcard).
- **Integration:** `test/integration/phase83v_cors_test.go` spawns
  two httptest servers (Runtime + simulated Console origin) and
  asserts the cross-origin preflight chain works end-to-end with
  the allowlist match.
- **Manual:** boot `harbor dev` with `server.allowed_origins:
  ['http://127.0.0.1:18790']`, boot `harbor console`, use the
  Settings → Add Runtime form to attach to `:18080` — every page
  loads real data without console errors.

## Smoke script additions

`scripts/smoke/phase-83v.sh` asserts:

- `ServerConfig.AllowedOrigins` field declared.
- `internal/protocol/transports/cors/cors.go` exists.
- `cmd/harbor/cmd_dev.go` wires the middleware around the mux.
- `docs/CONFIG.md` documents the two new fields.

## Coverage target

- `internal/protocol/transports/cors/`: 90% (small + critical
  security middleware).

## Dependencies

- Phase 60 (wire transports).

## Risks / open questions

- **SSE access_token query-param is not subject to CORS** (it's a GET
  by EventSource, but `EventSource` can't set Authorization header
  so the access_token rides as a query param). The CORS middleware
  still needs to emit headers on the GET response so a cross-origin
  Console can read the stream. Tested in the integration test.
- **Preflight cache TTL of 24h** is the default browser behavior;
  could be operator-configurable later. V1 picks one value.
- **The dev-only wildcard escape hatch could leak into prod.**
  Mitigated by the stderr banner + a CONFIG.md warning + the
  validator rejection of `*` without the explicit flag.

## Glossary additions

- **CORS allowlist** — operator-declared list of cross-origin
  Console origins the Runtime accepts requests from
  (`server.allowed_origins`). Phase 83v / D-162.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on `internal/protocol/transports/cors` ≥ 90%
- [ ] Concurrent-reuse — middleware is stateless; D-025 trivially OK
- [ ] Integration test exists per §17 — phase83v_cors_test.go
- [ ] Glossary updated
- [ ] If a brief finding was departed from: N/A
