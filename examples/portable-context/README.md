# Portable session context editing sample

A small public-SDK agent for testing retained context against a Bifrost-supported
provider. It edits one application-owned HTML document with two tools:
`document_read` and `document_replace`. There is no additional service or private
MCP schema. Separate program invocations reopen the same SQLite store, so this
is not a demonstration that only works while one process stays alive.

Use synthetic test data and a private disposable directory. Live runs send
prompts and document content to the selected provider and may incur charges,
including summarization and normal runtime retry calls. The program never picks
a provider/model for you. Stowage is not required and is not configured here.

## Build and select a model

From the repository root, using the Go version required by `go.mod`:

```sh
go build -o ./bin/context-lab ./examples/portable-context
export PORTABLE_CONTEXT_API_KEY='your-test-provider-key'
export PROVIDER='your-bifrost-provider'
export MODEL='your-authorized-model-id'
export WINDOW='the-verified-context-window-as-an-integer'
```

Replace all three provider/model/window placeholders. Use a model supporting
ordinary text completion and native tool calling through the pinned Bifrost
version. Provider-native compaction, cache hints and native structured output
are not required. Check the provider's current model limits; the sample does
not discover or guess them. `-max-output` defaults to 2,048 and must be valid
for the selected model. For a trusted compatible endpoint, `-base-url` is
available; it is a credential destination, not an untrusted model argument.

This is a headless SDK example, so it uses `ValidateCore` and does not configure
a Protocol JWT listener. It builds its config from flags, not the full YAML or
`HARBOR_*` override layer. It deliberately selects SQLite state, filesystem
artifacts, `memory.strategy: rolling_summary`, and `memory.recent_turns: 8`.
There is no separate retention switch. Ordinary YAML and `config.Defaults()`
now select rolling memory with 20 detailed turns; this sample uses eight to
exercise rollover sooner. Set `memory.strategy: none` for a stateless agent.
The current branch is still retiring legacy memory-store interfaces.

## Run three distinct user turns

The same tenant, user, session and data directory select the same document and
retained conversation. Omitting identity flags uses `context-lab/tester/editing`.
The initial HTML source is exactly 14,660 bytes, version 7, with an unmodified
footer and a `Draft` heading. `more:false` in a read means the complete source.

```sh
./bin/context-lab -provider "$PROVIDER" -model "$MODEL" \
  -context-window "$WINDOW" -data-dir /tmp/harbor-context-lab \
  -prompt 'Read sample-document completely. Identify its version, heading and footer. Do not edit it.'

./bin/context-lab -provider "$PROVIDER" -model "$MODEL" \
  -context-window "$WINDOW" -data-dir /tmp/harbor-context-lab \
  -prompt 'Change the heading from Draft to Approved in the document we just read. Preserve everything else. Use its observed version and exact source; reread only when needed for correctness.'

./bin/context-lab -provider "$PROVIDER" -model "$MODEL" \
  -context-window "$WINDOW" -data-dir /tmp/harbor-context-lab \
  -prompt 'Now change the navigation label Overview to Details. Preserve the approved heading and the existing footer.'
```

For a more aggressive compaction exercise, add `-token-budget 2000` to every
invocation. This is the working-input target, not the physical model capacity
or a total-cost allowance. A protected fresh exchange may exceed this target;
physical input/output limits still apply. A low target alone does not guarantee
a compaction at every step. The example caps a run at 12 planner steps; a run
can also make bounded maintenance and retry calls. These are not dollar limits.

The output reports the answer, run ID and tool count, plus at most 64
`llm.context.prepared` payloads from the existing event bus. Availability and
truncation flags are explicit. These content-free preparation diagnostics are
best effort, not billing or provider-success receipts. A model may still
make mistakes; inspect the actual stored document rather than accepting its
claim that an edit succeeded.

## Inspect without any inference

```sh
./bin/context-lab -data-dir /tmp/harbor-context-lab -inspect > /tmp/context-document.json
python3 - <<'PY'
import json
from pathlib import Path
p = json.loads(Path('/tmp/context-document.json').read_text())
assert p['resource_id'] == 'sample-document'
assert p['version'] == 9, p['version']
assert '<h1>Approved</h1>' in p['source']
assert '<nav>Details</nav>' in p['source']
assert '<footer>Keep this footer</footer>' in p['source']
Path('/tmp/context-document.html').write_text(p['source'])
print('Both expected edits are present; the earlier edit and footer survived.')
PY
```

Version 9 assumes exactly one accepted replacement in each editing turn. Extra
accepted edits advance the version, so a different number needs investigation,
not automatic acceptance. Rendering and interaction are deliberately marked
unverified in save receipts; open the exported HTML to check appearance. This
small static fixture does not claim to exercise complex interactive mockups.

A stale expected version or a nonunique exact match is refused. Concurrent
writers use StateStore generation preconditions; one cannot overwrite the
other's successful edit from the same version. A write acknowledgement error is
not proof that nothing happened: inspect before retrying. The example never
automatically retries the whole run or an uncertain document save.

To test isolation, use a different `-session` (or tenant/user); it gets its own
new version-7 document. To test provider switching, keep the original identity
and data directory and select another compatible provider/model with its correct
window. Unset the old provider key and supply the new authorized key as needed.
The state is portable; quality or identical decisions across models is not promised.

## Deterministic acceptance without paid calls

```sh
go test -race ./examples/portable-context -count=1
```

Tests use an ordinary scripted local HTTP/SSE endpoint with the real Bifrost
adapter, runtime, compaction, tool dispatch and SQLite. They verify separate
stack lifecycles, a compacted source-bound checkpoint, the exact persisted edit,
and inspection that makes no provider request. Two independent SQLite pools
also exercise 128 session-scoped competing writers. Synthetic provider output
proves harness behavior, not a real model's proficiency or cache savings.

The sample consumes the public `sdk/state.SlotExpectation` and
`ErrConditionFailed` aliases for the existing conditional-write API. It introduces
no new store or concurrency mechanism into Harbor.
