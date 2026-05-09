## Summary
<!-- 1-3 sentences. What changed and why. -->

## Phase / RFC reference
<!-- e.g. Phase 02 (planner interface) — RFC §6.2 -->

## Checklist
<!-- Tick what applies; mark N/A with reason; mark deferred items with an issue link. -->
- [ ] `make vet test build` passes locally (or skipped: no Go sources yet)
- [ ] `make lint` is clean (or skipped: no Go sources yet)
- [ ] `make preflight` passes locally
- [ ] `make check-mirror` clean
- [ ] If new endpoint/method/CLI subcommand: smoke check added in `scripts/smoke/phase-NN.sh`
- [ ] Coverage on touched packages ≥ phase target
- [ ] If multi-isolation code: cross-session isolation test passes
- [ ] If Protocol types changed: all references compile
- [ ] If config schema changed: example configs updated, backward compat verified
- [ ] If migrations added: clean and existing-DB tests pass
- [ ] No new TODO without issue link
- [ ] No new dependency without one-liner rationale below

## Risk / blast radius
<!-- What could go wrong with this change? -->

## Notes for reviewers
<!-- Optional. Anything you want highlighted. -->
