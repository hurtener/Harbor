package serve

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// llm_provider_installer.go — the production concrete that installs /
// uninstalls a Protocol-installed, ZERO-URL broker-pull INFERENCE provider
// binding, and boot-connects a config-declared brokered primary. It is the
// §4.4 boundary glue: it imports the concrete inference credential-source
// (allowed in cmd/harbor + devstack) and satisfies the driver-agnostic
// agentcfgprotocol.LLMProviderInstaller the agent-config service depends on.
//
// Every credential-sink-determining value (the pull endpoint, audience,
// scope ceiling) is read from the boot-declared inference broker (resolved
// from the descriptor's non-secret inference_broker NAME) — NONE from the
// wire descriptor — so no admin-writable field determines where the
// credential is sourced.
//
// # Lifecycle
//
// Each installed binding owns an [credsource.InferenceKeySource] over the
// runtime's SHARED LiveKey (the exact holder the bifrost Account reads) plus
// a ctx-cancellable refresh scheduler goroutine (joined on uninstall /
// shutdown). Install kicks an async first pull + the refresh loop (parity
// with the OAuth `remote` source's lazy credential resolution — the binding
// is installed immediately; the pull is best-effort-then-fail-loud in the
// loop). The boot-declared brokered primary instead connects SYNCHRONOUSLY
// (BootConnectPrimary) so a misconfigured primary fails the boot loud.

// LLMProviderInstaller manages the runtime-level set of Protocol-installed
// inference provider bindings. Because a provider KEY is a runtime-level
// credential (not identity-scoped), the set is keyed by binding NAME, not by
// owner tuple — the install/uninstall verbs feed the one shared LiveKey.
//
// Concurrent reuse: the immutable config is set once at construction; the
// managed-source map is guarded by mu. Per-run identity is never read from
// the installer.
type LLMProviderInstaller struct {
	liveKey         *llm.LiveKey
	primaryProvider string
	brokers         map[string]config.InferenceBrokerConfig
	bus             events.EventBus
	redactor        audit.Redactor
	principal       string
	logger          *slog.Logger

	mu      sync.Mutex
	managed map[string]*managedLLMSource
	closed  bool
}

// managedLLMSource is one installed binding's source + its refresh goroutine.
type managedLLMSource struct {
	src    *credsource.InferenceKeySource
	cancel context.CancelFunc
	done   chan struct{}
}

// NewLLMProviderInstaller builds the production installer. liveKey is
// mandatory (the shared holder the bifrost Account reads); a nil returns a
// nil installer so the caller leaves the verb unwired (→ 501 at the wire
// edge) rather than nil-panicking. primaryProvider is the runtime's
// configured provider — a binding for any other provider is rejected (V1 is
// single-provider). principal is the per-runtime service-principal id for the
// pull audit event.
func NewLLMProviderInstaller(liveKey *llm.LiveKey, primaryProvider string, brokers []config.InferenceBrokerConfig, bus events.EventBus, redactor audit.Redactor, principal string, logger *slog.Logger) *LLMProviderInstaller {
	if liveKey == nil || bus == nil || redactor == nil {
		return nil
	}
	byName := make(map[string]config.InferenceBrokerConfig, len(brokers))
	for _, b := range brokers {
		byName[b.Name] = b
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMProviderInstaller{
		liveKey:         liveKey,
		primaryProvider: primaryProvider,
		brokers:         byName,
		bus:             bus,
		redactor:        redactor,
		principal:       principal,
		managed:         make(map[string]*managedLLMSource),
		logger:          logger,
	}
}

// BrokerNames returns the boot-declared inference-broker names (for the
// agent-config service's WithInferenceBrokers unknown-broker gate).
func (i *LLMProviderInstaller) BrokerNames() []string {
	names := make([]string, 0, len(i.brokers))
	for n := range i.brokers {
		names = append(names, n)
	}
	return names
}

// buildSource resolves the descriptor's inference_broker against the boot
// broker set and constructs an InferenceKeySource over the shared LiveKey.
// An unknown broker or a provider mismatch fails loud with the agent-config
// service's client-error sentinels (→ 400).
func (i *LLMProviderInstaller) buildSource(provider, brokerName string) (*credsource.InferenceKeySource, error) {
	broker, ok := i.brokers[brokerName]
	if !ok {
		return nil, fmt.Errorf("%w: %q", agentcfgprotocol.ErrLLMProviderBrokerUnknown, brokerName)
	}
	if provider != i.primaryProvider {
		return nil, fmt.Errorf("%w: provider %q is not this runtime's configured provider %q (V1 binds the single primary provider's key)",
			agentcfgprotocol.ErrInvalidLLMProvider, provider, i.primaryProvider)
	}
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider:         provider,
		BrokerName:       brokerName,
		RuntimePrincipal: i.principal,
		CredentialURL:    broker.CredentialURL,
		AuthTokenEnv:     broker.AuthTokenEnv,
		CacheTTL:         broker.CacheTTL,
		Timeout:          broker.Timeout,
		Live:             i.liveKey,
		Bus:              i.bus,
		Redactor:         i.redactor,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidLLMProvider, err)
	}
	return src, nil
}

// InstallLLMProvider installs (upserts) / rotates the named binding: it
// builds the broker-pull source over the shared LiveKey and starts an async
// first pull + refresh scheduler. A re-install by the same name replaces
// (closes) the prior binding (the rotate path). The pull is best-effort at
// install time and fail-loud in the loop (parity with the OAuth lazy
// credential resolution) — the binding is installed immediately.
func (i *LLMProviderInstaller) InstallLLMProvider(_ context.Context, _, _ string, desc agentcfg.LLMProviderDescriptor) error {
	src, err := i.buildSource(desc.Provider, desc.InferenceBroker)
	if err != nil {
		return err
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		// The source runs on a runtime-lifetime ctx (below), not the install
		// request's ctx, so a shutting-down installer refuses new bindings.
		return fmt.Errorf("%w: installer is shutting down", agentcfgprotocol.ErrLLMProviderInstallUnavailable)
	}
	if prior, ok := i.managed[desc.Name]; ok {
		i.stopManagedLocked(prior)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m := &managedLLMSource{src: src, cancel: cancel, done: make(chan struct{})}
	i.managed[desc.Name] = m
	i.mu.Unlock()

	go func() {
		defer close(m.done)
		// Best-effort first pull (audited + fail-loud internally); the loop
		// keeps it fresh and retries on failure.
		if cErr := src.Connect(runCtx); cErr != nil {
			i.logger.WarnContext(runCtx, "llm provider install: initial broker pull failed (the refresh loop will retry)",
				slog.String("binding", desc.Name), slog.String("broker", desc.InferenceBroker), slog.String("error", cErr.Error()))
		}
		src.Run(runCtx)
	}()
	return nil
}

// UninstallLLMProvider removes the named binding and CLOSES its source — the
// live key is zeroed so a subsequently-bound call fails loud rather than
// serving the old key. A missing name is an idempotent no-op.
func (i *LLMProviderInstaller) UninstallLLMProvider(_ context.Context, name string) error {
	i.mu.Lock()
	m, ok := i.managed[name]
	if ok {
		delete(i.managed, name)
	}
	i.mu.Unlock()
	if ok {
		i.stopManaged(m)
	}
	return nil
}

// BootConnectPrimary connects a config-declared brokered PRIMARY
// (`llm.credential_source: remote`) SYNCHRONOUSLY and fails loud on
// broker-unreachable — the boot exits non-zero naming the broker + missing
// config (never a silent empty LiveKey). It then starts the refresh
// scheduler. Called once at boot when the primary is brokered.
func (i *LLMProviderInstaller) BootConnectPrimary(ctx context.Context, brokerName string) error {
	src, err := i.buildSource(i.primaryProvider, brokerName)
	if err != nil {
		return fmt.Errorf("boot brokered primary %q: %w", i.primaryProvider, err)
	}
	if cErr := src.Connect(ctx); cErr != nil {
		return fmt.Errorf("boot brokered primary %q: initial broker pull failed (fix llm.inference_brokers[%q] / its auth_token_env, or set llm.credential_source: local): %w",
			i.primaryProvider, brokerName, cErr)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m := &managedLLMSource{src: src, cancel: cancel, done: make(chan struct{})}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		cancel()
		src.Close()
		return fmt.Errorf("%w: installer is shutting down", agentcfgprotocol.ErrLLMProviderInstallUnavailable)
	}
	i.managed["primary"] = m
	i.mu.Unlock()
	go func() {
		defer close(m.done)
		src.Run(runCtx)
	}()
	return nil
}

// Close stops every managed binding's refresh goroutine + closes its source
// (zeroing the LiveKey). Joined on runtime shutdown.
func (i *LLMProviderInstaller) Close(context.Context) error {
	i.mu.Lock()
	i.closed = true
	managed := i.managed
	i.managed = make(map[string]*managedLLMSource)
	i.mu.Unlock()
	for _, m := range managed {
		i.stopManaged(m)
	}
	return nil
}

// stopManaged cancels the refresh goroutine, joins it, and closes the source.
func (i *LLMProviderInstaller) stopManaged(m *managedLLMSource) {
	m.cancel()
	<-m.done
	m.src.Close()
}

// stopManagedLocked is stopManaged for the re-install path (caller holds mu);
// it must not block on the join under the lock, so it spawns the teardown.
func (i *LLMProviderInstaller) stopManagedLocked(m *managedLLMSource) {
	go i.stopManaged(m)
}
