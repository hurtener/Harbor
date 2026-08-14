// Package rollups owns Harbor's observability-rollup domain: fixed-UTC-bucket,
// identity-dimensioned, additive aggregate records projected from the
// successfully-persisted canonical event log.
//
// # What this is
//
// The rollups domain answers "what happened, bucketed, per identity axis" for
// operators: cost, tokens, successful LLM completions, latency, and task
// outcomes, grouped by the closed dimension set (tenant / user / session /
// model) over a fixed UTC time grid. The grid is anchored at the MINUTE: the
// projector stores every row on the fixed UTC minute grid, and queries
// coarsen minute rows to the allowed larger fixed UTC buckets (minute, hour,
// day). Agent is NOT a rollup dimension — none of the V1 canonical payloads
// carry an authoritative agent id, and an empty axis is not shipped (a
// group_by of "agent" is rejected loudly).
//
// It is the durable, queryable counterpart of the event bus's live metrics
// derivation — metrics labels stay low-cardinality by the telemetry
// cardinality firewall, while rollups carry the identity dimensions the
// firewall deliberately excludes, stored as rows in a Store rather than as
// OTel metric labels. Rollups do NOT register identity-labelled OTel metrics;
// the identity dimensions exist only in the rollup rows.
//
// # Precision model
//
// Every measure is accumulated, stored, and queried in exact integer form.
// Counts, tokens, and latency are plain int64; cost is integer micro-units of
// USD (CostScaleMicros). The source float cost is converted to micro-units
// EXACTLY ONCE per canonical event, in Extract, with strict
// finite/nonnegative/range checks and deterministic rounding (see
// microsFromUSD). Nothing is ever accumulated or stored as float64, and query
// results carry typed integer MeasureValue — counters above 2^53 stay exact.
// A consumer formats decimal USD at the edge as N / CostScaleMicros.
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
// records, and there is no exactly-once claim. The projector is a DOWNSTREAM
// consumer: the durable log persisted and fanned out each event before the
// projector read it, so a projector failure (reported in Quality as
// StateUnavailable and retried on the next Advance) never fails the
// already-successful canonical event publication. Cost and token measures are
// exact integer aggregates of the provider-reported values carried by the
// canonical `llm.cost.recorded` event; authoritative per-call accounting
// remains in-band in the governance subsystem. The projector makes NO
// active-active claim: the Store has a single writer, the Projector, and no
// cross-runtime consistency is promised. Erased sessions are fenced
// PERMANENTLY (rows deleted, late events refused, Rebuild never clears the
// fence), so an erasure is never resurrected by an asynchronous tail event or
// by reprojection.
//
// # Layout
//
//   - bucket.go — the fixed UTC bucket grid (minute, hour, day).
//   - dimension.go — the closed dimension set (tenant / user / session /
//     model — agent is absent by design).
//   - measure.go — the closed, additive measure set (integer-only; cost in
//     micro-units; latency count/sum/min/max).
//   - key.go — the row key (minute bucket + authoritative dimension values)
//     and the comparable SessionTriple fence key.
//   - extract.go — the pure event → deltas extractor (source-backed measures).
//   - query.go — the typed, validated read surface + deterministic pagination
//     (typed integer/decimal measure values).
//   - store.go — the mandatory Store interface + sentinels (permanent
//     erasure fences; no unfence operation).
//   - projector.go — the checkpointed projector + honest quality surface.
//   - memstore/ — the indexed in-memory Store implementation (the reference
//     driver the conformance suite exercises; SQLite / Postgres
//     implementations consume the same interface + suite).
//   - conformancetest/ — the conformance suite every Store implementation
//     must pass.
package rollups
