// Package conformancefork is the in-tree worked example for certifying
// a Protocol-server fork (or embedder assembly) against Harbor's
// conformance suite.
//
// The conformance suite (internal/protocol/conformance) is the single
// binding pass/fail definition of "the Harbor Protocol surface works at
// the pinned version". It is deliberately NOT published as an
// importable package or a standalone certification runner — it lives
// under internal/ and runs with `go test` from a clone of the Harbor
// repository. Its entry point, conformance.RunSuite(t, factory), is
// bound to *testing.T, so the way an implementer certifies their own
// assembly is to write a `go test`-compiled harness that wires a
// custom conformance.Factory and hands it to RunSuite — NOT a runnable
// client binary like the sibling event-viewer example.
//
// This package is exactly that harness. The runnable code lives in
// conformance_fork_test.go: a custom Factory (forkFactory) that
// assembles the real-driver runtime surface a fork would ship — its own
// event bus, state store, task registry, control surface, wire mux, and
// JWT validator over a real ES256 keypair — and returns a fresh
// *conformance.Stack per subtest. TestConformanceFork then runs the
// FULL suite against that custom Factory.
//
// # Why a custom Factory and not the default one
//
// Harbor's own gate is one line over the canonical factory:
//
//	func TestProtocol_Conformance(t *testing.T) {
//	    conformance.RunSuite(t, conformance.NewDefaultFactory(""))
//	}
//
// That certifies Harbor's canonical assembly. A fork — a new transport,
// a different driver composition, a vendored surface — does NOT want the
// canonical assembly; it wants to certify ITS OWN. The Factory seam is
// the substitution point: a Factory is just
// `func(t *testing.T) *conformance.Stack`, and the Stack's fields are
// the components a fork swaps (Surface, Mux, Bus, the token-minting
// closures). This example shows the full wiring an implementer copies
// and then replaces component-by-component with their own.
//
// # The suite is the gate (no self-consistent fixture)
//
// The forkFactory does not assert its own correctness in isolation. The
// gate is RunSuite running green: if the custom assembly mis-wires the
// auth pipeline, the version handshake, or the wire status mapping, the
// suite FAILS. The worked example is therefore guarded by the same
// exhaustive contract Harbor's own assembly is — a hand-built stack that
// drifts from the real surface cannot pass.
//
// # Note on imports
//
// Unlike the SDK-free event-viewer example, this harness imports Harbor
// internal packages on purpose: a runtime fork or embedder assembles
// Harbor's runtime surface and certifies that assembly in-tree. That is
// the documented posture for forks and embedders — certify through the
// Factory seam, in the repository, with `go test`.
package conformancefork
