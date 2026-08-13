// Package rollups owns Harbor's observability-rollup domain: fixed-UTC-bucket,
// identity-dimensioned, additive aggregate records projected from the
// successfully-persisted canonical event log.
//
// # What this is
//
// The rollups domain answers "what happened, bucketed, per identity axis" for
// operators: cost, tokens, successful LLM completions, latency, and task
// outcomes, grouped by a closed dimension set (tenant / user / session / model,
// agent only when the source carries an authoritative agent id) over a fixed
// UTC time grid. It is the durable, queryable counterpart of the event bus's
// live metrics derivation — metrics labels stay low-cardinality by the
// telemetry cardinality firewall, while rollups carry the identity dimensions
// the firewall deliberately excludes, stored as rows in a Store rather than as
// OTel metric labels. Rollups do NOT register identity-labelled OTel metrics;
// the identity dimensions exist only in the rollup rows.
//
// # How data enters
//
// A Projector consumes successfully-persisted canonical events from the
// durable event log (via a caller-provided Source) and applies their measure
// deltas to a Store. The checkpoint is the existing local durable sequence —
// the bus Sequence the durable log already assigns and persists — so no new
// event id, no outbox, and no cross-runtime coordination exist: the projector
// is a single-runtime cursor over the log it reads. Application is atomic per
// batch (deltas + checkpoint in one Store transaction), which makes replay
// idempotent: re-applying a batch whose checkpoint does not advance the
// stored checkpoint is a no-op.
//
// # Honest scope
//
// Rollups are BEST-EFFORT operational aggregates, not billing or accounting
// records. Cost and token measures are exact sums of the provider-reported
// values carried by the canonical `llm.cost.recorded` event; authoritative
// per-call accounting remains in-band in the governance subsystem. The
// projector makes NO active-active claim: the Store has a single writer, the
// Projector, and no cross-runtime consistency is promised. Erased sessions are
// fenced (rows deleted, late events refused), so an erasure is never
// resurrected by an asynchronous tail event.
//
// # Layout
//
//   - bucket.go — the fixed UTC bucket grid (hour, day).
//   - dimension.go — the closed dimension set.
//   - measure.go — the closed, additive measure set.
//   - key.go — the row key (bucket + authoritative dimension values).
//   - extract.go — the pure event → deltas extractor (source-backed measures).
//   - query.go — the typed, validated read surface + deterministic pagination.
//   - store.go — the mandatory Store interface + sentinels.
//   - projector.go — the checkpointed projector + quality surface.
//   - memstore/ — the indexed in-memory Store implementation (the reference
//     driver the conformance suite exercises; SQLite / Postgres
//     implementations consume the same interface + suite).
//   - conformancetest/ — the conformance suite every Store implementation
//     must pass.
package rollups
