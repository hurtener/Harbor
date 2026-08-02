// Package protocol implements the admin-scoped `agent_config.*` Protocol
// methods the Console agent-config control plane consumes:
//
//   - agent_config.get — read an agent's active config revision.
//   - agent_config.set_revision — write a new immutable revision.
//   - agent_config.list_revisions — the agent's revision chain.
//   - agent_config.diff — server-side compare of two revisions.
//   - agent_config.rollback — repoint the active pointer.
//   - agent_config.skills.{list,upsert,delete} — skills control (the
//     first consumer of the registry primitive; see skills.go).
//
// Every write is admin-scoped (the verified `auth.ScopeAdmin` claim,
// enforced at the wire handler — the agent-config authorization model) and identity-mandatory. A config
// edit applies to the agent's NEXT run (next-turn projection — never
// mid-flight, per the concurrent-reuse contract).
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the narrow `agentcfg.Registry` interface (the
// StateStore-backed concrete satisfies it) plus an optional
// `skills.SkillStore` for the durable/shared skills consumer and a narrow
// `SessionPersonalSkillController` for the agent-owned session tier; tests
// inject fakes. The
// Service owns wire validation + the wire↔domain mapping; the registry
// owns persistence + the revision events; the SkillStore owns the skill
// rows + the skill events.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's IdentityScope. An incomplete
// triple fails closed with ErrIdentityRequired. The handler overlays the
// verified identity onto the request body, so a caller cannot target
// another identity's config through this surface.
//
// # Concurrent reuse
//
// A constructed *Service is immutable after NewService and safe to share
// across N goroutines: it holds only the registry / skill-store references
// + a clock + logger. Per-call state lives in arguments and locals.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/skills"
)

// Sentinel errors the Service returns. The wire handler maps each onto a
// canonical Protocol Code + HTTP status; in-process callers compare with
// errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple or an empty agent id. Fails closed.
	ErrIdentityRequired = errors.New("agentcfg/protocol: identity scope incomplete")
	// ErrMisconfigured — NewService was called with a nil registry.
	ErrMisconfigured = errors.New("agentcfg/protocol: NewService missing a mandatory dependency")
	// ErrSkillsUnavailable — a skills method was called but its SkillStore or
	// session-personal controller was not wired into the Service.
	ErrSkillsUnavailable = errors.New("agentcfg/protocol: skills control not wired on this runtime")
	// ErrUnknownModel — a set_llm_params (or a set_revision carrying an
	// LLMParams.Model) named a model with no configured ModelProfile. The
	// per-agent model is validated at set time (parity with the tenant
	// model-swap), so an invalid model can never be persisted — fail loud,
	// never a silent fallback (CLAUDE.md §13).
	ErrUnknownModel = errors.New("agentcfg/protocol: unknown model (no configured ModelProfile)")
	// ErrInvalidLLMParams — a set_llm_params (or a set_revision carrying an
	// LLMParams section) supplied an out-of-range sampling value (temperature
	// outside [0,2], non-positive max-tokens, or an unknown reasoning-effort).
	// Validated at set time so an invalid value never reaches a run (parity
	// with runs.set_overrides), fail loud (CLAUDE.md §13).
	ErrInvalidLLMParams = errors.New("agentcfg/protocol: invalid LLM parameters")
	// ErrInvalidHooks — a set_revision carrying a hooks section supplied an
	// invalid run-completion hook (a negative timeout_ms). Validated at set
	// time so an invalid value is rejected loud BEFORE any registry write —
	// parity with the yaml validator's negative-timeout rejection; the
	// normalizer's negative→0 coercion is defence-in-depth behind this gate,
	// never the primary posture (CLAUDE.md §13).
	ErrInvalidHooks = errors.New("agentcfg/protocol: invalid hooks section")
	// ErrInvalidToolExposureLoading — a set_tool_exposure (or a set_revision
	// carrying a ToolExposure section) supplied a server_loading_modes /
	// tool_loading_modes entry with an empty key or a value outside
	// "always"|"deferred". Validated at set time BEFORE any registry write
	// so an invalid override can never be persisted — no revision, no
	// event — fail loud, never a silent drop (CLAUDE.md §13).
	ErrInvalidToolExposureLoading = errors.New("agentcfg/protocol: invalid tool-loading-mode override")
	// ErrInvalidNaming — a set_revision carrying a naming section supplied an
	// invalid session auto-naming policy: a negative after_turns / repeat_every
	// / max_repetitions, a max_title_len outside [8,200], a repeat_every > 0
	// with max_repetitions < 1 (no unlimited value exists), or an unknown
	// model. Validated at set time BEFORE any registry write so an invalid
	// policy can never be persisted — fail loud, never a silent clamp
	// (CLAUDE.md §13). The model is validated via the same validateModel path
	// as set_llm_params.
	ErrInvalidNaming = errors.New("agentcfg/protocol: invalid naming section")
	// ErrInvalidExtraSystemBlocks — a set_extra_system_blocks (or a
	// set_revision carrying a blocks section) supplied a block with an
	// empty / out-of-charset name, an empty body, or a DUPLICATE name.
	// Uniqueness is what makes remove-by-name well defined — it is the
	// whole composability property the section exists for — so a duplicate
	// is refused loud BEFORE any registry write rather than silently
	// de-duplicated (CLAUDE.md §13). The message names the offender and,
	// for a duplicate, BOTH offending positions.
	ErrInvalidExtraSystemBlocks = errors.New("agentcfg/protocol: invalid extra system blocks section")
)

// namingMaxTitleLenBounds is the inclusive [min,max] a set max_title_len must
// fall within. 0 is exempt (inherits the runtime default at run start).
const (
	namingMinTitleLen = 8
	namingMaxTitleLen = 200
)

// validLLMReasoningEffort is the canonical reasoning-effort taxonomy the
// per-agent LLM-params section accepts — the set the planner honours
// (off | low | medium | high).
var validLLMReasoningEffort = map[string]struct{}{
	"off": {}, "low": {}, "medium": {}, "high": {},
}

// validLoadingModes is the closed value set a loading-mode override may
// take — mirrors tools.LoadingAlways / tools.LoadingDeferred without an
// internal/tools import (the wire-edge validation stays package-local).
var validLoadingModes = map[string]struct{}{"always": {}, "deferred": {}}

// validateToolExposureLoading validates a tool-exposure section's
// loading-mode override maps at set time: every key must be non-empty and
// every value must be "always" or "deferred". A nil section is a no-op.
func validateToolExposureLoading(te *prototypes.AgentConfigToolExposure) error {
	if te == nil {
		return nil
	}
	if err := validateLoadingModeMap("server_loading_modes", te.ServerLoadingModes); err != nil {
		return err
	}
	return validateLoadingModeMap("tool_loading_modes", te.ToolLoadingModes)
}

// validateLoadingModeMap rejects an empty key or a value outside
// "always"|"deferred" in m, naming field in the error for an actionable
// message.
func validateLoadingModeMap(field string, m map[string]string) error {
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: %s has an empty key", ErrInvalidToolExposureLoading, field)
		}
		if _, ok := validLoadingModes[v]; !ok {
			return fmt.Errorf("%w: %s[%q] = %q (want always|deferred)", ErrInvalidToolExposureLoading, field, k, v)
		}
	}
	return nil
}

// Clock is the time source the Service stamps response timestamps from.
type Clock func() time.Time

// SessionPersonalSkillController is the sole mutation and read authority for
// one selected agent's session-personal skill tier. UpsertSessionSkill and
// DeleteSessionSkill each perform one controller-owned CAS; callers must not
// pair them with a legacy SkillStore or Overlay.PersonalSkills write.
// SessionSkills returns only ScopeSession rows for the supplied real identity
// triple and selected agent. Implementations preserve cutover and unstable-read
// sentinels so the Protocol edge can map them to their canonical 409 codes.
type SessionPersonalSkillController interface {
	SessionSkills(ctx context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, error)
	UpsertSessionSkill(ctx context.Context, id identity.Quadruple, agentID string, skill skills.Skill) error
	DeleteSessionSkill(ctx context.Context, id identity.Quadruple, agentID, name string) error
}

// Service implements the admin-scoped agent-config methods.
type Service struct {
	registry              agentcfg.Registry
	bootDefaultAgentID    string
	ensureBootLifecycle   agentcfg.BootLifecycleEnsurer
	skills                skills.SkillStore // optional — nil ⇒ shared/durable skills methods return ErrSkillsUnavailable
	sessionPersonalSkills SessionPersonalSkillController
	bus                   events.EventBus // optional — nil ⇒ tool-exposure edits emit no mcp.connection.* events
	logger                *slog.Logger
	now                   Clock

	// preparer drives the unpublished MCP prepare/persist/activate lifecycle.
	// Optional — nil ⇒ add_mcp_connection returns ErrConnectionAttachUnavailable
	// (→ 501 at the wire edge). The concrete that calls the MCP driver is
	// injected at the cmd/harbor + devstack boundary (the §4.4 boundary keeps
	// the concrete MCP driver out of this package).
	preparer ConnectionPreparer
	// detacher tears a just-attached MCP server back down when the add's
	// revision write then fails (the expected-revision precondition being the
	// reachable case). Optional — nil ⇒ the compensation logs the residual live
	// server LOUD rather than pretending it was undone (CLAUDE.md §13). The
	// concrete is the same object that satisfies ConnectionAttacher, injected at
	// the cmd/harbor + devstack boundary, so attach and compensating detach can
	// never drift onto different registries.
	detacher ConnectionDetacher
	// discoveryApplier applies a connection's OAuth-discovery cross-origin
	// allow-list to the LIVE MCP registry for
	// `agent_config.set_mcp_discovery_origins` (and the run-start
	// allowance-reconcile). Optional — nil ⇒ the write records the revision but
	// reports applied_live=false (no live registry wired). The concrete (which
	// imports the MCP driver) is injected at the cmd/harbor + devstack boundary;
	// this package depends only on the interface. The registry stays
	// process-global bare-name (never re-keyed by identity).
	discoveryApplier DiscoveryOriginApplier
	// providerInstaller installs / uninstalls a Protocol-installed, zero-URL
	// OAuth provider live into the owner-tagged provider set for
	// `agent_config.set_oauth_provider` / `remove_oauth_provider` (and the
	// run-start provider reconcile). Optional — nil ⇒ the two verbs return
	// ErrProviderInstallUnavailable (→ 501 at the wire edge). The concrete
	// (which imports the auth package + resolves the boot credential broker) is
	// injected at the cmd/harbor + devstack boundary; this package depends only
	// on the interface. Owner-tagged for reconcile scoping; resolution stays
	// process-global bare-name.
	providerInstaller ProviderInstaller
	providerPreparer  ProviderPreparer
	// llmProviderInstaller installs / uninstalls a Protocol-installed, zero-URL
	// broker-pull INFERENCE provider binding live into the owner-tagged provider
	// set for `agent_config.set_llm_provider`. Optional — nil ⇒ the verb returns
	// ErrLLMProviderInstallUnavailable (→ 501 at the wire edge). The concrete
	// (which resolves the boot inference broker + builds the InferenceKeySource)
	// is injected at the cmd/harbor + devstack boundary; this package depends
	// only on the interface.
	llmProviderInstaller LLMProviderInstaller
	// inferenceBrokers is the set of boot-declared inference-broker names
	// (`llm.inference_brokers[].name`). set_llm_provider rejects a descriptor
	// whose inference_broker is not in this set with ErrLLMProviderBrokerUnknown
	// (400) — no admin-writable field determines a credential sink (the credential-plane invariant). An
	// empty / nil set means NO broker is installable (every name is unknown →
	// 400), the fail-closed default. Populated at the cmd/harbor + devstack
	// boundary from the loaded config.
	inferenceBrokers map[string]struct{}
	// bootDeclaredOAuthProviders is the set of OAuth provider names declared in
	// the boot yaml (`tools.oauth_providers[].name`). set_oauth_provider /
	// remove_oauth_provider reject a name in this set with a DISTINCT loud error:
	// the verbs govern revisioned installed-provider state only, so a
	// boot-declared provider is edited in yaml + restart (boot wins). Populated
	// at the cmd/harbor + devstack boundary from the loaded config.
	bootDeclaredOAuthProviders map[string]struct{}
	// allowWireOAuthDescriptor is the effective DEV-ONLY, fail-closed opt-in that
	// permits set_oauth_provider / add_mcp_connection to carry a FULL OAuth
	// provider binding over the wire (token_url / audience / scopes / remote{})
	// instead of only a boot-declared provider NAME. Default false / fail-closed:
	// with it off (all of production) a wire descriptor carrying any
	// credential-sink field is REJECTED (the zero-URL name-only posture,
	// unchanged). The effective value is (tools.allow_wire_oauth_descriptor config
	// flag) OR (the HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR boot env), computed at the
	// cmd/harbor + devstack boundary and injected here — never Protocol-writable.
	allowWireOAuthDescriptor bool
	// allowWireInjection is the effective DEV-ONLY, fail-closed opt-in that
	// permits add_mcp_connection to carry a per-user credential-INJECTION mapping
	// (the `injection` object) for a receiver-style MCP server over the wire,
	// instead of only a boot-declared `tools.mcp_servers[].injection` block. It is
	// INDEPENDENT of allowWireOAuthDescriptor (an operator may enable one without
	// the other). Default false / fail-closed: with it off (all of production) a
	// connection carrying any injection field is REJECTED. The effective value is
	// (tools.allow_wire_injection config flag) OR (the HARBOR_ALLOW_WIRE_INJECTION
	// boot env), computed at the cmd/harbor + devstack boundary and injected here
	// — never Protocol-writable.
	allowWireInjection bool
	// coordinator is the unified pause/resume primitive an auth-required
	// attach parks on. Optional — nil ⇒ an auth-required attach fails loud
	// with ErrCoordinatorUnavailable rather than silently dropping the auth
	// requirement (CLAUDE.md §13).
	coordinator pauseresume.Coordinator
	// bootDeclaredMCPServers is the set of MCP server names declared in the
	// boot yaml (`tools.mcp_servers[].name`). `remove_mcp_connection` rejects
	// a name in this set with a DISTINCT loud error: the verb governs
	// revisioned state only, so a boot-declared server is edited in yaml +
	// restart, never removed through the control plane. A nil / empty set
	// means no yaml-declared servers (every unknown name is a plain
	// not-found). Populated at the cmd/harbor + devstack boundary from the
	// loaded config.
	bootDeclaredMCPServers map[string]struct{}

	// stdioAllowlist is the fail-closed set of permitted stdio commands
	// (matched on argv[0]). A stdio add whose command[0] is absent from this
	// set is rejected (an empty / nil set rejects EVERY stdio add — the
	// secure default). http adds are admin-scoped but not gated here.
	stdioAllowlist map[string]struct{}

	// sessionOverlay backs the session-safe (non-admin) lower tier: the
	// `agent_config.session.*` verbs write the session's user prompt layer and
	// narrow-only disable set. Personal-skill names are legacy migration input
	// only and are dynamically projected from sessionPersonalSkills. Optional —
	// nil ⇒ overlay-writing methods return ErrSessionOverlayUnavailable (→ 501
	// at the wire edge). Keyed by the REAL (tenant, user, session) triple, so it
	// is session-isolated by construction.
	sessionOverlay sessionoverlay.Store

	// validModels is the set of model names with a configured ModelProfile.
	// `set_llm_params` (and a `set_revision` carrying an LLMParams.Model)
	// validate a set model against it — an unknown model is rejected with
	// ErrUnknownModel (parity with the tenant model-swap). A nil / empty set
	// means model validation is disabled (no model is pinnable via this
	// service): the binary always wires the configured profiles, so an empty
	// set signals model-pinning is unavailable, never a silent accept-all.
	validModels map[string]struct{}

	// writeLocks serialises the read-modify-write of each write verb PER
	// OWNER. A convenience verb reads the active revision, rebuilds the sibling
	// sections from that snapshot, then writes a new revision — and the
	// registry is last-write-wins (no CAS). Without serialisation two
	// concurrent edits to the SAME owner (e.g. set_skills racing
	// set_tool_exposure) would each rebuild from the other's pre-write
	// snapshot, silently reverting the concurrent sibling change (an MCP pause
	// could vanish) and forking the parent chain. The Service is the sole
	// registry-writer, so an owner lock held across the whole read-modify-write
	// makes it atomic within the process. Cross-replica atomicity would need
	// StateStore CAS — out of scope while the registry is a single in-process
	// artifact.
	//
	// Owners are STRIPED across a fixed array of mutexes (keyed by hash) rather
	// than held in a per-owner map: the lock memory is constant regardless of
	// how many distinct owners ever write, instead of a map that grows with the
	// user population and is never reclaimed. The same owner always maps to the
	// same shard, so same-owner read-modify-write stays atomic; two distinct
	// owners that hash to the same shard serialise occasionally, which is benign
	// (config writes are infrequent, and a brief unrelated serialisation costs
	// nothing and never affects correctness).
	writeLocks [writeLockShards]sync.Mutex
}

// WithBootLifecycleEnsurer wires the production bootstrap authority into the
// session/user handler path. The handler calls it only after signed reach has
// authorized the effective target; this option itself never creates a named
// caller-selected agent.
func WithBootLifecycleEnsurer(defaultAgentID string, ensure agentcfg.BootLifecycleEnsurer) Option {
	return func(s *Service) {
		if defaultAgentID != "" && ensure != nil {
			s.bootDefaultAgentID = defaultAgentID
			s.ensureBootLifecycle = ensure
		}
	}
}

// EnsureBootLifecycle materialises the boot-declared default for a verified
// request identity. It is intentionally a no-op for named agents: their
// lifecycle is explicit configuration authority, never request-created state.
func (s *Service) EnsureBootLifecycle(ctx context.Context, scope prototypes.IdentityScope, agentID string) error {
	if s.ensureBootLifecycle == nil || agentID == "" || agentID != s.bootDefaultAgentID {
		return nil
	}
	id, err := identityFromScope(scope, agentID)
	if err != nil {
		return err
	}
	if err := s.ensureBootLifecycle(ctx, id, agentID); err != nil {
		return fmt.Errorf("ensure boot agent lifecycle: %w", err)
	}
	return nil
}

// writeLockShards bounds the per-owner write-lock memory. 256 shards keep
// cross-owner collisions rare for realistic owner counts while costing a fixed
// ~2KiB regardless of how many owners ever write.
const writeLockShards = 256

// compensatingWrite is the UNCONDITIONAL write option used by the internal
// compensation paths — the revision rollbacks and empty-payload
// neutralisations that undo a forward write whose live side-effect (a
// provider install, a discovery-origin apply) then failed, and the audit-emit
// reverts that follow them.
//
// A compensation is deliberately unconditional. It has no caller and no base
// to compare against: it is restoring a pointer the same locked
// read-modify-write just moved, so an expected-content precondition would be
// comparing against a state this code path itself produced. Applying the
// caller's token here would turn a successful undo into a refusal and leave
// the observable half-applied state the compensation exists to erase.
//
// The forward write of each door, by contrast, carries the caller's token.
//
// It is a FUNCTION rather than a package-level `var` so it is immutable by
// construction. A `var` holding a value every compensation path reads is
// package-level mutable state, which CLAUDE.md §5 forbids outside `init()`,
// driver registries and metric definitions — and the reason is exactly this
// shape: nothing mutates it today, and a single stray assignment would
// silently re-arm a precondition on every undo path at once.
func compensatingWrite() agentcfg.SetOptions { return agentcfg.SetOptions{} }

// lockAgent acquires the agent-tier write lock for (tenant, agent) and
// returns the release func (call via defer). It serialises every admin write
// verb's read-modify-write against concurrent writes to the same agent.
func (s *Service) lockAgent(tenant, agentID string) func() {
	return s.lockOwner(agentcfg.ConfigScopeAgent, tenant, "", agentID)
}

// lockOwner acquires the scope-aware per-owner write lock and returns the
// release func. The key includes the scope so the two tiers are never treated
// as the same owner: a ConfigScopeAgent write keys by (scope, tenant, agent) —
// the user slot is intentionally excluded so all of an agent's admin writers
// serialise — while a ConfigScopeUser write keys by (scope, tenant, real-user,
// agent), so distinct users (and the two tiers) own distinct keys. The key is
// striped onto a shard mutex; the same owner always resolves to the same shard,
// so same-owner writes still serialise. Distinct owners (including an agent and
// a user write) may hash to the same shard and serialise occasionally — benign
// over-locking, never a correctness loss; see the writeLocks field doc.
func (s *Service) lockOwner(scope agentcfg.ConfigScope, tenant, user, agentID string) func() {
	var key string
	if scope == agentcfg.ConfigScopeUser {
		key = "u\x00" + tenant + "\x00" + user + "\x00" + agentID
	} else {
		key = "a\x00" + tenant + "\x00" + agentID
	}
	m := &s.writeLocks[writeLockShard(key)]
	m.Lock()
	return m.Unlock
}

// writeLockShard maps an owner key to a stable shard index in
// [0, writeLockShards). It is FNV-1a over the key bytes — deterministic and
// allocation-free — so the same owner key always resolves to the same shard,
// which is what preserves per-owner serialisation.
func writeLockShard(key string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := range len(key) {
		h ^= uint32(key[i])
		h *= prime
	}
	return h % writeLockShards
}

// Option configures NewService.
type Option func(*Service)

// WithLogger sets the slog.Logger. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock injects the time source. Defaults to time.Now.
func WithClock(c Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.now = c
		}
	}
}

// WithSkillStore wires the SkillStore so the `agent_config.skills.*`
// methods are live. A nil store leaves the skills methods returning
// ErrSkillsUnavailable (→ 501 at the wire edge).
func WithSkillStore(st skills.SkillStore) Option {
	return func(s *Service) {
		if st != nil {
			s.skills = st
		}
	}
}

// WithSessionPersonalSkillController wires the sole authority for
// `agent_config.session.skills.*` and the dynamic personal-name projection on
// every session-overlay response. A nil controller leaves those reads/writes
// failing loud with ErrSkillsUnavailable (→ 501 at the wire edge); the Service
// never falls back to SkillStore or persists Overlay.PersonalSkills.
func WithSessionPersonalSkillController(controller SessionPersonalSkillController) Option {
	return func(s *Service) {
		if controller != nil {
			s.sessionPersonalSkills = controller
		}
	}
}

// WithBus wires the EventBus the tool-exposure consumer publishes the
// `mcp.connection.paused` / `.resumed` overlay events through. A nil bus
// leaves those events unpublished (the revision is still recorded — the
// generic `agent.config.revised` still fires from the registry).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithConnectionAttacher is a compatibility option for older deterministic
// embedders. Harbor production wiring uses [WithConnectionPreparer]; when the
// supplied concrete also implements [ConnectionPreparer], that stronger seam
// is selected automatically.
func WithConnectionAttacher(a ConnectionAttacher) Option {
	return func(s *Service) {
		if a != nil {
			if p, ok := a.(ConnectionPreparer); ok {
				s.preparer = p
			} else {
				s.preparer = legacyConnectionPreparer{attacher: a}
			}
		}
	}
}

// WithConnectionPreparer wires the unpublished prepare/persist/activate MCP
// transaction seam. Harbor's production assembler uses this option.
func WithConnectionPreparer(p ConnectionPreparer) Option {
	return func(s *Service) {
		if p != nil {
			s.preparer = p
		}
	}
}

// WithConnectionDetacher wires the concrete that tears a just-attached MCP
// server back down when `agent_config.add_mcp_connection`'s revision write
// fails after the attach succeeded. A nil detacher leaves that compensation
// logging the residual live server loudly instead of silently leaking it. The
// concrete (which imports the MCP driver) is the same object wired as the
// attacher; this package depends only on the interface.
func WithConnectionDetacher(d ConnectionDetacher) Option {
	return func(s *Service) {
		if d != nil {
			s.detacher = d
		}
	}
}

// WithDiscoveryOriginApplier wires the concrete that applies a connection's
// OAuth-discovery cross-origin allow-list to the LIVE MCP registry for
// `agent_config.set_mcp_discovery_origins` (and the run-start
// allowance-reconcile). A nil applier leaves the write recording the revision
// but reporting applied_live=false. The concrete (which imports the MCP driver)
// is injected at the cmd/harbor + devstack boundary; this package depends only
// on the interface.
func WithDiscoveryOriginApplier(a DiscoveryOriginApplier) Option {
	return func(s *Service) {
		if a != nil {
			s.discoveryApplier = a
		}
	}
}

// WithProviderInstaller wires the concrete that installs / uninstalls a
// Protocol-installed, zero-URL OAuth provider live into the owner-tagged
// provider set for `agent_config.set_oauth_provider` / `remove_oauth_provider`
// (and the run-start provider reconcile). A nil installer leaves the two verbs
// returning ErrProviderInstallUnavailable (→ 501). The concrete (which imports
// the auth package + resolves the boot credential broker) is injected at the
// cmd/harbor + devstack boundary; this package depends only on the interface.
func WithProviderInstaller(a ProviderInstaller) Option {
	return func(s *Service) {
		if a != nil {
			s.providerInstaller = a
			if p, ok := a.(ProviderPreparer); ok {
				s.providerPreparer = p
			}
		}
	}
}

// WithLLMProviderInstaller wires the concrete that installs / uninstalls a
// Protocol-installed, zero-URL broker-pull inference provider binding live into
// the owner-tagged provider set for `agent_config.set_llm_provider`. A nil
// installer leaves the verb returning ErrLLMProviderInstallUnavailable (→ 501).
// The concrete (which resolves the boot inference broker + builds the
// InferenceKeySource) is injected at the cmd/harbor + devstack boundary; this
// package depends only on the interface.
func WithLLMProviderInstaller(a LLMProviderInstaller) Option {
	return func(s *Service) {
		if a != nil {
			s.llmProviderInstaller = a
		}
	}
}

// WithInferenceBrokers sets the set of boot-declared inference-broker names
// (`llm.inference_brokers[].name`). set_llm_provider rejects a descriptor whose
// inference_broker is not in this set with ErrLLMProviderBrokerUnknown (400) —
// no admin-writable field determines a credential sink (the credential-plane invariant). An empty / nil
// list leaves EVERY broker name unknown (the fail-closed default). Injected at
// the cmd/harbor + devstack boundary from the loaded config.
func WithInferenceBrokers(names []string) Option {
	return func(s *Service) {
		if len(names) == 0 {
			return
		}
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			if n != "" {
				set[n] = struct{}{}
			}
		}
		s.inferenceBrokers = set
	}
}

// WithBootDeclaredOAuthProviders sets the set of OAuth provider names declared
// in the boot yaml. set_oauth_provider / remove_oauth_provider reject a name in
// this set with ErrBootDeclaredProvider (boot wins; edit yaml + restart). An
// empty / nil list leaves every unknown name a plain not-found (for remove) or
// installable (for set). Injected at the cmd/harbor + devstack boundary.
func WithBootDeclaredOAuthProviders(names []string) Option {
	return func(s *Service) {
		if len(names) == 0 {
			return
		}
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			if n != "" {
				set[n] = struct{}{}
			}
		}
		s.bootDeclaredOAuthProviders = set
	}
}

// WithAllowWireOAuthDescriptor sets the effective DEV-ONLY, fail-closed opt-in
// that permits set_oauth_provider / add_mcp_connection to carry a FULL OAuth
// provider binding over the wire (token_url / audience / scopes / remote{}). The
// caller passes (tools.allow_wire_oauth_descriptor config flag) OR (the
// HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR boot env). Default (option not applied) is
// false / fail-closed — a wire descriptor carrying any credential-sink field is
// rejected, the zero-URL name-only posture unchanged. Injected at the cmd/harbor
// + devstack boundary; never Protocol-writable.
func WithAllowWireOAuthDescriptor(allow bool) Option {
	return func(s *Service) {
		s.allowWireOAuthDescriptor = allow
	}
}

// WithAllowWireInjection sets the effective DEV-ONLY, fail-closed opt-in that
// permits add_mcp_connection to carry a per-user credential-INJECTION mapping
// (the `injection` object) for a receiver-style MCP server over the wire. The
// caller passes (tools.allow_wire_injection config flag) OR (the
// HARBOR_ALLOW_WIRE_INJECTION boot env). It is INDEPENDENT of
// WithAllowWireOAuthDescriptor. Default (option not applied) is false /
// fail-closed — a connection carrying any injection field is rejected. Injected
// at the cmd/harbor + devstack boundary; never Protocol-writable.
func WithAllowWireInjection(allow bool) Option {
	return func(s *Service) {
		s.allowWireInjection = allow
	}
}

// WithCoordinator wires the unified pause/resume primitive an auth-required
// MCP attach parks on. A nil coordinator leaves an auth-required attach
// failing loud with ErrCoordinatorUnavailable (never a silent drop).
func WithCoordinator(c pauseresume.Coordinator) Option {
	return func(s *Service) {
		if c != nil {
			s.coordinator = c
		}
	}
}

// WithStdioAllowlist sets the fail-closed allowlist of permitted stdio
// commands (matched on argv[0]) for `agent_config.add_mcp_connection`. A
// stdio add whose command[0] is absent is rejected with ErrStdioNotAllowed.
// An empty / nil allowlist rejects EVERY stdio add (the secure default);
// http adds are unaffected. Adding a stdio server runs an operator-supplied
// command (an RCE surface) — this gate is the §7 fail-closed boundary.
func WithStdioAllowlist(commands []string) Option {
	return func(s *Service) {
		if len(commands) == 0 {
			return
		}
		set := make(map[string]struct{}, len(commands))
		for _, c := range commands {
			if c != "" {
				set[c] = struct{}{}
			}
		}
		s.stdioAllowlist = set
	}
}

// WithBootDeclaredMCPServers records the set of MCP server names declared in
// the boot yaml (`tools.mcp_servers[].name`). `remove_mcp_connection` rejects
// a name in this set with ErrBootDeclaredConnection (distinct from the
// unknown-name error) — a boot-declared server is not revisioned state and is
// edited in yaml + restart. An empty / nil list leaves every unknown name a
// plain not-found. Injected at the cmd/harbor + devstack boundary from the
// loaded config so the verb (and the run-start reconcile, which never detaches
// a boot server) share one authoritative set.
func WithBootDeclaredMCPServers(names []string) Option {
	return func(s *Service) {
		if len(names) == 0 {
			return
		}
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			if n != "" {
				set[n] = struct{}{}
			}
		}
		s.bootDeclaredMCPServers = set
	}
}

// WithSessionOverlay wires the session-scoped safe-subset overlay store so
// the NON-admin `agent_config.session.*` verbs are live. A nil store leaves
// those methods returning ErrSessionOverlayUnavailable (→ 501 at the wire
// edge). The store is keyed by the caller's real (tenant, user, session)
// triple, so a session's overlay is invisible to another session.
func WithSessionOverlay(st sessionoverlay.Store) Option {
	return func(s *Service) {
		if st != nil {
			s.sessionOverlay = st
		}
	}
}

// WithValidModels sets the set of model names with a configured
// ModelProfile. `set_llm_params` (and a `set_revision` carrying an
// LLMParams.Model) reject a model outside this set with ErrUnknownModel —
// parity with the tenant model-swap, validated at SET time so an invalid
// model can never be persisted (never a silent run-start fallback).
//
// An empty / nil set means model-pinning is UNAVAILABLE: every non-empty
// model set fails loud (the binary always wires at least the bound default
// model, so an empty set signals a misconfiguration, not "accept anything").
// This matches the governance tenant-override policy's fail-loud-on-empty
// stance; it deliberately DIFFERS from runs.set_overrides (the one-shot
// session swap), which accepts any model when no set is configured.
func WithValidModels(models []string) Option {
	return func(s *Service) {
		if len(models) == 0 {
			return
		}
		set := make(map[string]struct{}, len(models))
		for _, m := range models {
			if m != "" {
				set[m] = struct{}{}
			}
		}
		s.validModels = set
	}
}

// validateModel rejects a set, non-empty model that has no configured
// ModelProfile. A nil model (the dimension is unset) is always allowed —
// only a model the run would actually request is validated. With no
// validModels configured, any model set fails loud (model-pinning is
// unavailable on this service), so a misconfiguration cannot silently
// accept an arbitrary model.
func (s *Service) validateModel(model *string) error {
	if model == nil || *model == "" {
		return nil
	}
	if _, ok := s.validModels[*model]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownModel, *model)
	}
	return nil
}

// validateLLMParams validates a per-agent LLM-params section at set time: the
// model (against the configured ModelProfiles) plus the sampling ranges
// (temperature ∈ [0,2], max-tokens > 0, reasoning-effort in the canonical
// taxonomy) — parity with runs.set_overrides so an out-of-range value never
// reaches a run (fail loud, never silently passed through to the provider
// edge). A nil section (no LLM-params edit) is a no-op. Each field is checked
// only when set.
func (s *Service) validateLLMParams(lp *prototypes.AgentConfigLLMParams) error {
	if lp == nil {
		return nil
	}
	if err := s.validateModel(lp.Model); err != nil {
		return err
	}
	if lp.Temperature != nil && (*lp.Temperature < 0 || *lp.Temperature > 2) {
		return fmt.Errorf("%w: temperature %v outside [0,2]", ErrInvalidLLMParams, *lp.Temperature)
	}
	if lp.MaxTokens != nil && *lp.MaxTokens <= 0 {
		return fmt.Errorf("%w: max_tokens %d must be positive", ErrInvalidLLMParams, *lp.MaxTokens)
	}
	if lp.ReasoningEffort != nil && *lp.ReasoningEffort != "" {
		if _, ok := validLLMReasoningEffort[*lp.ReasoningEffort]; !ok {
			return fmt.Errorf("%w: unknown reasoning_effort %q (want off/low/medium/high)",
				ErrInvalidLLMParams, *lp.ReasoningEffort)
		}
	}
	return nil
}

// validateHooks validates a run-lifecycle-hook section at set time: a
// negative run-completion timeout_ms is rejected loud (parity with the yaml
// validator's `runtime.hooks.run_completion.timeout` negative rejection). A
// nil section (no hooks edit) is a no-op; a zero timeout inherits the runtime
// default at run start. A present section with an empty run-completion tool is
// VALID — it is preserved as the explicit per-agent no-hook that overrides a
// yaml fleet hook at run start, not normalised away.
func (s *Service) validateHooks(h *prototypes.AgentConfigHooks) error {
	if h == nil || h.RunCompletion == nil {
		return nil
	}
	if h.RunCompletion.TimeoutMS < 0 {
		return fmt.Errorf("%w: run_completion.timeout_ms %d must not be negative",
			ErrInvalidHooks, h.RunCompletion.TimeoutMS)
	}
	return nil
}

// validateNaming validates a session auto-naming policy section at set time:
// the numeric bounds (no negative after_turns / repeat_every / max_repetitions,
// a set max_title_len in [8,200]), the no-unlimited invariant (repeat_every > 0
// REQUIRES max_repetitions ≥ 1), and the model (against the configured
// ModelProfiles, via the same validateModel path set_llm_params uses). A nil
// section (no naming edit) is a no-op. Each numeric field is bounds-checked
// only against its rule; a zero after_turns / max_title_len is valid (inherits
// the runtime default at run start).
func (s *Service) validateNaming(n *prototypes.AgentConfigNaming) error {
	if n == nil {
		return nil
	}
	if n.AfterTurns < 0 {
		return fmt.Errorf("%w: after_turns %d must not be negative", ErrInvalidNaming, n.AfterTurns)
	}
	if n.RepeatEvery < 0 {
		return fmt.Errorf("%w: repeat_every %d must not be negative", ErrInvalidNaming, n.RepeatEvery)
	}
	if n.MaxRepetitions < 0 {
		return fmt.Errorf("%w: max_repetitions %d must not be negative", ErrInvalidNaming, n.MaxRepetitions)
	}
	if n.RepeatEvery > 0 && n.MaxRepetitions < 1 {
		return fmt.Errorf("%w: repeat_every > 0 requires max_repetitions >= 1 (no unlimited value exists)", ErrInvalidNaming)
	}
	if n.MaxTitleLen != 0 && (n.MaxTitleLen < namingMinTitleLen || n.MaxTitleLen > namingMaxTitleLen) {
		return fmt.Errorf("%w: max_title_len %d outside [%d,%d]", ErrInvalidNaming, n.MaxTitleLen, namingMinTitleLen, namingMaxTitleLen)
	}
	if n.Model != "" {
		m := n.Model
		if err := s.validateModel(&m); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidNaming, err)
		}
	}
	return nil
}

// NewService builds the agent-config Service over a Registry. registry is
// mandatory — a nil fails loud with ErrMisconfigured rather than building
// a Service that would nil-panic on the first request (CLAUDE.md §5).
//
// The returned *Service is immutable after construction and safe for
// concurrent use by N goroutines.
func NewService(registry agentcfg.Registry, opts ...Option) (*Service, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: agentcfg.Registry is nil", ErrMisconfigured)
	}
	s := &Service{registry: registry, logger: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.coordinator != nil {
		registrar, ok := s.coordinator.(pauseresume.ContinuationRegistrar)
		if !ok {
			return nil, fmt.Errorf("%w: coordinator does not support durable continuations", ErrMisconfigured)
		}
		if err := registrar.RegisterContinuation(mcpAddContinuationKind, s.resumeMCPConnection); err != nil {
			return nil, fmt.Errorf("%w: register mcp continuation: %w", ErrMisconfigured, err)
		}
	}
	return s, nil
}

// Get reads the agent's active config revision.
func (s *Service) Get(ctx context.Context, req prototypes.AgentConfigGetRequest) (prototypes.AgentConfigGetResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	rev, set, err := s.registry.Active(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	resp := prototypes.AgentConfigGetResponse{Set: set, ProtocolVersion: prototypes.ProtocolVersion}
	if set {
		v := revisionToWire(rev)
		resp.Revision = &v
	}
	return resp, nil
}

// SetRevision writes a new immutable revision and advances the active
// pointer.
func (s *Service) SetRevision(ctx context.Context, req prototypes.AgentConfigSetRevisionRequest) (prototypes.AgentConfigSetRevisionResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	defer s.lockAgent(id.TenantID, req.AgentID)()
	// A full-payload set that pins per-agent LLM params is validated at set
	// time (parity with set_llm_params / the tenant model-swap) so an invalid
	// model or out-of-range sampling value can never be persisted.
	if err := s.validateLLMParams(req.Payload.LLMParams); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// A full-payload set that pins a hooks section is validated at set time —
	// a negative run-completion timeout is rejected loud before any registry
	// write, matching the yaml validator's posture.
	if err := s.validateHooks(req.Payload.Hooks); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// A full-payload set that pins loading-mode overrides is validated at
	// set time (parity with set_tool_exposure) so an unknown value can never
	// be persisted.
	if err := validateToolExposureLoading(req.Payload.ToolExposure); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// A full-payload set that pins a naming section is validated at set time —
	// the numeric bounds, the no-unlimited invariant, and the model — rejected
	// loud before any registry write.
	if err := s.validateNaming(req.Payload.Naming); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// A full-payload set that pins additive prompt blocks runs the SAME
	// name-charset + uniqueness + non-empty-body validation the
	// set_extra_system_blocks door enforces, so a section this second door
	// persists is one the first door would have accepted (parity at both
	// doors). Rejected loud before any registry write.
	if req.Payload.ExtraSystemBlocks != nil {
		if err := validateExtraSystemBlocks(req.Payload.ExtraSystemBlocks.Blocks); err != nil {
			return prototypes.AgentConfigSetRevisionResponse{}, err
		}
	}
	// A full-payload set that pins MCP connection descriptors runs every
	// descriptor through the SAME shape validator the add_mcp_connection door
	// enforces, so a descriptor this second door persists is a descriptor the
	// first door would have accepted (parity at both doors). The whole set is
	// rejected on the first offender — nothing is persisted. The NORMALISED
	// descriptors are persisted below so both doors record the same bytes (and
	// therefore the same content hash) for the same logical input.
	normalizedConns, err := validateConnectionsSection(req.Payload.Connections)
	if err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// The fail-closed stdio command allowlist — the SAME §7 RCE gate the
	// add_mcp_connection door applies. Both doors write the same revision spine,
	// so a stdio command the add door refuses is refused here too rather than
	// persisted for a later attach leg to read.
	if err := s.gateStdioConnectionCommands(normalizedConns); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	// A full-payload set that pins a runtime-added MCP connection carrying a
	// per-user credential-INJECTION mapping runs through the SAME fail-closed
	// opt-in gate + shape validation the add_mcp_connection door enforces — so a
	// gated (opt-in off) or malformed injection can never be persisted through
	// this second door (the fail-closed invariant holds at both doors).
	if err := s.gateAndValidateConnectionsInjection(req.Payload.Connections); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	payload := payloadToDomain(req.Payload)
	// Persist the validator's NORMALISED descriptors rather than the raw wire
	// values connectionsToDomain projects. The nil-vs-present distinction on the
	// section itself is preserved — only the server list is replaced, so a
	// payload that pins no connections still carries none.
	if payload.Connections != nil {
		payload.Connections.Servers = normalizedConns
	}
	rev, err := s.registry.SetRevision(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeAgent, payload,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
	if err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	return prototypes.AgentConfigSetRevisionResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// ListRevisions returns the agent's revision chain, newest-first.
func (s *Service) ListRevisions(ctx context.Context, req prototypes.AgentConfigListRevisionsRequest) (prototypes.AgentConfigListRevisionsResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	revs, err := s.registry.ListRevisions(ctx, identity.Quadruple{Identity: id}, req.AgentID, agentcfg.ConfigScopeAgent, req.Limit)
	if err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	out := make([]prototypes.AgentConfigRevisionView, 0, len(revs))
	for _, r := range revs {
		out = append(out, revisionToWire(r))
	}
	return prototypes.AgentConfigListRevisionsResponse{
		Revisions:       out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// Diff returns the server-side compare of two existing revisions.
func (s *Service) Diff(ctx context.Context, req prototypes.AgentConfigDiffRequest) (prototypes.AgentConfigDiffResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	d, err := s.registry.Diff(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.FromRevision, req.ToRevision, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	return prototypes.AgentConfigDiffResponse{
		Diff:            diffToWire(d),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// Rollback repoints the active pointer to an existing revision.
func (s *Service) Rollback(ctx context.Context, req prototypes.AgentConfigRollbackRequest) (prototypes.AgentConfigRollbackResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	defer s.lockAgent(id.TenantID, req.AgentID)()
	rev, err := s.registry.Rollback(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.RevisionID, agentcfg.ConfigScopeAgent,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
	if err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	return prototypes.AgentConfigRollbackResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// identityFromScope validates a wire IdentityScope + agent id into an
// identity.Identity, failing closed on an incomplete triple or empty
// agent id.
func identityFromScope(scope prototypes.IdentityScope, agentID string) (identity.Identity, error) {
	id := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return identity.Identity{}, fmt.Errorf("%w: (tenant=%q user=%q session=%q)",
			ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	if agentID == "" {
		return identity.Identity{}, fmt.Errorf("%w: agent_id is empty", ErrIdentityRequired)
	}
	return id, nil
}

// revisionToWire projects a domain Revision onto the wire view. The
// author's session is intentionally dropped (the audit anchor is the
// (tenant, user) actor).
func revisionToWire(r agentcfg.Revision) prototypes.AgentConfigRevisionView {
	return prototypes.AgentConfigRevisionView{
		RevisionID:       r.RevisionID,
		ParentRevisionID: r.ParentRevisionID,
		ContentHash:      r.ContentHash,
		AuthorTenant:     r.Author.TenantID,
		AuthorUser:       r.Author.UserID,
		CreatedAt:        r.CreatedAt,
		Payload:          payloadToWire(r.Payload),
	}
}

// payloadToWire projects a domain ConfigPayload onto the wire envelope.
func payloadToWire(p agentcfg.ConfigPayload) prototypes.AgentConfigPayload {
	var out prototypes.AgentConfigPayload
	if p.PromptLayers != nil {
		out.PromptLayers = &prototypes.AgentConfigPromptLayers{
			Base: copyStringPtr(p.PromptLayers.Base),
			User: copyStringPtr(p.PromptLayers.User),
		}
	}
	if p.Skills != nil {
		out.Skills = &prototypes.AgentConfigSkillsSelection{Names: append([]string(nil), p.Skills.Names...)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &prototypes.AgentConfigToolExposure{
			PausedServers:      append([]string(nil), p.ToolExposure.PausedServers...),
			DisabledTools:      append([]string(nil), p.ToolExposure.DisabledTools...),
			ServerLoadingModes: cloneAnnotations(p.ToolExposure.ServerLoadingModes),
			ToolLoadingModes:   cloneAnnotations(p.ToolExposure.ToolLoadingModes),
		}
	}
	if p.Connections != nil {
		out.Connections = &prototypes.AgentConfigConnections{
			Servers: connectionsToWire(p.Connections.Servers),
		}
	}
	if p.OAuthProviders != nil {
		out.OAuthProviders = &prototypes.AgentConfigOAuthProviders{
			Providers: oauthProvidersToWire(p.OAuthProviders.Providers),
		}
	}
	if p.LLMParams != nil {
		out.LLMParams = &prototypes.AgentConfigLLMParams{
			Model:           copyStringPtr(p.LLMParams.Model),
			Temperature:     copyFloat64Ptr(p.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(p.LLMParams.MaxTokens),
			ReasoningEffort: copyStringPtr(p.LLMParams.ReasoningEffort),
		}
	}
	if p.Hooks != nil {
		wh := &prototypes.AgentConfigHooks{}
		if p.Hooks.RunCompletion != nil {
			wh.RunCompletion = &prototypes.AgentConfigRunCompletionHook{
				Tool:      p.Hooks.RunCompletion.Tool,
				TimeoutMS: p.Hooks.RunCompletion.TimeoutMS,
			}
		}
		out.Hooks = wh
	}
	if p.ExtraSystemBlocks != nil {
		// ORDER-PRESERVING: the projection copies the slice positionally.
		// A map here — or a sort — would silently re-order the operator's
		// composed prompt on the way to the reader.
		wb := make([]prototypes.AgentConfigNamedBlock, 0, len(p.ExtraSystemBlocks.Blocks))
		for _, b := range p.ExtraSystemBlocks.Blocks {
			wb = append(wb, prototypes.AgentConfigNamedBlock{Name: b.Name, Body: b.Body})
		}
		out.ExtraSystemBlocks = &prototypes.AgentConfigExtraSystemBlocks{Blocks: wb}
	}
	if p.Naming != nil {
		out.Naming = &prototypes.AgentConfigNaming{
			Auto:           p.Naming.Auto,
			AfterTurns:     p.Naming.AfterTurns,
			RepeatEvery:    p.Naming.RepeatEvery,
			MaxRepetitions: p.Naming.MaxRepetitions,
			MaxTitleLen:    p.Naming.MaxTitleLen,
			Model:          p.Naming.Model,
		}
	}
	return out
}

// loadingChangesToWire projects domain LoadingModeChange entries onto the
// wire shape (a defensive copy; no shared backing slice).
func loadingChangesToWire(in []agentcfg.LoadingModeChange) []prototypes.AgentConfigLoadingModeChange {
	if len(in) == 0 {
		return nil
	}
	out := make([]prototypes.AgentConfigLoadingModeChange, 0, len(in))
	for _, c := range in {
		out = append(out, prototypes.AgentConfigLoadingModeChange{Key: c.Key, From: c.From, To: c.To})
	}
	return out
}

// connectionsToWire projects domain connection descriptors onto the wire
// shape (defensive copies; no shared backing slices).
func connectionsToWire(in []agentcfg.MCPConnectionDescriptor) []prototypes.AgentConfigMCPConnectionDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]prototypes.AgentConfigMCPConnectionDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, prototypes.AgentConfigMCPConnectionDescriptor{
			Name:            d.Name,
			Transport:       string(d.Transport),
			Command:         append([]string(nil), d.Command...),
			URL:             d.URL,
			OAuthProvider:   d.OAuthProvider,
			MetaAnnotations: cloneAnnotations(d.MetaAnnotations),
			// The OAuth-discovery cross-origin allow-list is part of the versioned
			// desired state — surface it on the revision view so get / list / diff
			// show the allowance the connection actually carries, matching what the
			// allowance write recorded. NON-SECRET (public https origins).
			OAuthDiscoveryAllowedOrigins: append([]string(nil), d.OAuthDiscoveryAllowedOrigins...),
			// The per-user credential-injection mapping is part of the versioned
			// desired state — surface it on the revision view (diff / rollback /
			// list parity). NON-SECRET (a mapping, never a value).
			Injection: injectionDescriptorToWire(d.Injection),
			// The egress-substitution declaration is likewise versioned
			// desired state — surface it so get / list / diff / rollback show
			// the byte-eligibility and the mapping the connection actually
			// carries. NON-SECRET (a boolean plus parameter names).
			ArtifactByteEligible: d.ArtifactByteEligible,
			ArtifactParams:       d.CloneArtifactParams(),
		})
	}
	return out
}

// connectionsToDomain projects wire connection descriptors onto the domain
// shape (defensive copies; no shared backing slices).
func connectionsToDomain(in []prototypes.AgentConfigMCPConnectionDescriptor) []agentcfg.MCPConnectionDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentcfg.MCPConnectionDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, agentcfg.MCPConnectionDescriptor{
			Name:            d.Name,
			Transport:       agentcfg.MCPTransport(d.Transport),
			Command:         append([]string(nil), d.Command...),
			URL:             d.URL,
			OAuthProvider:   d.OAuthProvider,
			MetaAnnotations: cloneAnnotations(d.MetaAnnotations),
			// The OAuth-discovery cross-origin allow-list is part of the persisted
			// descriptor (the same field the add door and the allowance write
			// record), so a full-payload write carries it onto the revision and it
			// round-trips through get / list / diff and the run-start
			// allowance-reconcile — one descriptor shape across every door.
			OAuthDiscoveryAllowedOrigins: append([]string(nil), d.OAuthDiscoveryAllowedOrigins...),
			Injection:                    injectionDescriptorToDomain(d.Injection),
			// The egress-substitution declaration is part of the persisted
			// descriptor, so it round-trips through get / list / diff and the
			// run-start reconcile — one descriptor shape across every door.
			ArtifactByteEligible: d.ArtifactByteEligible,
			ArtifactParams:       cloneArtifactParams(d.ArtifactParams),
		})
	}
	return out
}

// cloneArtifactParams returns a defensive deep copy of an
// egress-substitution artifact-parameter mapping (nil for nil / empty),
// so a wire payload and a persisted revision never share a backing map
// or slice.
func cloneArtifactParams(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for tool, params := range in {
		out[tool] = append([]string(nil), params...)
	}
	return out
}

// injectionDescriptorToDomain projects a wire injection descriptor onto the
// domain shape (nil for nil). NON-SECRET throughout — the mapping only.
func injectionDescriptorToDomain(d *prototypes.AgentConfigMCPCredentialInjectionDescriptor) *agentcfg.MCPCredentialInjectionDescriptor {
	if d == nil {
		return nil
	}
	return &agentcfg.MCPCredentialInjectionDescriptor{
		Provider:      d.Provider,
		Form:          d.Form,
		Header:        d.Header,
		BasicUsername: d.BasicUsername,
		MetaKey:       d.MetaKey,
	}
}

// oauthProvidersToWire projects domain installed-provider descriptors onto the
// zero-URL wire shape (defensive copies; no shared backing slices).
func oauthProvidersToWire(in []agentcfg.OAuthProviderDescriptor) []prototypes.AgentConfigOAuthProviderDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]prototypes.AgentConfigOAuthProviderDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, prototypes.AgentConfigOAuthProviderDescriptor{
			Name:             d.Name,
			Driver:           d.Driver,
			CredentialSource: d.CredentialSource,
			CredentialBroker: d.CredentialBroker,
			Scopes:           append([]string(nil), d.Scopes...),
		})
	}
	return out
}

// oauthProvidersToDomain projects zero-URL wire descriptors onto the domain
// shape (defensive copies; no shared backing slices).
func oauthProvidersToDomain(in []prototypes.AgentConfigOAuthProviderDescriptor) []agentcfg.OAuthProviderDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentcfg.OAuthProviderDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, agentcfg.OAuthProviderDescriptor{
			Name:             d.Name,
			Driver:           d.Driver,
			CredentialSource: d.CredentialSource,
			CredentialBroker: d.CredentialBroker,
			Scopes:           append([]string(nil), d.Scopes...),
		})
	}
	return out
}

// payloadToDomain projects a wire envelope onto the domain ConfigPayload.
func payloadToDomain(p prototypes.AgentConfigPayload) agentcfg.ConfigPayload {
	var out agentcfg.ConfigPayload
	if p.PromptLayers != nil {
		out.PromptLayers = &agentcfg.PromptLayers{
			Base: copyStringPtr(p.PromptLayers.Base),
			User: copyStringPtr(p.PromptLayers.User),
		}
	}
	if p.Skills != nil {
		out.Skills = &agentcfg.SkillsSelection{Names: append([]string(nil), p.Skills.Names...)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &agentcfg.ToolExposure{
			PausedServers:      append([]string(nil), p.ToolExposure.PausedServers...),
			DisabledTools:      append([]string(nil), p.ToolExposure.DisabledTools...),
			ServerLoadingModes: cloneAnnotations(p.ToolExposure.ServerLoadingModes),
			ToolLoadingModes:   cloneAnnotations(p.ToolExposure.ToolLoadingModes),
		}
	}
	if p.Connections != nil {
		out.Connections = &agentcfg.ConnectionsSection{
			Servers: connectionsToDomain(p.Connections.Servers),
		}
	}
	if p.OAuthProviders != nil {
		out.OAuthProviders = &agentcfg.OAuthProvidersSection{
			Providers: oauthProvidersToDomain(p.OAuthProviders.Providers),
		}
	}
	if p.LLMParams != nil {
		out.LLMParams = &agentcfg.LLMParams{
			Model:           copyStringPtr(p.LLMParams.Model),
			Temperature:     copyFloat64Ptr(p.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(p.LLMParams.MaxTokens),
			ReasoningEffort: copyStringPtr(p.LLMParams.ReasoningEffort),
		}
	}
	if p.Hooks != nil {
		dh := &agentcfg.HooksSection{}
		if p.Hooks.RunCompletion != nil {
			dh.RunCompletion = &agentcfg.RunCompletionHook{
				Tool:      p.Hooks.RunCompletion.Tool,
				TimeoutMS: p.Hooks.RunCompletion.TimeoutMS,
			}
		}
		out.Hooks = dh
	}
	if p.ExtraSystemBlocks != nil {
		// ORDER-PRESERVING, same reason as the wire projection above.
		out.ExtraSystemBlocks = &agentcfg.ExtraSystemBlocks{
			Blocks: blocksToDomain(p.ExtraSystemBlocks.Blocks),
		}
	}
	if p.Naming != nil {
		out.Naming = &agentcfg.NamingSection{
			Auto:           p.Naming.Auto,
			AfterTurns:     p.Naming.AfterTurns,
			RepeatEvery:    p.Naming.RepeatEvery,
			MaxRepetitions: p.Naming.MaxRepetitions,
			MaxTitleLen:    p.Naming.MaxTitleLen,
			Model:          p.Naming.Model,
		}
	}
	return out
}

// diffToWire projects a domain Diff onto the wire diff.
func diffToWire(d agentcfg.Diff) prototypes.AgentConfigDiff {
	return prototypes.AgentConfigDiff{
		FromRevisionID: d.FromRevisionID,
		ToRevisionID:   d.ToRevisionID,
		Skills: prototypes.AgentConfigSkillsDiff{
			Added:   append([]string(nil), d.Skills.Added...),
			Removed: append([]string(nil), d.Skills.Removed...),
		},
		ToolExposure: prototypes.AgentConfigToolExposureDiff{
			PausedAdded:          append([]string(nil), d.ToolExposure.PausedAdded...),
			PausedResumed:        append([]string(nil), d.ToolExposure.PausedResumed...),
			DisabledAdded:        append([]string(nil), d.ToolExposure.DisabledAdded...),
			DisabledEnabled:      append([]string(nil), d.ToolExposure.DisabledEnabled...),
			ServerLoadingChanges: loadingChangesToWire(d.ToolExposure.ServerLoadingChanges),
			ToolLoadingChanges:   loadingChangesToWire(d.ToolExposure.ToolLoadingChanges),
		},
		PromptLayers: prototypes.AgentConfigPromptLayersDiff{
			BaseChanged: d.PromptLayers.BaseChanged,
			BaseFrom:    d.PromptLayers.BaseFrom,
			BaseTo:      d.PromptLayers.BaseTo,
			UserChanged: d.PromptLayers.UserChanged,
			UserFrom:    d.PromptLayers.UserFrom,
			UserTo:      d.PromptLayers.UserTo,
		},
		Connections: prototypes.AgentConfigConnectionsDiff{
			Added:   append([]string(nil), d.Connections.Added...),
			Removed: append([]string(nil), d.Connections.Removed...),
		},
		OAuthProviders: prototypes.AgentConfigOAuthProvidersDiff{
			Added:   append([]string(nil), d.OAuthProviders.Added...),
			Removed: append([]string(nil), d.OAuthProviders.Removed...),
		},
		LLMParams: prototypes.AgentConfigLLMParamsDiff{
			ModelChanged: d.LLMParams.ModelChanged,
			ModelFrom:    d.LLMParams.ModelFrom,
			ModelTo:      d.LLMParams.ModelTo,

			TemperatureChanged: d.LLMParams.TemperatureChanged,
			TemperatureFrom:    d.LLMParams.TemperatureFrom,
			TemperatureTo:      d.LLMParams.TemperatureTo,

			MaxTokensChanged: d.LLMParams.MaxTokensChanged,
			MaxTokensFrom:    d.LLMParams.MaxTokensFrom,
			MaxTokensTo:      d.LLMParams.MaxTokensTo,

			ReasoningEffortChanged: d.LLMParams.ReasoningEffortChanged,
			ReasoningEffortFrom:    d.LLMParams.ReasoningEffortFrom,
			ReasoningEffortTo:      d.LLMParams.ReasoningEffortTo,
		},
		Hooks: prototypes.AgentConfigHooksDiff{
			SectionPresentChanged: d.Hooks.SectionPresentChanged,
			SectionPresentFrom:    d.Hooks.SectionPresentFrom,
			SectionPresentTo:      d.Hooks.SectionPresentTo,

			RunCompletionToolChanged: d.Hooks.RunCompletionToolChanged,
			RunCompletionToolFrom:    d.Hooks.RunCompletionToolFrom,
			RunCompletionToolTo:      d.Hooks.RunCompletionToolTo,

			RunCompletionTimeoutChanged: d.Hooks.RunCompletionTimeoutChanged,
			RunCompletionTimeoutFrom:    d.Hooks.RunCompletionTimeoutFrom,
			RunCompletionTimeoutTo:      d.Hooks.RunCompletionTimeoutTo,
		},
		Naming: prototypes.AgentConfigNamingDiff{
			AutoChanged: d.Naming.AutoChanged,
			AutoFrom:    d.Naming.AutoFrom,
			AutoTo:      d.Naming.AutoTo,

			AfterTurnsChanged: d.Naming.AfterTurnsChanged,
			AfterTurnsFrom:    d.Naming.AfterTurnsFrom,
			AfterTurnsTo:      d.Naming.AfterTurnsTo,

			RepeatEveryChanged: d.Naming.RepeatEveryChanged,
			RepeatEveryFrom:    d.Naming.RepeatEveryFrom,
			RepeatEveryTo:      d.Naming.RepeatEveryTo,

			MaxRepetitionsChanged: d.Naming.MaxRepetitionsChanged,
			MaxRepetitionsFrom:    d.Naming.MaxRepetitionsFrom,
			MaxRepetitionsTo:      d.Naming.MaxRepetitionsTo,

			MaxTitleLenChanged: d.Naming.MaxTitleLenChanged,
			MaxTitleLenFrom:    d.Naming.MaxTitleLenFrom,
			MaxTitleLenTo:      d.Naming.MaxTitleLenTo,

			ModelChanged: d.Naming.ModelChanged,
			ModelFrom:    d.Naming.ModelFrom,
			ModelTo:      d.Naming.ModelTo,
		},
		ExtraSystemBlocks: prototypes.AgentConfigExtraSystemBlocksDiff{
			Added:   append([]string(nil), d.ExtraSystemBlocks.Added...),
			Removed: append([]string(nil), d.ExtraSystemBlocks.Removed...),
			Changed: append([]string(nil), d.ExtraSystemBlocks.Changed...),
			// Order is render order, so a pure re-ordering is a real change
			// and the reader must be able to see it.
			Reordered: d.ExtraSystemBlocks.Reordered,
		},
	}
}

// copyStringPtr returns a fresh copy of a *string (nil stays nil) so the
// wire↔domain projections never share a pointer with the caller's payload.
func copyStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// copyFloat64Ptr / copyIntPtr return a fresh copy of a numeric pointer (nil
// stays nil) so the wire↔domain projections never share a pointer with the
// caller's payload.
func copyFloat64Ptr(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

func copyIntPtr(i *int) *int {
	if i == nil {
		return nil
	}
	v := *i
	return &v
}
