// Package a2a is Harbor's southbound A2A integration with the tool
// catalog (Phase 29). It implements `tools.ToolProvider` by composing
// the wire `distributed.RemoteTransport` (`internal/distributed/drivers/a2a`)
// with the catalog's standard `RunWithPolicyHooked` shell.
//
// Discovery model. The Provider is parameterised by a single peer's
// base URL plus a wire `distributed.RemoteTransport` instance. At
// `Connect`, the Provider issues a `GetExtendedAgentCard` against the
// peer; `Discover` materialises each `AgentSkill` from the card into a
// `tools.ToolDescriptor` with `Transport: TransportA2A` and an Invoke
// closure that translates the catalog-edge JSON-RawMessage into an
// A2A `Message` with a `TextPart`, dispatches via the transport, and
// returns the resulting `a2a.Task` as the `ToolResult.Value`.
//
// One Provider per peer. Operators wanting to register N peers
// instantiate N Providers; the catalog presents the union as one set
// of `Tool`s. The `SourceID` is derived from the peer URL so
// observability + audit can attribute each tool to its origin.
//
// Reliability shell (D-024). Every Invoke routes through
// `tools.RunWithPolicyHooked` so timeout / retry / validation happen
// once, in the same place, regardless of transport. The wire driver
// does NOT add its own shell.
//
// Concurrent reuse (D-025). Provider is constructed once with an
// immutable peer URL + transport reference; Connect / Discover / Close
// take an internal Mutex so concurrent calls are serialised; Invoke
// is stateless on the Provider and dispatches through the shared
// transport.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/distributed"
	a2atypes "github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// DriverName documents the conceptual driver identifier. The tools
// subsystem has no driver-factory registry (catalogs are constructed
// in code), so this constant is for documentation + audit logging.
const DriverName = "a2a"

// Sentinels.
var (
	// ErrNotConnected — Discover or invocation called before Connect.
	ErrNotConnected = errors.New("tools/a2a: provider not connected")
	// ErrAgentSkillEmpty — discovered card declared zero skills.
	ErrAgentSkillEmpty = errors.New("tools/a2a: AgentCard declares no skills")
)

// Option configures a Provider at construction.
type Option func(*config)

// WithDescriptorOptions injects per-skill descriptor options applied
// to every materialised Tool. Use this to override the policy shell
// (e.g. tighter timeouts for chatty peers) or to add audit tags.
func WithDescriptorOptions(opts ...tools.DescriptorOption) Option {
	return func(c *config) { c.descOpts = append(c.descOpts, opts...) }
}

// WithToolNamePrefix prepends prefix + "." to every materialised
// tool's Name. The default is the peer URL's host (so two peers can
// expose a skill named "echo" without colliding).
func WithToolNamePrefix(prefix string) Option {
	return func(c *config) { c.namePrefix = prefix }
}

type config struct {
	namePrefix string
	descOpts   []tools.DescriptorOption
}

// Provider implements `tools.ToolProvider` for a single A2A peer.
type Provider struct {
	transport distributed.RemoteTransport
	card      *a2atypes.AgentCard
	peerURL   string
	cfg       config
	mu        sync.Mutex
	connected bool
}

// New constructs a Provider. `peerURL` is the peer's canonical base
// URL (matches a registered peer on the wire transport's Registry);
// `transport` is the wire `distributed.RemoteTransport`.
//
// Returns an error when peerURL is empty or transport is nil — the
// Provider is unusable in either case.
func New(peerURL string, transport distributed.RemoteTransport, opts ...Option) (*Provider, error) {
	if peerURL == "" {
		return nil, fmt.Errorf("tools/a2a.New: peerURL is empty")
	}
	if transport == nil {
		return nil, fmt.Errorf("tools/a2a.New: transport is nil")
	}
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}
	return &Provider{
		peerURL:   peerURL,
		transport: transport,
		cfg:       cfg,
	}, nil
}

// SourceID returns the stable per-peer identifier surfaced on every
// materialised Tool.Source.
func (p *Provider) SourceID() tools.ToolSourceID {
	return tools.ToolSourceID("a2a:" + p.peerURL)
}

// Connect fetches the AgentCard via the wire transport's
// `GetExtendedAgentCard`. Identity is read from ctx and propagated
// through the wire driver.
//
// Connect is idempotent: a second call refreshes the cached card so
// callers can use Connect as a "manual cache invalidation" hook.
func (p *Provider) Connect(ctx context.Context) error {
	if _, ok := identity.From(ctx); !ok {
		return fmt.Errorf("tools/a2a.Connect: identity required on ctx")
	}
	card, err := p.transport.GetExtendedAgentCard(ctx)
	if err != nil {
		return fmt.Errorf("tools/a2a.Connect: get card: %w", err)
	}
	if card == nil {
		return fmt.Errorf("tools/a2a.Connect: nil AgentCard")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.card = card
	p.connected = true
	return nil
}

// Discover returns one ToolDescriptor per `AgentSkill` on the cached
// AgentCard. Returns ErrNotConnected when Connect has not run;
// ErrAgentSkillEmpty when the card lists no skills.
func (p *Provider) Discover(_ context.Context) ([]tools.ToolDescriptor, error) {
	p.mu.Lock()
	card := p.card
	connected := p.connected
	p.mu.Unlock()
	if !connected || card == nil {
		return nil, ErrNotConnected
	}
	if len(card.Skills) == 0 {
		return nil, ErrAgentSkillEmpty
	}
	out := make([]tools.ToolDescriptor, 0, len(card.Skills))
	for i := range card.Skills {
		skill := card.Skills[i] // copy so the Invoke closure captures stable
		out = append(out, p.buildDescriptor(skill))
	}
	return out, nil
}

// Close is a no-op: the Provider does not own the transport (the
// caller does). Multiple Closes are safe.
func (p *Provider) Close(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connected = false
	p.card = nil
	return nil
}

// buildDescriptor materialises one AgentSkill into a Tool/Descriptor.
func (p *Provider) buildDescriptor(skill a2atypes.AgentSkill) tools.ToolDescriptor {
	cfg := tools.ResolveOptions(p.cfg.descOpts...)

	// The catalog edge validates against ArgsSchema; an A2A skill's
	// schema is not declared by the proto (skills describe input/
	// output via free-form `input_modes` / `output_modes` MIME
	// hints). For V1 we accept any JSON-shaped input via an
	// open-object schema; tightening this is post-V1 work (the
	// planner can supply a sub-schema via DescriptorOptions).
	argsSchema := json.RawMessage(`{"type":"object","additionalProperties":true}`)
	outSchema := json.RawMessage(`{"type":"object","additionalProperties":true}`)

	tool := tools.Tool{
		Name:        p.toolName(skill),
		Description: chooseString(skill.Description, skill.Name),
		ArgsSchema:  argsSchema,
		OutSchema:   outSchema,
		SideEffects: pickSideEffect(cfg),
		Tags:        joinTags(skill.Tags, cfg.Tags),
		AuthScopes:  cfg.AuthScopes,
		CostHint:    cfg.CostHint,
		LatencyHint: cfg.LatencyHint,
		SafetyNotes: cfg.SafetyNotes,
		Loading:     pickLoading(cfg),
		Examples:    cfg.Examples,
		Source:      p.SourceID(),
		Transport:   tools.TransportA2A,
		Policy:      cfg.Policy,
	}

	skillID := skill.ID
	validate := func(args json.RawMessage) error {
		// V1 accepts any JSON object; catalog-edge validation
		// against the open-object schema is permissive. The
		// peer is expected to return a typed error if it
		// rejects the payload.
		if len(args) == 0 {
			return nil
		}
		var probe map[string]any
		if err := json.Unmarshal(args, &probe); err != nil {
			return fmt.Errorf("%w: not a JSON object: %w", tools.ErrToolInvalidArgs, err)
		}
		return nil
	}
	descriptor := tools.ToolDescriptor{
		Tool:     tool,
		Validate: validate,
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.RunWithPolicy(
				ctx,
				args,
				func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
					return p.invoke(ctx, skillID, args)
				},
				validate,
				nil, // no output validation at V1
				tool.Policy,
			)
		},
	}
	return descriptor
}

// invoke is the inner-most call: build the A2A Message, dispatch via
// the wire transport, surface the resulting Task as ToolResult.
func (p *Provider) invoke(ctx context.Context, skillID string, args json.RawMessage) (tools.ToolResult, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("tools/a2a.invoke: identity required")
	}
	// Encode args as a DataPart so structured arguments round-trip
	// faithfully (TextPart would force a JSON-in-string envelope).
	var argVal any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argVal); err != nil {
			return tools.ToolResult{}, fmt.Errorf("%w: %w", tools.ErrToolInvalidArgs, err)
		}
	}
	msg := a2atypes.Message{
		MessageID: skillID + ":" + id.SessionID,
		Role:      a2atypes.RoleUser,
		Parts: a2atypes.Parts{
			&a2atypes.DataPart{
				Data: argVal,
			},
		},
		Metadata: map[string]any{
			"skill_id": skillID,
			"tenant":   id.TenantID,
			"user":     id.UserID,
		},
	}
	req := distributed.RemoteCallRequest{
		AgentURL: p.peerURL,
		Kind:     distributed.RemoteCallKindSend,
		Message:  msg,
	}
	res, err := p.transport.Send(ctx, req)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{
		Value: res.Task,
		Meta: map[string]any{
			"a2a_peer":   p.peerURL,
			"skill_id":   skillID,
			"task_state": res.Task.Status.State.String(),
		},
	}, nil
}

// toolName composes the catalog name for a skill. Default is the
// peer URL's host concatenated with the skill ID; an explicit
// WithToolNamePrefix overrides the host.
func (p *Provider) toolName(skill a2atypes.AgentSkill) string {
	prefix := p.cfg.namePrefix
	if prefix == "" {
		prefix = "a2a:" + p.peerURL
	}
	if skill.ID == "" {
		return prefix + "/" + skill.Name
	}
	return prefix + "/" + skill.ID
}

// chooseString returns first if non-empty else second.
func chooseString(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// pickSideEffect resolves a sensible default for an A2A skill — the
// remote agent's effects are by definition external to Harbor, so
// SideEffectExternal is the safe default when the operator did not
// set one explicitly.
func pickSideEffect(cfg tools.ResolvedDescriptorConfig) tools.SideEffect {
	if cfg.SideEffect == "" {
		return tools.SideEffectExternal
	}
	return cfg.SideEffect
}

// pickLoading resolves a default LoadingMode — A2A skills are loaded
// always so the planner sees them in the prompt-time catalog. An
// operator can override via WithDescriptorOptions(tools.WithLoading(...)).
func pickLoading(cfg tools.ResolvedDescriptorConfig) tools.LoadingMode {
	if cfg.Loading == "" {
		return tools.LoadingAlways
	}
	return cfg.Loading
}

// joinTags concatenates skill.Tags + cfg.Tags, deduplicating.
func joinTags(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range b {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// Compile-time guards.
var _ tools.ToolProvider = (*Provider)(nil)
