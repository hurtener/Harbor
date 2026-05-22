package llm

// posture.go — the Phase 72g (D-112) read-only posture accessor over the
// runtime's bound LLM provider. The `llm.posture` Protocol method (Phase
// 72g) consumes a PostureProvider; the provider returns a PostureSnapshot
// carrying the provider name, model id, region, and the `MockMode`
// boolean.
//
// # The MockMode capture path (D-089)
//
// `MockMode == true` iff the runtime booted with the dev-only mock
// escape hatch `HARBOR_DEV_ALLOW_MOCK=1`. The flag is captured ONCE at
// boot via `RegisterMockModeCaptured`, called from the SAME call site in
// `cmd/harbor/devmock.go::registerMockIfDevAllowMock` that prints the
// `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` stderr banner. The
// posture handler reads the captured boolean — it NEVER re-reads
// `os.Getenv("HARBOR_DEV_ALLOW_MOCK")` at request time. D-089's boot-
// time capture is the single source of truth; a request-time re-read
// would be a second source that could silently desync from the banner.
//
// The capture flag is package-level mutable state. CLAUDE.md §5 rule 5
// permits package-level mutable state for "driver registries (write-
// once, read-many)" — the mock-mode flag is the same shape: written
// exactly once at boot before any request is served, read-many
// thereafter. It is guarded by an atomic so the write/read pair is
// race-free even if a test exercises the capture path concurrently.
//
// # Concurrent reuse (D-025)
//
// PostureProvider is immutable after construction — its ConfigSnapshot
// is set once and never mutated. `Posture` reads the snapshot's
// provider/model fields and the atomic mock-mode flag; it holds no
// per-request state. Safe to share across N concurrent goroutines.

import (
	"context"
	"sync/atomic"
)

// mockModeCaptured records whether the runtime booted with the dev-only
// mock escape hatch (`HARBOR_DEV_ALLOW_MOCK=1`). It is written exactly
// once at boot via RegisterMockModeCaptured (CLAUDE.md §5 rule 5 —
// write-once-at-boot package state, the same posture as the driver
// registry) and read-many by every `llm.posture` request. The atomic
// makes the write/read pair race-free.
var mockModeCaptured atomic.Bool

// RegisterMockModeCaptured records that the runtime booted with
// `HARBOR_DEV_ALLOW_MOCK=1` (D-089). It is called exactly once from
// `cmd/harbor/devmock.go::registerMockIfDevAllowMock` at boot — the SAME
// call site that prints the `[DEV-ONLY MOCK LLM — DO NOT USE IN
// PRODUCTION]` stderr banner. Calling it with `true` flips the captured
// flag so `llm.posture` surfaces `MockMode: true`; calling it with
// `false` (or never calling it — the zero value) leaves the flag false.
//
// A future PR that re-routes the dev-hatch path (e.g. promotes the env
// var to a CLI flag) MUST keep this call reciprocal with the banner
// emit — otherwise `LLMPostureResponse.MockMode` silently desyncs from
// the banner. The Phase 72g integration + smoke tests assert both paths
// fire together.
func RegisterMockModeCaptured(v bool) {
	mockModeCaptured.Store(v)
}

// resetMockModeCapturedForTesting clears the captured mock-mode flag.
// Used only by package-internal tests that exercise both the captured
// and uncaptured posture paths; the flag is otherwise write-once-at-boot.
//

func resetMockModeCapturedForTesting() {
	mockModeCaptured.Store(false)
}

// PostureSnapshot is the read-only view of the runtime's bound LLM
// provider. It is the source the `llm.posture` Protocol handler projects
// onto the `LLMPostureResponse` wire type.
type PostureSnapshot struct {
	// Provider is the LLM provider name (e.g. "bifrost", "mock").
	Provider string
	// Model is the bound model identifier (e.g. "openai/gpt-5.3-chat").
	Model string
	// Region is the provider endpoint region; "" when not applicable.
	Region string
	// MockMode is true iff the runtime booted with HARBOR_DEV_ALLOW_MOCK=1
	// (D-089). Captured at boot via RegisterMockModeCaptured.
	MockMode bool
}

// PostureProvider is the Phase 72g read-only accessor over the runtime's
// bound LLM configuration. Built once per Runtime process via
// NewPostureProvider; `Posture` is safe for concurrent use by N
// goroutines (D-025).
type PostureProvider struct {
	provider string
	model    string
	region   string
}

// NewPostureProvider builds a PostureProvider over the LLM
// ConfigSnapshot the binary resolved at boot. The provider / model /
// region are read from the snapshot and frozen at construction; the
// `MockMode` flag is NOT taken from the snapshot — it is read live (but
// race-free) from the boot-captured atomic, so the posture surface
// reflects D-089's single capture-path source.
//
// When the snapshot's `Driver` field is empty it is normalised to
// `DefaultDriver` ("bifrost") — the same default `Open` applies — so the
// posture surface never reports an empty provider for a default-driver
// boot.
func NewPostureProvider(cfg ConfigSnapshot) *PostureProvider {
	provider := cfg.Provider
	if provider == "" {
		// When no explicit provider is configured, fall back to the
		// driver name — for the mock driver `Provider` is unset, so the
		// posture surface reports "mock" via the driver name. For
		// bifrost with a configured provider, `Provider` wins.
		provider = cfg.Driver
		if provider == "" {
			provider = DefaultDriver
		}
	}
	return &PostureProvider{
		provider: provider,
		model:    cfg.Model,
		// Region is the provider endpoint region. Bifrost-side, "region"
		// is not a first-class concept on every provider — OpenAI direct
		// has US/EU, OpenRouter routes through bifrost-internal regions.
		// When a custom provider declares an explicit BaseURL the
		// operator picked an endpoint, so we surface it as the region
		// hint; for the native-default endpoints (and the mock driver)
		// Region stays "" and the Console renders an em-dash placeholder.
		region: cfg.BaseURL,
	}
}

// Posture returns the read-only PostureSnapshot of the runtime's bound
// LLM provider for the caller. The `ctx` is accepted for signature
// symmetry with `governance.PostureProvider.Posture` and so a future
// per-tenant LLM-routing model can scope the read; V1 ships a single
// provider per Harbor instance (RFC §6.15 + D-088), so the snapshot is
// identity-independent at this layer — the Protocol handler is the
// identity-mandatory gate.
//
// `MockMode` is read from the boot-captured atomic (D-089) — NOT from an
// `os.Getenv` re-read.
func (p *PostureProvider) Posture(_ context.Context) (PostureSnapshot, error) {
	return PostureSnapshot{
		Provider: p.provider,
		Model:    p.model,
		Region:   p.region,
		MockMode: mockModeCaptured.Load(),
	}, nil
}
