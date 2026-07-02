// Package agentcfg owns Harbor's agent-config control plane: a durable,
// identity-scoped, versioned desired-state registry where every edit to
// an agent's configuration (skills membership now; tool/MCP exposure and
// prompt layers as later consumers extend the envelope) is an immutable,
// content-addressed revision. The active configuration is a pointer to a
// revision; rollback is a repoint; diff is a server-side revision
// compare.
//
// # The seam (CLAUDE.md §4.4)
//
// The Registry interface lives here; drivers live under
// internal/agentcfg/drivers/<driver>/; a factory + registry dispatches by
// name. The StateStore-backed driver reuses the §9 persistence triad for
// identity isolation, exactly as the governance tenant-override policy
// does — no new persistence subsystem.
//
// # Identity (CLAUDE.md §6)
//
// Every record is scoped by the full (tenant, user, session) triple. The
// registry is KEYED by agent_id (the agent's registration identity), but
// agent_id is NEVER an isolation filter: the driver keys each agent's
// config under a synthetic identity whose tenant is the caller's verified
// tenant, so a tenant's config is invisible to another tenant.
//
// # Concurrent reuse (the concurrent-reuse contract)
//
// A constructed Registry is a compiled artifact: immutable after
// construction and safe to share across N concurrent goroutines. Per-run
// state lives in ctx and the supplied arguments, never on the Registry.
package agentcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityRequired — a method was called with an incomplete
	// identity triple or an empty agent id. Fails closed (CLAUDE.md §6).
	ErrIdentityRequired = errors.New("agentcfg: identity triple incomplete")
	// ErrInvalidConfig — a factory was called with a missing mandatory
	// dependency (a nil StateStore or EventBus).
	ErrInvalidConfig = errors.New("agentcfg: invalid configuration")
	// ErrUnknownDriver — Open was asked for a driver name no registered
	// factory handles.
	ErrUnknownDriver = errors.New("agentcfg: unknown driver")
	// ErrClosed — a method was called after Close.
	ErrClosed = errors.New("agentcfg: registry is closed")
	// ErrRevisionNotFound — Get / Diff / Rollback referenced a revision
	// id that has no record. Fails loud (CLAUDE.md §13 — no silent drop).
	ErrRevisionNotFound = errors.New("agentcfg: revision not found")
	// ErrStateUnavailable — a StateStore read/write failed.
	ErrStateUnavailable = errors.New("agentcfg: state store unavailable")
	// ErrInvalidPayload — the supplied ConfigPayload failed validation.
	ErrInvalidPayload = errors.New("agentcfg: invalid config payload")
	// ErrReservedUser — a user-scoped call carried a verified user id equal
	// to the reserved internal sentinel ("__agentcfg__"); fails closed so the
	// per-user key space can never alias onto the agent-level chain.
	ErrReservedUser = errors.New("agentcfg: user id collides with the reserved internal slot")
)

// ConfigScope is the durable-config ownership discriminator. One Registry
// implementation serves two keyings: the admin/tenant durable config (keyed
// under a synthetic per-agent slot) and the per-user durable config variant
// (keyed under the caller's real (tenant, user) with the agent id as a key).
//
// It is named with the ConfigScope prefix to disambiguate from the unrelated
// tools/auth.BindingScope constants (ScopeAgent / ScopeUser = OAuth token
// binding) and from the Protocol JWT scope auth.ScopeAgentConfigUser. The
// isolation tuple is never widened: the real user is the isolation principal
// for the user variant; the agent id stays a key, never a WHERE-clause
// isolation filter (RFC §6.16 / CLAUDE.md §6 clarifying note).
type ConfigScope uint8

const (
	// ConfigScopeAgent is the admin/tenant durable config, keyed under the
	// synthetic per-agent identity slot and the agentcfg.* record kinds. The
	// default (zero value).
	ConfigScopeAgent ConfigScope = iota
	// ConfigScopeUser is the per-user durable config variant, keyed under the
	// caller's real (tenant, user) with the agent id in the session slot and a
	// DISTINCT agentcfg.user.* record-kind prefix. The distinct prefix is the
	// structural guarantee the two key spaces can never alias.
	ConfigScopeUser
)

// SkillsSelection is the membership set of skill names active for an
// agent in a revision. The revision records skill MEMBERSHIP (names),
// never skill bodies — the bodies stay in the SkillStore. Resolved at run
// start; applied next-turn.
type SkillsSelection struct {
	// Names is the set of skill names active for the agent in this
	// revision. Order is not significant — the content hash is computed
	// over a canonical sorted, de-duplicated form so a re-ordering does
	// not perturb the hash.
	Names []string `json:"names"`
}

// PromptLayers is the prompt-layer section of the config envelope: an
// operator-owned, versioned base prompt layer plus an optional user layer
// that composes ABOVE the base without mutating it. The composition order
// is the structural security boundary — because Base and User are distinct
// fields and the user layer is always appended below the base, a writer of
// only User can extend the operator's guidance but can never precede,
// replace, or weaken the operator base.
type PromptLayers struct {
	// Base is the admin-owned, versioned base prompt layer. When set it is
	// the run's base system prompt (overriding the agent's configured
	// default base); unset inherits the configured default.
	Base *string `json:"base,omitempty"`
	// User is the optional higher user-instruction layer that composes
	// ABOVE the base without mutating it (the composition order is the
	// security boundary — the agent-config authorization model).
	User *string `json:"user,omitempty"`
}

// ToolExposure is the forward-compatible tool/MCP-exposure section of the
// config envelope. Reserved for the MCP pause / per-tool policy consumer;
// declared now so the envelope evolves without a schema break. Not wired
// in the first wave.
type ToolExposure struct {
	// PausedServers names MCP servers excluded from the next run's
	// projection (resume is a flag flip, not a re-dial).
	PausedServers []string `json:"paused_servers,omitempty"`
	// DisabledTools names tools the per-tool policy excludes.
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

// LLMParams pins an agent's sampling defaults as a versioned config
// section. Every field is pointer-optional: an unset field falls through
// to the next resolution layer (the tenant-wide baseline, then the config
// default) — the per-agent layer overrides ONLY the dimensions it sets.
// This is sampling parameters only; additive system-prompt text lives in
// PromptLayers (there is one home for prompt text).
type LLMParams struct {
	// Model, when set, is the model the agent's next run requests
	// (overriding the tenant-wide baseline / config default). An empty
	// string normalises to unset.
	Model *string `json:"model,omitempty"`
	// Temperature, when set, is the sampling temperature for the next run.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens, when set, is the per-message output-token ceiling.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// ReasoningEffort, when set, is the reasoning-effort hint
	// ("off" | "low" | "medium" | "high"). An empty string normalises to
	// unset.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// MCPTransport is the closed set of transports a runtime-added MCP
// connection descriptor may declare. The control-plane add path supports
// MCP over stdio (an operator-supplied argv command) and http (a URL);
// other transports are out of scope for the runtime add.
type MCPTransport string

// The canonical runtime-add MCP transports.
const (
	// MCPTransportStdio — the server is an operator-supplied subprocess,
	// launched argv-form (NEVER via a shell). The most privileged add (an
	// RCE surface) — allowlist-gated at the control-plane edge.
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportHTTP — the server is reachable over an HTTP(S) endpoint.
	MCPTransportHTTP MCPTransport = "http"
)

// MCPConnectionDescriptor is the NON-SECRET shape of one runtime-added MCP
// server connection, recorded in a config revision so the connection is
// part of the agent's versioned desired state (diff / rollback). It
// carries ONLY the non-secret descriptor — the server name, the transport,
// the stdio argv command or the http URL, the non-secret OAuth provider NAME,
// and the non-secret operator `_meta` annotations. Secret auth material
// (bearer headers, OAuth tokens, credentials, the minted downstream token) is
// NEVER part of this descriptor: it flows through the live attach + the
// tool-side OAuth / pause-resume path and is never persisted in a revision, a
// diff, or an event (CLAUDE.md §7). The `oauth_provider` field is a NAME, not
// a secret — it selects a config-declared acquisition strategy.
type MCPConnectionDescriptor struct {
	// Name is the unique MCP source id (the tool-name prefix the planner
	// sees). Required.
	Name string `json:"name"`
	// Transport is the wire transport — "stdio" or "http".
	Transport MCPTransport `json:"transport"`
	// Command is the stdio argv (argv[0] is the binary; no shell). Set for
	// the stdio transport; empty for http.
	Command []string `json:"command,omitempty"`
	// URL is the http(s) endpoint. Set for the http transport; empty for
	// stdio.
	URL string `json:"url,omitempty"`
	// OAuthProvider names a declared OAuth provider to bind for per-identity
	// southbound bearer injection on this connection's calls. NON-SECRET — a
	// provider NAME selecting a config-declared acquisition strategy; the
	// secret stays on the provider. Empty leaves the connection on its static
	// (attach-time) headers. Set only for the http transport.
	OAuthProvider string `json:"oauth_provider,omitempty"`
	// MetaAnnotations is a static, NON-SECRET set of operator-declared
	// key/values merged verbatim into the MCP `_meta` on every
	// identity-stamped per-call RPC (the deployment's attribution
	// vocabulary). Reserved / spec-prefixed keys are rejected at attach.
	MetaAnnotations map[string]string `json:"meta_annotations,omitempty"`
}

// ConnectionsSection is the runtime-added MCP-connection section of the
// config envelope: the set of NON-SECRET connection descriptors an admin
// has added over the control plane. Declared as its own section so an add
// REPLACES only this section, preserving Skills / ToolExposure /
// PromptLayers (the bidirectional section-merge invariant).
type ConnectionsSection struct {
	// Servers is the set of runtime-added MCP connection descriptors,
	// keyed by Name. Canonicalised sorted-by-name with a name-unique
	// invariant so a re-ordering does not perturb the content hash.
	Servers []MCPConnectionDescriptor `json:"servers,omitempty"`
}

// ConfigPayload is the forward-compatible config envelope. Every section
// is an optional pointer so later consumers extend the envelope without a
// schema break; only Skills is wired in the first wave.
type ConfigPayload struct {
	PromptLayers *PromptLayers       `json:"prompt_layers,omitempty"`
	ToolExposure *ToolExposure       `json:"tool_exposure,omitempty"`
	Skills       *SkillsSelection    `json:"skills,omitempty"`
	Connections  *ConnectionsSection `json:"connections,omitempty"`
	LLMParams    *LLMParams          `json:"llm_params,omitempty"`
}

// Revision is an immutable, content-addressed agent-config record with a
// parent pointer. SetRevision never mutates an existing Revision; it
// writes a new one and advances the active pointer.
type Revision struct {
	// RevisionID is the unique, time-ordered id of this revision.
	RevisionID string
	// ParentRevisionID is the revision this one descends from (the
	// previously-active revision). Empty for the first revision.
	ParentRevisionID string
	// ContentHash is the full hex-encoded SHA-256 over the revision's
	// canonical (sorted-key) payload encoding.
	ContentHash string
	// Author is the identity that wrote the revision (the audit anchor).
	Author identity.Quadruple
	// CreatedAt is the revision's creation instant (UTC).
	CreatedAt time.Time
	// Payload is the config envelope this revision pins.
	Payload ConfigPayload
}

// SkillsDiff is the structured set-diff of the skills membership across
// two revisions. Deterministic: Added / Removed are sorted.
type SkillsDiff struct {
	// Added are the skill names present in the to-revision but not the
	// from-revision.
	Added []string
	// Removed are the skill names present in the from-revision but not
	// the to-revision.
	Removed []string
}

// Changed reports whether the skills membership differs between the two
// revisions.
func (d SkillsDiff) Changed() bool { return len(d.Added) > 0 || len(d.Removed) > 0 }

// Diff is the server-side compare of two revision payloads. The first
// wave carries the structured skills set-diff; later consumers add a text
// diff for the prompt layer and structured diffs for tool exposure.
type Diff struct {
	// FromRevisionID / ToRevisionID name the compared revisions.
	FromRevisionID string
	ToRevisionID   string
	// Skills is the structured set-diff of the skills membership.
	Skills SkillsDiff
	// ToolExposure is the structured set-diff of the MCP-exposure /
	// per-tool policy.
	ToolExposure ToolExposureDiff
	// PromptLayers is the text delta of the base + user prompt layers.
	PromptLayers PromptLayersDiff
	// Connections is the structured set-diff of the runtime-added MCP
	// connection descriptors (by name).
	Connections ConnectionsDiff
	// LLMParams is the per-field delta of the per-agent LLM-parameter
	// section (model / temperature / max-tokens / reasoning-effort).
	LLMParams LLMParamsDiff
}

// Registry is the durable, identity-scoped, versioned desired-state
// store. It is a compiled artifact: immutable after construction and safe
// for concurrent reuse (the concurrent-reuse contract).
type Registry interface {
	// SetRevision writes a NEW immutable revision pinning payload and
	// advances the active pointer to it. An idempotent re-set of
	// byte-identical canonical content (relative to the current active
	// revision) is a no-op that returns the existing active revision.
	SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope, payload ConfigPayload) (Revision, error)
	// Active returns the agent's current active revision and whether one
	// exists. No active pointer returns (zero, false, nil).
	Active(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope) (Revision, bool, error)
	// Get returns the revision identified by revisionID. A missing
	// revision fails loud with ErrRevisionNotFound.
	Get(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope ConfigScope) (Revision, error)
	// ListRevisions returns the agent's revision chain newest-first,
	// capped at limit (0 = no cap). Enumeration uses the elevated
	// maintenance scan and filters to the agent's identity slot.
	ListRevisions(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope, limit int) ([]Revision, error)
	// Rollback writes a new active pointer to an existing revision
	// WITHOUT mutating or deleting any revision. A missing target fails
	// loud with ErrRevisionNotFound.
	Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope ConfigScope) (Revision, error)
	// Diff returns the deterministic compare of two existing revisions.
	Diff(ctx context.Context, id identity.Quadruple, agentID, fromRev, toRev string, scope ConfigScope) (Diff, error)
	// Close releases resources. Idempotent.
	Close(ctx context.Context) error
}

// NormalizePayload returns a defensive, canonicalised copy of payload:
// the skills membership and the tool-exposure sets (paused servers,
// disabled tools) are sorted and de-duplicated so a re-ordering does not
// perturb the content hash and the stored form is stable. The prompt-layer
// section is copied verbatim (no nested mutation). It is exported so
// consumers (the skills + tool-exposure services) and the driver share one
// canonical form.
func NormalizePayload(p ConfigPayload) ConfigPayload {
	out := ConfigPayload{}
	if p.PromptLayers != nil {
		// Normalise empty-string layers to nil so a semantically-equivalent
		// "inherit the default" state (whether the client sent `nil` or `""`)
		// produces one canonical form — equal states hash equal, no spurious
		// revision on a re-set, no phantom diff. A section that normalises to
		// both-nil drops out entirely.
		pl := &PromptLayers{Base: emptyToNil(p.PromptLayers.Base), User: emptyToNil(p.PromptLayers.User)}
		if pl.Base != nil || pl.User != nil {
			out.PromptLayers = pl
		}
	}
	if p.Skills != nil {
		out.Skills = &SkillsSelection{Names: sortDedup(p.Skills.Names)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &ToolExposure{
			PausedServers: sortDedup(p.ToolExposure.PausedServers),
			DisabledTools: sortDedup(p.ToolExposure.DisabledTools),
		}
	}
	if p.Connections != nil {
		servers := normalizeConnections(p.Connections.Servers)
		if len(servers) > 0 {
			out.Connections = &ConnectionsSection{Servers: servers}
		}
	}
	if p.LLMParams != nil {
		// Normalise empty Model / ReasoningEffort strings to nil so a
		// semantically-equivalent "inherit the next layer" state produces one
		// canonical form (equal states hash equal, no spurious revision, no
		// phantom diff). A section that normalises to all-nil drops out.
		lp := &LLMParams{
			Model:           emptyToNil(p.LLMParams.Model),
			Temperature:     copyFloatPtr(p.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(p.LLMParams.MaxTokens),
			ReasoningEffort: emptyToNil(p.LLMParams.ReasoningEffort),
		}
		if lp.Model != nil || lp.Temperature != nil || lp.MaxTokens != nil || lp.ReasoningEffort != nil {
			out.LLMParams = lp
		}
	}
	return out
}

// normalizeConnections returns a name-unique, sorted-by-name copy of the
// connection descriptors so a re-ordering or a re-add of the same name does
// not perturb the content hash. The LAST descriptor for a given name wins
// (a re-add replaces a prior descriptor of the same name). The argv Command
// order is significant and is preserved verbatim (never sorted). A nil
// input returns nil; an empty result returns nil so an all-empty section
// drops out of the canonical form.
// cloneStringMap returns a defensive copy of m (nil for an empty map) so a
// descriptor's annotations cannot be mutated through a retained reference.
func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func normalizeConnections(in []MCPConnectionDescriptor) []MCPConnectionDescriptor {
	if len(in) == 0 {
		return nil
	}
	byName := make(map[string]MCPConnectionDescriptor, len(in))
	names := make([]string, 0, len(in))
	for _, d := range in {
		if strings.TrimSpace(d.Name) == "" {
			continue
		}
		if _, seen := byName[d.Name]; !seen {
			names = append(names, d.Name)
		}
		byName[d.Name] = MCPConnectionDescriptor{
			Name:            d.Name,
			Transport:       d.Transport,
			Command:         append([]string(nil), d.Command...),
			URL:             d.URL,
			OAuthProvider:   d.OAuthProvider,
			MetaAnnotations: cloneStringMap(d.MetaAnnotations),
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]MCPConnectionDescriptor, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}

// ContentHash computes the full hex-encoded SHA-256 over the canonical
// (sorted-key) JSON encoding of the normalised payload. Computing over a
// canonical form means an unrelated re-ordering of a set does not change
// the hash, and the idempotent re-set check is exact.
func ContentHash(p ConfigPayload) (string, error) {
	canon, err := canonicalJSON(NormalizePayload(p))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON marshals v, re-decodes into a generic value, and
// re-marshals so map keys are emitted in sorted order (encoding/json
// sorts map[string]any keys). Struct field order is already deterministic;
// the round-trip makes the encoding canonical across any nested maps a
// later envelope section may introduce.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %w", ErrInvalidPayload, err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("%w: canonical re-decode: %w", ErrInvalidPayload, err)
	}
	canon, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical marshal: %w", ErrInvalidPayload, err)
	}
	return canon, nil
}

// sortDedup returns a sorted, de-duplicated copy of in. A nil input
// returns nil (preserving the absent/present distinction) — an empty
// non-nil slice returns an empty non-nil slice.
func sortDedup(in []string) []string {
	if in == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// emptyToNil collapses a pointer to an empty (or whitespace-only) string to
// nil, so "inherit the default" has one canonical representation regardless
// of whether the client sent a nil pointer or an empty string. A non-empty
// value is returned as-is (a fresh copy is not needed — callers treat the
// payload as immutable after normalisation).
func emptyToNil(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}

// copyFloatPtr / copyIntPtr return a fresh copy of a numeric pointer (nil
// stays nil) so the normalised payload never shares a pointer with the
// caller's input.
func copyFloatPtr(f *float64) *float64 {
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

// SkillNames returns the agent's active skills membership from a payload,
// or nil when the payload pins no skills section. A convenience for the
// skills consumer.
func (p ConfigPayload) SkillNames() []string {
	if p.Skills == nil {
		return nil
	}
	return p.Skills.Names
}

// PausedServers returns the agent's paused MCP server set from a payload,
// or nil when the payload pins no tool-exposure section. A convenience for
// the tool-exposure consumer + the run-start projection.
func (p ConfigPayload) PausedServers() []string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.PausedServers
}

// DisabledTools returns the agent's disabled tool set from a payload, or
// nil when the payload pins no tool-exposure section.
func (p ConfigPayload) DisabledTools() []string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.DisabledTools
}

// ConnectionDescriptors returns the agent's runtime-added MCP connection
// descriptors from a payload, or nil when the payload pins no connections
// section. A convenience for the add-connection consumer + the diff.
func (p ConfigPayload) ConnectionDescriptors() []MCPConnectionDescriptor {
	if p.Connections == nil {
		return nil
	}
	return p.Connections.Servers
}

// BasePrompt returns the agent's base prompt layer from a payload and
// whether it is set. A nil prompt-layer section or a nil Base returns
// ("", false). A convenience for the layered-prompt run-start projection.
func (p ConfigPayload) BasePrompt() (string, bool) {
	if p.PromptLayers == nil || p.PromptLayers.Base == nil {
		return "", false
	}
	return *p.PromptLayers.Base, true
}

// UserPrompt returns the agent's user prompt layer from a payload and
// whether it is set. A nil prompt-layer section or a nil User returns
// ("", false).
func (p ConfigPayload) UserPrompt() (string, bool) {
	if p.PromptLayers == nil || p.PromptLayers.User == nil {
		return "", false
	}
	return *p.PromptLayers.User, true
}

// LLMParamsView returns the agent's per-agent LLM-parameter section from a
// payload and whether it is set. A nil section returns (nil, false). The
// returned pointer is the payload's own (read-only) section — callers that
// mutate must copy first.
func (p ConfigPayload) LLMParamsView() (*LLMParams, bool) {
	if p.LLMParams == nil {
		return nil, false
	}
	return p.LLMParams, true
}

// PromptLayersDiff is the text delta of the base + user prompt layers
// across two revisions. An unset layer compares as the empty string, so a
// set→unset change registers as a change to "".
type PromptLayersDiff struct {
	// BaseChanged reports whether the base layer text differs.
	BaseChanged bool
	// BaseFrom / BaseTo are the base layer text in the from / to revision.
	BaseFrom string
	BaseTo   string
	// UserChanged reports whether the user layer text differs.
	UserChanged bool
	// UserFrom / UserTo are the user layer text in the from / to revision.
	UserFrom string
	UserTo   string
}

// Changed reports whether either prompt layer differs between the two
// revisions.
func (d PromptLayersDiff) Changed() bool { return d.BaseChanged || d.UserChanged }

// DiffPromptLayers computes the text delta of two prompt-layer states.
// Exported so the diff is one canonical implementation shared by the driver
// and tests. An unset layer is compared as the empty string.
func DiffPromptLayers(from, to ConfigPayload) PromptLayersDiff {
	fb, _ := from.BasePrompt()
	tb, _ := to.BasePrompt()
	fu, _ := from.UserPrompt()
	tu, _ := to.UserPrompt()
	return PromptLayersDiff{
		BaseChanged: fb != tb,
		BaseFrom:    fb,
		BaseTo:      tb,
		UserChanged: fu != tu,
		UserFrom:    fu,
		UserTo:      tu,
	}
}

// LLMParamsDiff is the per-field delta of the per-agent LLM-parameter
// section across two revisions. Each dimension reports whether it changed
// plus its from / to values (formatted for display; an unset dimension is
// the empty string). Deterministic.
type LLMParamsDiff struct {
	ModelChanged bool
	ModelFrom    string
	ModelTo      string

	TemperatureChanged bool
	TemperatureFrom    string
	TemperatureTo      string

	MaxTokensChanged bool
	MaxTokensFrom    string
	MaxTokensTo      string

	ReasoningEffortChanged bool
	ReasoningEffortFrom    string
	ReasoningEffortTo      string
}

// Changed reports whether any LLM-parameter dimension differs between the
// two revisions.
func (d LLMParamsDiff) Changed() bool {
	return d.ModelChanged || d.TemperatureChanged || d.MaxTokensChanged || d.ReasoningEffortChanged
}

// DiffLLMParams computes the per-field delta of two per-agent LLM-parameter
// states. Exported so the diff is one canonical implementation shared by
// the driver and tests. An unset dimension is compared as the empty string;
// numeric dimensions are formatted canonically (so 0.7 vs unset registers a
// change to/from "").
func DiffLLMParams(from, to ConfigPayload) LLMParamsDiff {
	fm, ft, fmax, fr := llmParamStrings(from.LLMParams)
	tm, tt, tmax, tr := llmParamStrings(to.LLMParams)
	return LLMParamsDiff{
		ModelChanged: fm != tm,
		ModelFrom:    fm,
		ModelTo:      tm,

		TemperatureChanged: ft != tt,
		TemperatureFrom:    ft,
		TemperatureTo:      tt,

		MaxTokensChanged: fmax != tmax,
		MaxTokensFrom:    fmax,
		MaxTokensTo:      tmax,

		ReasoningEffortChanged: fr != tr,
		ReasoningEffortFrom:    fr,
		ReasoningEffortTo:      tr,
	}
}

// llmParamStrings renders a (possibly nil) LLM-parameter section to its
// four canonical display strings (model, temperature, max-tokens,
// reasoning-effort); an unset section or dimension renders "".
func llmParamStrings(lp *LLMParams) (model, temperature, maxTokens, reasoningEffort string) {
	if lp == nil {
		return "", "", "", ""
	}
	if lp.Model != nil {
		model = *lp.Model
	}
	if lp.Temperature != nil {
		temperature = strconv.FormatFloat(*lp.Temperature, 'g', -1, 64)
	}
	if lp.MaxTokens != nil {
		maxTokens = strconv.Itoa(*lp.MaxTokens)
	}
	if lp.ReasoningEffort != nil {
		reasoningEffort = *lp.ReasoningEffort
	}
	return model, temperature, maxTokens, reasoningEffort
}

// ToolExposureDiff is the structured set-diff of the MCP-exposure /
// per-tool policy across two revisions. Deterministic: every slice is
// sorted.
type ToolExposureDiff struct {
	// PausedAdded / PausedResumed are the MCP servers newly paused / newly
	// resumed (present-in-to-but-not-from / present-in-from-but-not-to).
	PausedAdded   []string
	PausedResumed []string
	// DisabledAdded / DisabledEnabled are the tools newly disabled / newly
	// re-enabled.
	DisabledAdded   []string
	DisabledEnabled []string
}

// Changed reports whether the tool exposure differs between the two
// revisions.
func (d ToolExposureDiff) Changed() bool {
	return len(d.PausedAdded) > 0 || len(d.PausedResumed) > 0 ||
		len(d.DisabledAdded) > 0 || len(d.DisabledEnabled) > 0
}

// DiffToolExposure computes the structured set-diff of two tool-exposure
// states. Exported so the diff is one canonical implementation shared by
// the driver and tests.
func DiffToolExposure(from, to ConfigPayload) ToolExposureDiff {
	pAdded, pResumed := setDiff(from.PausedServers(), to.PausedServers())
	dAdded, dEnabled := setDiff(from.DisabledTools(), to.DisabledTools())
	return ToolExposureDiff{
		PausedAdded:     pAdded,
		PausedResumed:   pResumed,
		DisabledAdded:   dAdded,
		DisabledEnabled: dEnabled,
	}
}

// setDiff returns (in to but not from, in from but not to), each sorted.
func setDiff(from, to []string) (added, removed []string) {
	fromSet := make(map[string]struct{}, len(from))
	for _, s := range from {
		fromSet[s] = struct{}{}
	}
	toSet := make(map[string]struct{}, len(to))
	for _, s := range to {
		toSet[s] = struct{}{}
	}
	for s := range toSet {
		if _, ok := fromSet[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range fromSet {
		if _, ok := toSet[s]; !ok {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// ConnectionsDiff is the structured set-diff of the runtime-added MCP
// connection descriptors across two revisions, by name. Deterministic:
// every slice is sorted.
type ConnectionsDiff struct {
	// Added are the connection names present in the to-revision but not the
	// from-revision.
	Added []string
	// Removed are the connection names present in the from-revision but not
	// the to-revision.
	Removed []string
}

// Changed reports whether the connections set differs between the two
// revisions.
func (d ConnectionsDiff) Changed() bool { return len(d.Added) > 0 || len(d.Removed) > 0 }

// DiffConnections computes the structured set-diff (by name) of two
// connection states. Exported so the diff is one canonical implementation
// shared by the driver and tests.
func DiffConnections(from, to ConfigPayload) ConnectionsDiff {
	added, removed := setDiff(connectionNames(from.ConnectionDescriptors()), connectionNames(to.ConnectionDescriptors()))
	return ConnectionsDiff{Added: added, Removed: removed}
}

// connectionNames projects a descriptor slice onto its name set.
func connectionNames(in []MCPConnectionDescriptor) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, d := range in {
		out = append(out, d.Name)
	}
	return out
}

// DiffSkills computes the structured set-diff of two skills memberships.
// Exported so the diff is one canonical implementation shared by the
// driver and tests.
func DiffSkills(from, to []string) SkillsDiff {
	fromSet := make(map[string]struct{}, len(from))
	for _, s := range from {
		fromSet[s] = struct{}{}
	}
	toSet := make(map[string]struct{}, len(to))
	for _, s := range to {
		toSet[s] = struct{}{}
	}
	var added, removed []string
	for s := range toSet {
		if _, ok := fromSet[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range fromSet {
		if _, ok := toSet[s]; !ok {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return SkillsDiff{Added: added, Removed: removed}
}
