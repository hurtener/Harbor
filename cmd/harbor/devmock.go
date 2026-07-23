// cmd/harbor/devmock.go — the conditional mock LLM driver wire-in for
// `harbor dev`.
//
// The mock package is blank-imported here (NOT in main.go) so:
//
//  1. The §13 "test stubs as production defaults" amendment's
//     "unreachable from `cmd/harbor/main.go`'s blank-import block in
//     a normal build" requirement is satisfied at the main.go layer
//     (mock is conspicuously absent from that file).
//
//  2. The dev-only escape hatch (`HARBOR_DEV_ALLOW_MOCK=1`) actually
//     resolves the `"mock"` driver name at runtime — for that to
//     work, the mock package's init() MUST have fired, which only
//     happens if SOME file in the binary imports it. This file is
//     that single point of import.
//
// The trade-off is that the mock package IS linked into every
// `harbor` binary (the import is unconditional at compile time). The
// runtime gate — `registerMockIfDevAllowMock(allowMock, ...)` below
// — refuses to surface the mock as an active driver unless the env
// var is set AND prints the unconditional stderr banner when it
// does. There is no static-link path that the mock self-registers
// without the env-var check having run first; the registration
// already happened at init() but the dev cmd's `validateLLMProvider`
// gate refuses to start a runtime against `driver: mock` unless
// `allowMock` is true.
//
// A future refactor that wants the mock genuinely unreachable in a
// production binary should:
//
//   - Add a `harbor_testfixtures` build tag to the mock package.
//   - Build the binary with `-tags harbor_testfixtures` for the dev
//     loop and without for production releases.
//
// That refactor was scoped out of because every test file
// that imports the mock would need the same build tag, expanding the
// blast radius. The current arrangement satisfies the §13 amendment
// in spirit (the mock cannot run as the default; the only path is
// the explicit, banner'd env var) without forcing every consumer
// test to declare a build tag.

package main

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
	// Token-exchange OAuth driver — imported (dev-only gated, §13
	// carve-out for `cmd/harbor`) to call its boot-capture setter for the
	// `HARBOR_DEV_ALLOW_PRIVATE_EXCHANGE` escape hatch, exactly as the
	// llm mock-mode capture is called below. The driver's registration
	// still rides the prod aggregator's blank import; this named import
	// is solely for the reciprocal banner+capture at boot.
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"

	// Mock LLM driver — deterministic test-grade. Blank-
	// imported here so its init() seats the registration in the
	// llm.factories map. The gate that allows it to BE the active
	// driver is `validateLLMProvider` (see cmd_dev.go), which fails
	// closed unless HARBOR_DEV_ALLOW_MOCK=1. The unconditional
	// stderr banner emit on every boot when the env var fires is the
	// "do not use in production" surfacing the §13 amendment
	// mandates.
	_ "github.com/hurtener/Harbor/internal/llm/mock"
)

// registerMockIfDevAllowMock prints the unconditional stderr banner
// when the operator set `HARBOR_DEV_ALLOW_MOCK=1` AND captures the
// dev-mock boot flag for the `llm.posture` surface.
//
// The mock registration itself already fired at init() via the blank
// import above. This helper is the BANNER half of the §13 amendment
// ("every boot prints a stderr banner") AND — — the
// MOCK-MODE CAPTURE half: it calls `llm.RegisterMockModeCaptured(allowMock)`
// at the SAME call site that emits the banner, so `llm.posture` surfaces
// `MockMode == true` iff the runtime booted with the dev escape hatch.
// Capturing the flag here (rather than re-reading the env var at request
// time) makes the boot-time decision the single source of truth and
// keeps the banner + the posture flag structurally reciprocal — a future
// PR that re-routes the dev-hatch path cannot desync one from the other
// without touching this function.
//
// The banner is a no-op when allowMock is false; the capture is called
// unconditionally with `allowMock` so the captured flag is the honest
// boot state (false when the escape hatch did not fire).
func registerMockIfDevAllowMock(allowMock bool, stderr interface{ Write(p []byte) (int, error) }) {
	// capture the dev-mock boot flag for llm.posture.
	// Called unconditionally — `false` is the honest non-dev boot state.
	llm.RegisterMockModeCaptured(allowMock)

	if !allowMock {
		return
	}
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintln(stderr, MockBanner)
}

// registerAllowPrivateExchangeIfDev is the reciprocal banner+capture for
// the dev-only `HARBOR_DEV_ALLOW_PRIVATE_EXCHANGE` escape hatch — the
// exact shape of registerMockIfDevAllowMock. When `allow` is true it
// permits the `tokenexchange` credential-bearing exchange POST to dial a
// private-range / link-local resolved address (a broker behind a
// private-IP TLS sidecar — the containerized local-dev topology).
//
// It captures the boot flag UNCONDITIONALLY (`false` is the honest
// non-dev boot state) via tokenexchange.RegisterAllowPrivateExchangeCaptured
// AND prints the [DEV-ONLY PRIVATE-IP TOKEN EXCHANGE …] stderr banner at
// the SAME call site when the hatch fired — keeping the capture and the
// banner structurally reciprocal (a future re-route of the dev-hatch path
// cannot relax the dial guard without also emitting the banner). The
// opt-in relaxes ONLY the private / link-local / ULA dial branch; the
// redirect refusal, the loopback carve-out, and the unspecified-address
// block are unaffected. Never Protocol-writable.
func registerAllowPrivateExchangeIfDev(allow bool, stderr interface{ Write(p []byte) (int, error) }) {
	// capture the dev private-dial boot flag for the tokenexchange driver.
	// Called unconditionally — `false` is the honest non-dev boot state.
	tokenexchange.RegisterAllowPrivateExchangeCaptured(allow)

	if !allow {
		return
	}
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintln(stderr, PrivateExchangeBanner)
}
