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
| 17 — `ArtifactStore` interface + InMem + FS drivers | `internal/artifacts` — content-addressed blob store; eight-method interface; `ScopedArtifacts` facade; conformance suite; mandatory routing above heavy-output threshold (32 KB default) — no `NoOp` fallback (D-022, D-026) | Shipped |
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
