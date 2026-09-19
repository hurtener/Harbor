# Portable compaction maintenance

This is an incremental implementation note for RFC 002 and
[phase 268](../plans/phase-268-portable-compaction.md), not a phase-completion or
release claim. It records the maintenance changes after the request-aware trigger.

## Capacity

The enclosing prepared request supplies a copied model profile and model controls.
Each summary completion reserves its own bounded output before choosing complete
chronological exchanges. System instructions, response schema and prior narrative
count against the same canonical input estimate. A byte limit remains an
independent constraint. No exchange or trailing receipt metadata is clipped to
make a chunk fit; an indivisible oversized exchange fails explicitly.

A run-bound route cannot silently change to a different summarization model.
Technical capacity is not authority: the final composed-client safety, routing,
governance and grant checks remain mandatory on every actual attempt.

## Grant preservation and child identity

Summary requests preserve the signed grant from the enclosing request, or the
existing run carrier for standalone callers. A supplied grant never disappears
into optional mode's ungoverned path. Every chunk passes through the existing
verifier, credential binding, durable reservation, settlement and receipt sink.
Missing required grants, invalid signatures, identity mismatches and unavailable
renewal fail before provider execution. Signed claims are not modified.

Within a planner step, completion ordinal K is in 1 through 16. The ordinary
step identity remains `root/step/S`; its maintenance completions use
`root/step/S/compaction/K`. A standalone granted call uses `root/compaction/K`.
The child nonce is the hexadecimal SHA-256 of the ordinary parent nonce, NUL,
`compaction`, NUL and canonical decimal K. The runtime installs the ordinal;
model-generated narrative cannot set it. Retry, downgrade and fallback numbers
retain their original meanings and are not borrowed as maintenance counters.

Reissuing the same granted maintenance coordinate keeps its durable identity;
it does not allocate another chargeable call after a response-loss replay.
The parent decision has a distinct reservation. Concurrent runs retain their
own immutable contexts and signed parent bindings.

## Receiver compatibility

No receipt fields or existing ordinary/legacy canonical bytes change. The
receipt-against-grant validator additionally recognizes only the exact bounded
maintenance-child derivation. Forged parent/step/nonce values, noncanonical
ordinals and arbitrary suffixes are rejected, even with a recomputed body hash.

**Receipt consumers must use a validator that understands this derivation before
granted compaction is enabled.** Older validators reject the new child identity;
there is no fallback to an unmetered call, modified signature or weakened verifier.
The existing SDK receipt validator delegates to this same implementation. This
is a coordinated semantic extension, not a claim that old receivers accept it.

## Validation and remaining gates

The new regressions reproduce both prior failures: required mode rejected the
summary's missing grant, while optional mode ran supplied-grant maintenance
without verification or receipts. Tests cover both modes, runtime-default and
coordinator-bound grants, real in-memory lease persistence, complete chunk
metering, exact receipt round trips, malformed/expired/wrong-identity grants,
forged receipt derivation, parent isolation and response-loss replay.

```sh
go test -race ./internal/llm ./internal/llm/grant ./internal/llm/leases -count=1
go test -race ./internal/llm ./internal/llm/grant \
  -run 'TestCompaction|TestWrap_GrantAttemptIdentity|TestUnmarshalCanonical' -count=2
```

These commands passed in the recovered local validation tree with Go 1.27.1 and
pinned vendor dependencies. The local tree does not yet contain every upstream
PR test, so this is not a full-PR validation claim. Broader provider-route/fallback
integration, complete source/test reconstruction, full preflight, downstream
receipt-consumer compatibility and durable cross-turn execution context remain
release gates. No paid provider call, deployment, merge or RC is claimed.
