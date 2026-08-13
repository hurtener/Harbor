# Phase 236f — Child artifact references and frozen output contracts (HA-59)

## Summary

Planner-spawned children forward artifact references through the existing task
fields. Every ID is authorized with the inherited `ScopedArtifacts` triple
before persistence; foreign and missing IDs are indistinguishable. Virtual
profiles own bounded input patterns/count/disposition and the frozen output
schema hash. Children reuse `CompileOutputSchema` and
`FinishAnswerEnvelope`; no model-authored schema, bytes, or URL is authoritative.

## Contract

- Artifact IDs are references only and resolve through the canonical materializer.
- Profile declarations are bounded and immutable for the child run.
- Terminal output is validated at the shared run edge and persisted as
  `answer_payload`; invalid output fails with `output_invalid`.
- Resolved bytes remain dispatch-local and never enter trajectory, events, audit,
  logs, or model context.

## Tests and smoke

Unit tests cover schema hash pinning and forged hashes. The implementation also
requires dispatch tests for same-scope forwarding, foreign/missing not-found
parity, profile limits, and output validation, plus a transcript fixture. The
phase smoke is static until the live child-run surface is available.

## References

- RFC §6.2, §6.5, §6.10
- D-415
- brief 02 and brief 05
