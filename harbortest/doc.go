// Package harbortest is Harbor's public test kit — the ergonomic
// authoring surface for flow-level agent tests.
//
// The kit exposes five entry points (RFC §6.13, brief 06 §3 "What the
// test kit gives authors"):
//
//   - RunOnce(ctx, agent, input, deps...) (Output, *EventLog, error)
//     runs an Agent once under a deterministic identity quadruple
//     (or one the caller supplies) and returns every event the run
//     emitted.
//
//   - EventLog.RecordedEvents(runID) []events.Event returns the
//     events for one RunID; EventLog.All() returns the full capture.
//
//   - AssertSequence(t, log, want) verifies a list of EventTypes
//     appears as an ordered subsequence of the captured log.
//
//   - AssertNoLeaks(t, log) verifies no event tagged with one
//     identity triple references a RunID owned by a different
//     identity triple — the public, ergonomic version of Harbor's
//     cross-tenant/session isolation contract (CLAUDE.md §6).
//
//   - SimulateFailure(injector, toolName, class, n) schedules the
//     next n invocations of a wrapped tool to fail with the given
//     error class.
//
// The kit composes REAL drivers — the production in-mem events bus,
// the patterns audit redactor, the canonical tool catalog. No stub
// LLM ships here; CLAUDE.md §13 ("Test stubs as production defaults
// on operator-facing seams") rules out shipping a stub-by-default
// kit, even on a test-only surface. The Agent the caller provides is
// whatever they want exercised — a planner, a flow, a hand-rolled
// function. Wiring a mock LLM for the Agent's interior is the test
// author's concern.
//
// Concurrent reuse contract (CLAUDE.md §5, D-025). A constructed
// EventLog is concurrent-safe; RunOnce builds a fresh EventLog per
// invocation and is itself safe to call from N concurrent goroutines.
// The FaultInjector serialises access to its counter map so
// SimulateFailure can be called from any goroutine.
//
// The package lives at the top level (not under internal/) because
// the kit is meant to be imported by consumer test code OUTSIDE this
// module — Go's `internal/` rule would forbid that. The CLAUDE.md §3
// layout documents the new top-level harbortest/ directory; the same
// rationale (`golang.org/x/tools/go/analysis/analysistest` lives at a
// top-level path inside its module) drives Harbor's choice. The
// package name `harbortest` makes a production import grep-visible.
package harbortest
