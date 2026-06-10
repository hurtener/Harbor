// phase112b_sdk_additions_test.go — Phase 112b (D-206): compile +
// resolution coverage for the facade packages the external-consumer
// conversions flushed out of 112a's inventory (RFC §3.6 item 3,
// amended): sdk/audit, sdk/telemetry (+ telemetry/eventbus),
// sdk/governance, sdk/tools/auth, sdk/skills/{importer,tools,
// generator}, and the ErrorClass vocabulary on sdk/tools.
//
// Same posture as the 112a integrity test's compile-coverage block:
// every exported name of the added packages is referenced below, so a
// re-export that stops resolving breaks this build. A handful of
// cheap runtime assertions prove the forwards actually forward (the
// production "patterns" redactor opens; the redactor-mandatory Logger
// constructs over a real bus; empty governance tiers stay latent; the
// skill retrieval handlers register on a real catalog).
//
// BINDING: this file MUST NOT import any internal/ package — the
// phase-112b external probe proves the same names from OUTSIDE the
// module; this in-module twin keeps the coverage in `make test`.
package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/sdk/drivers/prod"

	sdkaudit "github.com/hurtener/Harbor/sdk/audit"
	sdkconfig "github.com/hurtener/Harbor/sdk/config"
	sdkevents "github.com/hurtener/Harbor/sdk/events"
	sdkgovernance "github.com/hurtener/Harbor/sdk/governance"
	sdkskills "github.com/hurtener/Harbor/sdk/skills"
	sdkskillsgen "github.com/hurtener/Harbor/sdk/skills/generator"
	sdkskillsimporter "github.com/hurtener/Harbor/sdk/skills/importer"
	sdkskillstools "github.com/hurtener/Harbor/sdk/skills/tools"
	sdktelemetry "github.com/hurtener/Harbor/sdk/telemetry"
	sdkteleventbus "github.com/hurtener/Harbor/sdk/telemetry/eventbus"
	sdktools "github.com/hurtener/Harbor/sdk/tools"
	sdktoolsauth "github.com/hurtener/Harbor/sdk/tools/auth"
)

// TestPhase112b_FacadeAdditions_ResolveAndForward exercises the added
// packages' runtime-cheap paths end-to-end through the facade.
func TestPhase112b_FacadeAdditions_ResolveAndForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// sdk/audit — the production patterns redactor via the facade.
	red, err := sdkaudit.Open(ctx, sdkconfig.AuditConfig{})
	if err != nil {
		t.Fatalf("sdk/audit.Open: %v", err)
	}
	if got := sdkaudit.DefaultDriver; got != "patterns" {
		t.Fatalf("sdk/audit.DefaultDriver = %q, want patterns", got)
	}

	// sdk/events bus for the telemetry + skills legs.
	bus, err := sdkevents.Open(ctx, sdkconfig.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         32,
	}, red)
	if err != nil {
		t.Fatalf("sdk/events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	// sdk/telemetry + sdk/telemetry/eventbus — the redactor-mandatory
	// Logger over the production bus emitter.
	logger, err := sdktelemetry.New(sdkconfig.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "info",
		ServiceName: "phase112b-facade",
	}, red, sdktelemetry.WithBusEmitter(sdkteleventbus.New(bus)))
	if err != nil {
		t.Fatalf("sdk/telemetry.New: %v", err)
	}
	_ = logger

	// sdk/governance — empty tiers stay latent: (nil, nil).
	sub, err := sdkgovernance.NewSubsystemFromConfig(
		sdkgovernance.ConfigFromOperator(sdkconfig.GovernanceConfig{}), nil, nil)
	if err != nil {
		t.Fatalf("sdk/governance.NewSubsystemFromConfig (latent): %v", err)
	}
	if sub != nil {
		t.Fatal("sdk/governance: empty tiers must stay latent (nil Subsystem)")
	}

	// sdk/skills + sdk/skills/tools + sdk/skills/generator — the
	// retrieval handlers + generator register on a real catalog over a
	// real localdb store.
	//
	// ":memory:" is safe here since D-207: every sqlite-family driver
	// now translates ":memory:" to a PER-OPEN uniquely named memory
	// database (`file:harbor_<subsystem>_mem_<entropy>?mode=memory&cache=shared`),
	// so this skills store can no longer collide with any other
	// subsystem's parallel ":memory:" store on a shared
	// `schema_migrations` table (the pre-D-207 hazard this test
	// originally dodged with a file-backed DSN — observed as the
	// artifacts driver-parity test losing its `artifacts_blobs`
	// migration to the then-process-wide `file::memory:?cache=shared`
	// database).
	store, err := sdkskills.Open(ctx, sdkskills.ConfigSnapshot{
		Driver: sdkskills.DefaultDriver,
		DSN:    ":memory:",
	}, sdkskills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("sdk/skills.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	cat := sdktools.NewCatalog()
	if err := sdkskillstools.Register(cat, store, sdkskillstools.Deps{Bus: bus}); err != nil {
		t.Fatalf("sdk/skills/tools.Register: %v", err)
	}
	if err := sdkskillsgen.Register(cat, store, sdkskillsgen.Deps{Bus: bus, Redactor: red}); err != nil {
		t.Fatalf("sdk/skills/generator.Register: %v", err)
	}
	for _, name := range []string{"skill_search", "skill_get", "skill_list", "skill_propose"} {
		if _, found := cat.Resolve(name); !found {
			t.Fatalf("catalog missing %q after facade registration", name)
		}
	}

	// sdk/tools/auth — the callback handler constructs over an empty
	// provider map and is a plain http.Handler.
	h := sdktoolsauth.CallbackHandler(map[string]sdktoolsauth.OAuthProvider{})
	if _, isHandler := any(h).(http.Handler); !isHandler || h == nil {
		t.Fatal("sdk/tools/auth.CallbackHandler must return a non-nil http.Handler")
	}
}

// phase112bAdditionsCompileCoverage references every exported facade
// name of the 112b additions not already exercised above, so a
// re-export that stops resolving breaks this build (the 112a
// compile-coverage posture, extended).
//
//nolint:unused // compile-coverage by reference, mirrors the 112a integrity block
var phase112bAdditionsCompileCoverage = func() bool {
	// sdk/audit
	var _ sdkaudit.Redactor
	var _ sdkaudit.Rule
	_ = sdkaudit.OpenDriver
	_ = sdkaudit.RegisteredDrivers
	_ = sdkaudit.WithRedactor
	_ = sdkaudit.From
	_ = sdkaudit.MustFrom
	_ = sdkaudit.ErrUnknownDriver

	// sdk/telemetry
	var _ *sdktelemetry.Logger
	var _ sdktelemetry.Option
	var _ sdktelemetry.BusEmitter
	var _ *sdktelemetry.MetricsRegistry
	var _ sdktelemetry.MetricsOption
	var _ *sdktelemetry.Tracer
	var _ sdktelemetry.TracerOption
	_ = sdktelemetry.WithWriter
	_ = sdktelemetry.NewMetricsRegistry
	_ = sdktelemetry.BridgeBusToMetrics
	_ = sdktelemetry.PrometheusHandler
	_ = sdktelemetry.NewTracer
	_ = sdktelemetry.BridgeBusToTracer
	_ = sdktelemetry.DefaultTraceBridgeFilter
	_ = sdktelemetry.ErrLoggerNotConfigured
	_ = sdktelemetry.ErrRedactorMissing
	_ = sdktelemetry.ErrMetricsNotConfigured
	_ = sdktelemetry.ErrMetricExporterUnknown
	_ = sdktelemetry.ErrPrometheusHandlerUnavailable
	_ = sdktelemetry.ErrBridgeMisconfigured
	_ = sdktelemetry.ErrTraceBridgeMisconfigured

	// sdk/telemetry/eventbus
	var _ *sdkteleventbus.Adapter

	// sdk/governance
	var _ sdkgovernance.Subsystem
	var _ sdkgovernance.Config
	var _ sdkgovernance.TierConfig
	var _ sdkgovernance.RateLimitConfig
	var _ sdkgovernance.TierResolver
	var _ sdkgovernance.Clock
	var _ sdkgovernance.RealClock
	_ = sdkgovernance.Wrap
	_ = sdkgovernance.ErrBudgetExceeded
	_ = sdkgovernance.ErrRateLimited
	_ = sdkgovernance.ErrMaxTokensExceeded
	_ = sdkgovernance.ErrIdentityRequired

	// sdk/tools/auth
	var _ sdktoolsauth.CallbackOption
	var _ sdktoolsauth.PendingFlowInfo
	_ = sdktoolsauth.CallbackPath
	_ = sdktoolsauth.CallbackRoutePattern
	_ = sdktoolsauth.WithCallbackLogger
	_ = sdktoolsauth.WithSuccessPage
	_ = sdktoolsauth.ErrFlowNotFound
	_ = sdktoolsauth.ErrFlowExpired
	_ = sdktoolsauth.ErrStateMismatch

	// sdk/skills/importer
	var _ sdkskillsimporter.Deps
	var _ sdkskillsimporter.ImportReport
	var _ sdkskillsimporter.ImportStoreOption
	_ = sdkskillsimporter.ImportAndStore
	_ = sdkskillsimporter.WithOverwrite
	_ = sdkskillsimporter.ErrDuplicateSkillName
	_ = sdkskillsimporter.ErrMissingFrontmatter
	_ = sdkskillsimporter.ErrMalformedYAML
	_ = sdkskillsimporter.ErrMissingTrigger
	_ = sdkskillsimporter.ErrEmptySteps
	_ = sdkskillsimporter.ErrUnknownSection
	_ = sdkskillsimporter.ErrAttachmentOutsideRoot

	// sdk/skills/tools
	var _ sdkskillstools.CapabilityContext
	var _ sdkskillstools.SearchArgs
	var _ sdkskillstools.SearchResult
	var _ sdkskillstools.GetArgs
	var _ sdkskillstools.GetResult
	var _ sdkskillstools.ListArgs
	var _ sdkskillstools.ListResult
	_ = sdkskillstools.SearchHandler
	_ = sdkskillstools.GetHandler
	_ = sdkskillstools.ListHandler
	_ = sdkskillstools.ErrSkillTooLarge

	// sdk/tools — the 112b ErrorClass vocabulary.
	var _ sdktools.ErrorClass
	_ = sdktools.ErrClassTransient
	_ = sdktools.ErrClassTimeout
	_ = sdktools.ErrClass5xx
	_ = sdktools.ErrClassPermanent

	return true
}()
