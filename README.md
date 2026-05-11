# Harbor

A Go-native runtime for durable, steerable, event-driven AI agents.

> Harbor is in pre-V1 development. The design surface is fully landed (RFC, master phase plan, research briefs, contributor normatives); implementation is rolling out wave by wave. See `RFC-001-Harbor.md` for the architecture, `docs/plans/README.md` for the master phase plan (84 V1 + 14 post-V1 phases), and `docs/research/` for phase-planning research briefs.

## At a glance

Harbor is a four-layer system:

- **Harbor Runtime** — the orchestration kernel. Tasks, planner runtime, tools, memory, sessions, events, skills.
- **Harbor Protocol** — the canonical event/state contract between the Runtime and any client.
- **Harbor Console** — the observability + control-plane UI. Architecturally a Protocol client.
- **Harbor CLI** — the `harbor` binary. `harbor dev` boots the local Runtime, Console, and dynamic agent scaffolding with draft saving.

Three product properties are non-negotiable: multi-isolation across `(tenant, user, session)`, the Console-as-Protocol-client decoupling, and a swappable Planner.

## Status

| Phase | What lands | Status |
|-------|------------|--------|
| 00 — Skeleton | Repo hygiene, AGENTS.md/CLAUDE.md, LICENSE, Makefile, CI scaffold, preflight + drift-audit gates, `docs/plans/`, `docs/rfc/`, `docs/research/`, glossary, decisions log | Shipped |
| 01 — Identity foundation | `internal/identity` — `(tenant, user, session)` triple + `Quadruple` + ctx helpers + `conformancetest` suite | Shipped |
| 02 — Configuration loader | `internal/config` — typed YAML loader (`goccy/go-yaml`), env overrides, validation, secret redaction, `examples/harbor.yaml` | Shipped |
| 03 — Audit redactor | `internal/audit` — single deep-redaction pass + driver registry + canonical secret rules + multimodal-aware redaction | Shipped |
| 04 — slog logger + standard attribute set | `internal/telemetry` — identity-aware structured logger; redacts every record via `audit.Redactor`; `BusEmitter` seam for Phase 05+ runtime.error events | Shipped |
| 05 — Event taxonomy + InMem `EventBus` + isolation | `internal/events` + `internal/events/drivers/inmem` — typed `EventBus` with server-enforced identity-scoped `Filter`, drop-oldest backpressure with `bus.dropped`, idle reaper, audit-before-emit | Shipped |
| 07 — `StateStore` interface + InMem + conformance suite | `internal/state` — generic `(Quadruple, Kind, Bytes)` surface, ULID-keyed idempotency, `conformancetest.Run` for downstream drivers (Phase 15 SQLite, Phase 16 Postgres) | Shipped |
| 16 — Postgres `StateStore` driver | `internal/state/drivers/postgres` — `pgx`-backed driver, forward-only embedded migrations with `pg_advisory_lock`-serialised runner, conformance suite inherited verbatim, CI service-container job | Shipped |
| 17 — `ArtifactStore` interface + InMem + FS drivers | `internal/artifacts` — content-addressed blob store; eight-method interface; `ScopedArtifacts` facade; conformance suite; mandatory routing above heavy-output threshold (32 KB default) — no `NoOp` fallback (D-022, D-026) | Shipped |
| 19 — `ArtifactStore` S3-style driver | `internal/artifacts/drivers/s3` — `aws-sdk-go-v2`-backed driver for AWS S3 / MinIO / Cloudflare R2 / any S3-compat backend; conformance suite inherited verbatim; optional `Presigner` capability for read-side URL hand-off (`PresignGet` only); MinIO CI service-container job | Shipped |
| 22 — `MessageBus` + `RemoteTransport` contracts | `internal/distributed` — at-least-once `MessageBus` + cross-process `RemoteTransport` designed against the full A2A v1 spec (vendored at `docs/specifications/a2a.proto`); every A2A RPC, message type, and oneof variant has a Go counterpart in `internal/distributed/a2a`; loopback driver for both; conformance suite as the gate (D-031) | Shipped |
| 26 — Tool catalog core + InProcess registration + ToolPolicy | `internal/tools` + `internal/tools/drivers/inproc` — unified `Tool` / `ToolDescriptor` / `ToolCatalog` / `ToolProvider` surface; `tools.RegisterFunc[I, O]` with reflection-derived JSON-Schemas (`santhosh-tekuri/jsonschema/v6`); `ToolPolicy` reliability shell (D-024) wrapping every invocation in timeout + exponential-backoff retry + validation regardless of transport | Shipped |
| 26a — Flow-as-Tool + per-flow Budget | `internal/runtime/flow` — `flow.Definition` + `flow.Compose(def) → engine.Engine` + `flow.RegisterAsTool(catalog, def, eng)` wiring with `Transport: TransportFlow`; per-flow `Budget` (D-023) composing with parent + identity-tier ceilings via `min()`; lock-free atomic accumulator (D-025) | Shipped |
| 06, 08+ | Subsequent waves per `docs/plans/README.md` | Pending implementation |

## Working in this repo

If you (human or AI) are about to modify anything here, read `AGENTS.md` first. It is binding.

```bash
make help            # all targets
make check-mirror    # AGENTS.md ↔ CLAUDE.md
make preflight       # build + boot + smoke (no-ops until Phase 1 lands)
make install-hooks   # one-time per clone
```

## License

**Apache-2.0.** See `/LICENSE` for the full text and `RFC-001-Harbor.md` §10 for the rationale (MIT was the considered alternate).
