# Portable context RC acceptance

RFC 002 / PR #779. This is the test procedure, not an announcement that an RC
has shipped. The live implementation/release tracker is
[portable-context-tracker.md](portable-context-tracker.md).

## Before selecting an RC tag

Normally, finish the tracked adversarial review and final-tree Go, Protocol,
frontend, PostgreSQL, drift and live preflight gates. For this PR's current RC
effort, the owner explicitly waived both local and hosted preflight to move more
quickly. Run every other applicable release gate, and record preflight as skipped,
never green. Remove temporary publication/source recovery workflows and payloads.
Record the exact source commit, toolchain, checks and artifact checksums. Do not
infer readiness from a successful patch transport, an old green run or a source
ZIP. Do not merge or create a release merely because these instructions exist.

## Minimal sample

Build and run [the portable-context sample](../../examples/portable-context/README.md)
from the candidate source. It uses the public SDK, two real example tools and
persistent SQLite, with no memory service. Follow its read/edit/edit/inspect
sequence, then repeat in a fresh test directory with its low working-input target.
Verify actual source, retained earlier edits, exact versions and declared outcome
boundaries. Repeat with a second compatible Bifrost provider using the correct
model capacity. Record unnecessary repeated reads separately from correctness
reads after a version change. Inference may be billable; the automated suite
instead uses synthetic local responses.

## Existing served agents

On a disposable deployment, explicitly set:

```yaml
sessions:
  retained_context_turns: 8
memory:
  strategy: none
planner:
  token_budget: 12000
```

Keep the deployment's existing provider, model, JWT, store and tool authority
configuration. Set the model profile's actual context and output limits as well.
The input target is not the physical limit or a price cap. Use a durable configured
StateStore and ArtifactStore to test restarts. Stowage remains an independent
integration; do not expose the trusted completion ingestion hook to the planner.

Watch the existing event stream for `trajectory.compressed`,
`trajectory.compression_failed`, `llm.context.prepared` and `llm.cost.recorded`.
Prepared-event counts describe Harbor's final capacity check, not proof of
provider receipt or billing. The structural categories share the admission
estimator. Optional history fields describe installed coverage; maintenance calls
must not inherit a parent range. Availability/estimate flags distinguish missing
normalized usage from known reports and Harbor backfills. A zero scalar cache
field alone does not prove the provider reported zero cached tokens.

Test a read followed by a dependent edit across compaction, a failed edit whose
error must reach the next request, source deletion, changing a tool's authority,
and attachment references before and after restart. Fresh evidence must remain
available, and removed authority must not be restored from history. Use authorized
artifact range reads for offloaded content; do not replace retrieval with rerunning
a historical write. Do not log raw prompts, results, credentials or arbitrary
provider errors merely to observe these checks.

## Interruption is not automatic action replay

For a fully settled retained journal, an embedder may explicitly call:

```go
err := stack.ReconcileRetainedContext(ctx, id, sourceRunID)
```

The served equivalent is `sessions.reconcile_context`, whose canonical REST
route is `POST /v1/sessions/reconcile_context`. Use the existing authenticated
own-session client and supply `source_run_id`; no private execution content is
returned. Reconciliation seals evidence and fences further dispatch by the old
admission. It does not cancel a provider request already in flight, restart an
agent, run a tool or call the completion hook.

A pending external intent remains unknown and is refused. Reconcile the external
resource using the owning service's status/version/idempotency mechanism before
any new write. There is no generic exactly-once guarantee or automatic clearance
of uncertain external outcomes. Failed required persistence is not a tool-retry
instruction. Do not claim recovery passed merely because a new run starts.

## Migration and rollback

Retained context defaults to zero. Enabling it is explicit consent to the richer
bounded session representation; `memory.strategy: none` is not equivalent consent.
Old pair-only answers cannot recreate missing tool receipts. Readers migrate
supported older retained-window versions without inventing summary coverage;
unknown, malformed or invalid covered evidence is rejected. Source expiry,
eviction and changed redaction invalidate derived checkpoints rather than
extending source lifetime.

For rollout, pin active runs to one tested binary and use disposable sessions
first. Do not downgrade a live retained deployment assuming that older binaries
understand new windows/journals or events. A safe rollback rehearsal must establish
version compatibility, or stop active work and use fresh sessions after preserving
any uncertain-operation evidence for reconciliation. Disabling retention does
not itself erase existing data; apply the existing authorized session-erasure and
storage retention procedures. Never hand-delete one journal record to make a
failed recovery appear successful.

## Outcome record

For each scenario record the exact candidate commit, provider/model/route,
configured input/output targets, prompt sequence, observed tool calls, actual
artifact differences, checkpoint events, restart boundary and human corrections.
Keep real-model outcomes separate from deterministic test results. Include all
maintenance/retry usage in cost analysis; request-prefix stability is not evidence
of provider cache hits, lower bills or superior task success.
