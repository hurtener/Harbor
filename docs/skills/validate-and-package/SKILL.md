---
name: validate-and-package
description: "Run `harbor validate` + `make preflight` + the per-phase smoke gates before shipping an agent. Use when packaging an agent for a non-dev environment — production deploy, share with a teammate, hand off to CI, or ship a release tag."
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: "validate"
---

# Validate + package an agent

Before an agent leaves your dev workstation — to staging, to production, to a teammate's laptop, to a release tag — it goes through three gates: yaml validation, the preflight gate (the same one Harbor's pre-commit hook + CI both enforce), and one or more smoke checks against the running build. This skill is the canonical ordering and what to look for at each step.

## 1. `harbor validate` — yaml first

```bash
harbor validate ./harbor.yaml
```

This is the cheapest gate. It runs the config loader against your yaml and reports every issue with file:line precision:

- Missing required fields (`llm.driver`, `llm.provider`, `identity.issuer`, etc.).
- Type mismatches (`memory.budget_tokens: "8000"` instead of int).
- Enum violations (`memory.strategy: "summary"` not in `none|truncation|rolling_summary`).
- Bound violations (`governance.identity_tiers.free.budget_ceiling_usd: -1`).
- Cross-field constraints (`memory.driver: sqlite` without `memory.dsn`).
- Driver-existence checks (`llm.provider: nim` without a matching `custom_providers:` entry).

Run after every yaml edit. Failure modes are usually obvious from the message.

Expected output on success:

```text
config OK: ./harbor.yaml
```

## 2. `make preflight` — the live gate

```bash
make preflight
```

This is the same gate the pre-commit hook and CI both enforce. It:

1. Builds `./bin/harbor` (`CGO_ENABLED=0`, single static binary).
2. Boots `./bin/harbor dev` on `127.0.0.1:18080` with a temp data dir.
3. Waits for `/healthz` to return 200.
4. Runs every `scripts/smoke/phase-NN.sh` in parallel batches (per the `PREFLIGHT_REQUIRES:` header — `live-server` smokes against the running build, `static-only` smokes locally, `unit-tests` invokes `go test`).
5. Tears down (graceful TERM, then KILL, then cleanup of the temp dir).

What "preflight passes" means: every phase's surface that exists in your build works end-to-end against a live server.

If a surface doesn't yet exist in your build (e.g. a future phase), the smoke script auto-skips (404/405/501 → SKIP). The gate counts SKIP separately from FAIL — SKIPs are fine on master.

## 3. Per-phase smoke (cherry-pick when you need to)

Run a single phase's smoke in isolation:

```bash
HARBOR_BASE_URL=http://127.0.0.1:18080 \
HARBOR_DEV_TOKEN="$(cat /tmp/harbor-dev-token)" \
scripts/smoke/phase-72b.sh
```

Useful when you've narrowed a failing surface and want a fast iteration loop without re-running the full preflight (which takes ~2 minutes).

The smoke scripts use `scripts/smoke/common.sh`'s vocabulary:

- `assert_status <expected> <method> <path>` — HTTP status check.
- `skip_if_404 <method> <path>` — graceful degradation when a phase isn't built yet.
- `assert_json_path <jq-expr> <expected>` — JSON body assertion.
- `assert_json_truthy <jq-expr>` — non-empty body assertion.
- `protocol_call <method> <body>` — JSON-RPC call against the Protocol surface.
- `api_url <path>` — base-URL composition.

Don't roll new curl wrappers; reuse these. Forces consistency across phases.

## 4. Production deploy checklist

Beyond the gates above, before deploying to a non-dev environment:

- [ ] Real `identity.issuer` + `jwks_url` + `audience` — the placeholder values in the scaffold won't accept production tokens.
- [ ] Real LLM provider + API key in env (never in the yaml literal — use `env.NAME`).
- [ ] `state.driver: sqlite` or `postgres` — NOT `inmem` (runs vanish on restart).
- [ ] `memory.driver: sqlite` or `postgres` — same logic.
- [ ] `artifacts.driver: fs` / `sqlite` / `postgres` — `inmem` artifacts vanish on restart.
- [ ] `events.driver: sqlite` or `postgres` — for durable event replay.
- [ ] `governance.identity_tiers` set with real budget ceilings — `{}` (the default) is no enforcement.
- [ ] `server.allowed_origins` lists every Console host that will attach.
- [ ] `server.bind_addr` set (default `127.0.0.1:8080` only listens loopback).
- [ ] `telemetry.log_format: json` (default) — for log aggregator ingestion.
- [ ] `telemetry.log_level: info` (NOT `debug` — debug leaks prompt/completion content).
- [ ] If using Postgres: migrations run, connection pool sized.
- [ ] If using SQLite: WAL mode confirmed, DSN on a fast disk.

## 5. Mirror invariant — AGENTS.md ↔ CLAUDE.md

The scaffold drops `AGENTS.md` and `CLAUDE.md` as verbatim mirrors. Edit one, copy to the other:

```bash
cp AGENTS.md CLAUDE.md   # or vice versa
diff -q AGENTS.md CLAUDE.md   # expected: no output
```

If your project has the equivalent of Harbor's `make check-mirror`, run it. CI will fail if they drift.

## 6. Release tagging

When you're ready to tag a release:

```bash
git tag -a v1.0.0 -m "v1.0.0: <one-line summary>"
git push --tags
```

If you have a release workflow (Harbor's repo does — `.github/workflows/release.yaml`), the annotated tag triggers a build of the static binary + a checksum + a GitHub Release with the binary attached.

The release notes should reference the agent's CHANGELOG (the user-facing one, not internal phase numbering — see Harbor's `scripts/smoke/phase-82.sh` rule).

## Common failure modes

- **`make preflight` fails on a phase you didn't touch.** Almost always either (a) stale build (`make clean && make build`), (b) a port conflict on `:18080` (kill the stray `harbor dev`), or (c) a SQLite WAL trap from a memory/state DSN inside the project dir.
- **`harbor validate` passes but `harbor dev` exits at boot.** The validator catches yaml-shape errors; it does NOT catch runtime errors like missing env vars (`ErrMissingAPIKey`) or unreachable JWKS URLs. Boot once after validating.
- **A smoke script reports SKIP for a surface you definitely built.** The script's `skip_if_404` is firing because the endpoint returned 404. The endpoint is registered wrong, OR the build is stale.
- **Production agent rejects all tokens.** Identity block points at the wrong issuer/jwks_url. Cross-check with `curl <jwks_url>` from the deployment host.

## See also

- [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) — the start of the flow.
- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — every field validated by `harbor validate`.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — the dev posture; preflight uses the same boot path.
- Harbor's CLAUDE.md §4 + §14 — the canonical build/test/lint targets + the pre-merge checklist.
- Harbor's `.github/workflows/ci.yml` — the CI gates a real PR runs through.
