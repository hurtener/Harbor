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
	// ErrRevisionConflict — a conditional write declared an expected
	// content hash (SetOptions.ExpectedContentHash) and the agent's active
	// revision no longer carries it, or there is no active revision at all.
	// NOTHING is persisted: no revision record, no active-pointer move, no
	// emitted event. The caller re-reads the active revision and retries.
	ErrRevisionConflict = errors.New("agentcfg: revision conflict")
)

// ExpectNoActiveRevision is the reserved [SetOptions.ExpectedContentHash]
// value meaning "I read this agent BEFORE it had any config, and I expect it
// STILL to have none." It succeeds only when the agent has no active
// revision, and is refused with [ErrRevisionConflict] the moment any
// revision exists.
//
// # Why a sentinel exists at all
//
// The composition protocol the expected-revision token serves is: read,
// modify, write back with the read revision's content hash, so a concurrent
// sibling's write is refused rather than silently reverted. That protocol had
// no expressible form at its OWN base case. On an agent with no config,
// `agent_config.get` answers `set:false` with no hash to echo, and every
// non-empty token was refused — so the only token a first contributor could
// send was the empty one, which means "unconditional". Two contributors
// composing onto a fresh agent therefore BOTH wrote unconditionally and the
// second silently reverted the first: exactly the lost update the token
// exists to prevent, on the one write where a caller had no way to opt out.
//
// # Why this value
//
// A content hash is [ContentHash]'s output: exactly 64 lowercase hex
// characters. "-" is not a possible hash, so the sentinel can never collide
// with a real expectation no matter what payload is written — the property a
// sentinel living inside a hash-shaped field must have, and it is asserted by
// a test rather than argued.
//
// Returning a zero-state token from the READ instead was considered and
// rejected: it changes `agent_config.get`'s response shape (a new field on
// every read, mirrored into the Console client and the generated reference)
// to carry a value that would still have to be a reserved non-hash string —
// the same sentinel, plus a wire-shape change, plus a second place for it to
// drift.
//
// It is ADDITIVE. An absent token is still the unconditional write, and a
// 64-hex token still behaves exactly as it did.
const ExpectNoActiveRevision = "-"

// SetOptions carries the optional preconditions a durable spine write
// (SetRevision / Rollback) may declare. The ZERO value is the
// unconditional write — byte-for-byte the behaviour that shipped before
// this option existed, on every door.
type SetOptions struct {
	// ExpectedContentHash, when non-empty, requires the agent's ACTIVE
	// revision to carry exactly this content hash at write time. A
	// mismatch — or no active revision at all — fails with
	// ErrRevisionConflict and persists nothing.
	//
	// The reserved value [ExpectNoActiveRevision] ("-") inverts the
	// no-active-revision arm: it REQUIRES the agent to have no active
	// revision, and is refused once one exists. It is what makes the
	// read-modify-write composition protocol expressible at its base case,
	// where a read answers `set:false` and there is no hash to echo. A real
	// content hash is 64 lowercase hex characters, so the sentinel can never
	// collide with one.
	//
	// The expectation is compared against the content hash, not the
	// revision id, because a rollback repoints the active pointer WITHOUT
	// changing the content: a revision-id token would raise a false
	// conflict on the restore path. The guarded quantity is a value, so
	// the token is a value.
	//
	// # The bound on the guarantee — read this before relying on it
	//
	// The comparison is atomic against every other spine writer IN THIS
	// PROCESS, via the agent-config service's per-owner striped write
	// lock, which is held across each door's whole read-modify-write.
	//
	// It is NOT atomic across Runtime processes sharing one StateStore.
	// The StateStore has no compare-and-swap primitive — its own
	// interface godoc says it does not enforce CAS — and the active
	// pointer is written with a fresh event id on every save, so the slot
	// is overwritten unconditionally. Two Runtime processes sharing one
	// Postgres or SQLite store CAN STILL LOSE AN UPDATE, and this option
	// does not claim otherwise. Closing that gap needs a conditional-write
	// primitive on the StateStore interface across the persistence triad;
	// it is a separate change and is not pretended here.
	//
	// What the bounded guarantee is still worth: the window it closes is
	// the READ-TO-WRITE window — seconds to minutes for a human editing a
	// config in the Console, and however long an agent takes to compose
	// one. The window it cannot close cross-process is the driver's own
	// read-modify-write, on the order of microseconds. Single-process the
	// guard is exact; multi-process it is a large reduction in loss
	// probability and not an elimination.
	//
	// # It constrains only the writer that supplies it
	//
	// An unconditional writer can still clobber a conditional one, by
	// design: the token is a precondition on this write, never a lock on
	// the agent. A caller wanting exclusivity needs every writer of that
	// agent's config to participate, which is a deployment convention and
	// not something the runtime enforces — making the token mandatory
	// would break every caller that does not send one.
	ExpectedContentHash string
}

// CheckExpectedRevision evaluates opts against an active-revision snapshot.
// Persistence drivers use it at the authoritative write boundary. Protocol
// services also use it before a non-transactional side effect; that earlier
// check is an ordering guard and never replaces the driver's second check.
func CheckExpectedRevision(opts SetOptions, active Revision, hasActive bool) error {
	switch {
	case opts.ExpectedContentHash == "":
		return nil
	case opts.ExpectedContentHash == ExpectNoActiveRevision:
		if hasActive {
			return fmt.Errorf(
				"%w: expected no active revision, but revision %s is active carrying %q",
				ErrRevisionConflict, active.RevisionID, active.ContentHash)
		}
		return nil
	case !hasActive:
		return fmt.Errorf(
			"%w: expected content hash %q but the agent has no active revision (send %q to require that there be none)",
			ErrRevisionConflict, opts.ExpectedContentHash, ExpectNoActiveRevision)
	case active.ContentHash != opts.ExpectedContentHash:
		return fmt.Errorf(
			"%w: expected content hash %q, active revision %s carries %q",
			ErrRevisionConflict, opts.ExpectedContentHash, active.RevisionID, active.ContentHash)
	default:
		return nil
	}
}

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

// NamedBlock is one named, operator-authored unit of additive prompt text
// held in the ExtraSystemBlocks section. The Name is the ADDRESS a
// contributor uses to find and replace exactly its own contribution; the
// Body is rendered verbatim.
type NamedBlock struct {
	// Name addresses the block within the section. It is unique within the
	// section and drawn from a restricted identifier charset so it stays
	// legible in a transcript and in a diff. It is NEVER emitted as an XML
	// tag — attribution here is a data-model property, not a prompt-syntax
	// one, so the prompt's structural taxonomy never becomes a function of
	// caller input.
	Name string `json:"name"`
	// Body is the block's prompt text. It renders VERBATIM (unescaped) in
	// the operator-trusted additive position — see ExtraSystemBlocks for
	// the trust argument and the obligation it creates.
	Body string `json:"body"`
}

// ExtraSystemBlocks is the ORDERED list of named, operator-authored
// additive prompt blocks pinned by a config revision. It exists so N
// independent capability sources can each contribute — and later remove —
// exactly their own text, without any of them having to re-derive the
// others' contributions from prose.
//
// # Order is SEMANTIC
//
// The declared slice order IS the render order, so the canonical form
// PRESERVES it and a re-ordering is a real new revision with a different
// ContentHash and a visible diff. This is the deliberate asymmetry with
// SkillsSelection.Names (sortDedup'd) and OAuthProvidersSection.Providers
// (sorted by name): for those, order is not semantic and a re-ordering
// must NOT perturb the hash. Sorting blocks would silently re-order an
// operator's prompt and hide the change from the diff.
//
// # Trust — argued from the write door
//
// The section has exactly ONE write door, and it sits in the admin method
// set — the same tier that writes PromptLayers.Base, which is already
// rendered verbatim and is strictly more powerful (it is the whole spine).
// Blocks therefore render VERBATIM, unescaped, each preceded by a
// plain-text `[name]` label. Contrast PromptLayers.User, which IS escaped
// precisely because a claim-free session path can write it.
//
// The obligation this creates, stated rather than engineered away:
// ESCAPING IS NOT THE BOUNDARY HERE — the write door's authority tier is.
// A capability that wants to surface user-authored or model-authored text
// MUST NOT put it in a block; it uses the UNTRUSTED-framed memory tiers or
// PromptLayers.User instead.
type ExtraSystemBlocks struct {
	// Blocks is the ordered list. A slice, never a map: map iteration order
	// is not a composition order.
	Blocks []NamedBlock `json:"blocks"`
}

// ToolExposure is the forward-compatible tool/MCP-exposure section of the
// config envelope: the exclusion-based pause/disable sets (PausedServers /
// DisabledTools) plus the runtime loading-mode override maps
// (ServerLoadingModes / ToolLoadingModes).
//
// # Loading-mode precedence (pinned)
//
// Per descriptor, the effective LoadingMode resolves in this order:
//
//  1. ToolLoadingModes[name] — exact, unconditional (works on tool /
//     resource / prompt forms alike).
//  2. ServerLoadingModes[source] — the server-wide default, applied ONLY to
//     TOOL-form descriptors (tools.Tool.Form == tools.ToolFormTool); a
//     server-level override never blanket-flips that server's wrapped
//     resource/prompt descriptors.
//  3. The boot `tools.entries[].loading_mode` (already materialized into
//     the catalog's Tool.Loading at boot).
//  4. The driver/registration default.
//
// Layers 3-4 are already baked into the catalog's boot-effective
// tools.Tool.Loading, so a projection consumer applies exactly the top two
// layers over that boot-effective value (see
// internal/runtime/agentcfg/projection). Values are the closed set
// "always" | "deferred" — validated at the `agent_config.set_tool_exposure`
// wire edge BEFORE any registry write (CLAUDE.md §13: no invalid value is
// ever persisted). Loading is NOT capability-narrowing (a deferred tool
// stays reachable via `tool_search`), so it lives in the ADMIN tier only —
// the narrow-only user/session tiers gain no loading field.
type ToolExposure struct {
	// PausedServers names MCP servers excluded from the next run's
	// projection (resume is a flag flip, not a re-dial).
	PausedServers []string `json:"paused_servers,omitempty"`
	// DisabledTools names tools the per-tool policy excludes.
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// ServerLoadingModes overrides the default LoadingMode ("always" |
	// "deferred") for a server's TOOL-form descriptors, keyed by the
	// catalog's ToolSourceID. See the precedence order above.
	ServerLoadingModes map[string]string `json:"server_loading_modes,omitempty"`
	// ToolLoadingModes overrides the effective LoadingMode for one exact
	// catalog name ("always" | "deferred"), unconditionally. See the
	// precedence order above.
	ToolLoadingModes map[string]string `json:"tool_loading_modes,omitempty"`
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
	// key/values merged into the MCP `_meta` on every identity-stamped
	// per-call RPC (the deployment's attribution vocabulary). Each key is a
	// `_meta` PATH — a dotted key NESTS, exactly like `injection.meta_key`.
	// Keys are stored as declared; the nesting is a merge-time semantic.
	// Reserved / spec-prefixed keys (at the whole key OR any dot-segment),
	// over-deep paths, and paths colliding with another declared path on the
	// connection are rejected at validation.
	MetaAnnotations map[string]string `json:"meta_annotations,omitempty"`
	// OAuthDiscoveryAllowedOrigins is the explicit per-connection cross-origin
	// allow-list of public https origins the MCP OAuth-requirement discovery
	// walker may fetch authorization-server metadata from. NON-SECRET (an origin
	// allow-list, never a secret) — it rides the revision spine (versioned /
	// diffable / rollback-able) and is writable live over
	// `agent_config.set_mcp_discovery_origins`. Empty leaves the
	// authorization-server hop needs-allowance (partial discovery), never a
	// network hole: a granted origin is still refused at dial time if it
	// resolves private / loopback. Set only for the http transport.
	OAuthDiscoveryAllowedOrigins []string `json:"oauth_discovery_allowed_origins,omitempty"`
	// Injection, when set, binds this runtime-added connection to per-user
	// credential INJECTION for a RECEIVER-STYLE MCP server (the acting principal's
	// credential is pulled per outbound call from the named broker and delivered
	// in the declared form). NON-SECRET — it NAMES a boot-declared broker and
	// declares a target key/form; only the broker-pulled value is secret, and it
	// never rides this descriptor. Carried over the wire only behind the
	// fail-closed `tools.allow_wire_injection` opt-in; recorded here so the
	// binding is part of the agent's versioned desired state (diff / rollback).
	// Set only for the http transport; mutually exclusive with OAuthProvider.
	Injection *MCPCredentialInjectionDescriptor `json:"injection,omitempty"`
	// ArtifactByteEligible declares that this connection MAY receive
	// artifact BYTES through egress substitution: the runtime resolving an
	// artifact id the model authored and placing the resolved bytes into
	// the outbound tool-call body. NON-SECRET (a boolean declaration).
	//
	// It is the containment boundary for the feature. The reachable
	// artifact SET is unchanged by it — that stays the dispatching run's
	// own (tenant, user, session) — but the RECIPIENT widens: a remote
	// server may now be handed those bytes.
	//
	// Unlike its wire-writable siblings (the inline OAuth binding, the
	// injection mapping) it sits behind NO fail-closed boot opt-in, and
	// the asymmetry is deliberate: those gates exist because their fields
	// determine where a CREDENTIAL is sent, a boot-declared-only plane,
	// while this one determines where a user's OWN CONTENT is sent —
	// inside the co-tenant-admin trust boundary a shared runtime has
	// already accepted, with one-runtime-per-tenant as the stated remedy
	// for a deployment needing hard isolation. A boot gate would also
	// break the use case: a server attached over the control plane must be
	// usable without a redeploy.
	//
	// Every substitution is recorded (`mcp.artifact_egressed`,
	// fail-closed, before the wire request) — ids, sizes and a digest,
	// never the bytes. Set only for the http transport.
	ArtifactByteEligible bool `json:"artifact_byte_eligible,omitempty"`
	// ArtifactParams maps this server's TOOL names to the parameters on
	// those tools which carry artifact bytes. NON-SECRET (names only).
	// Requires ArtifactByteEligible. Each mapped parameter is validated
	// against the server's OWN discovered inputSchema at attach — it must
	// be declared there, and declared string-typed — so Harbor never
	// asserts an argument shape the server did not publish. Set only for
	// the http transport.
	ArtifactParams map[string][]string `json:"artifact_params,omitempty"`
}

// CloneArtifactParams returns a defensive deep copy of the descriptor's
// artifact-parameter mapping (nil for nil / empty), so a persisted
// revision and a live attach never share a backing map or slice.
func (d MCPConnectionDescriptor) CloneArtifactParams() map[string][]string {
	if len(d.ArtifactParams) == 0 {
		return nil
	}
	out := make(map[string][]string, len(d.ArtifactParams))
	for tool, params := range d.ArtifactParams {
		out[tool] = append([]string(nil), params...)
	}
	return out
}

// MCPCredentialInjectionDescriptor is the NON-SECRET per-connection mapping that
// binds a runtime-added receiver-style MCP server to per-user credential
// injection, recorded in a config revision as part of the agent's versioned
// desired state. It NAMES the broker the per-user credential is pulled from and
// declares WHERE the pulled value is placed on the outbound request (a header,
// an `Authorization: Basic` value, or a `_meta` key). No secret material lives
// here — only the broker-pulled value (resolved per-call from the ctx identity)
// is secret. It mirrors the boot `config.MCPCredentialInjectionConfig` so the
// runtime-add path and the boot path share ONE injection engine.
type MCPCredentialInjectionDescriptor struct {
	// Provider names the declared OAuth-provider broker the per-user credential is
	// pulled from. Required. NON-SECRET (a name).
	Provider string `json:"provider"`
	// Form selects the injection form: "header" / "basic" / "meta". Required.
	Form string `json:"form"`
	// Header is the target request header name for Form=="header". Required for
	// that form; a redaction-covered credential key, never `Authorization`.
	Header string `json:"header,omitempty"`
	// BasicUsername is the username half for Form=="basic" (the pulled credential
	// is the password half). Optional. NON-SECRET.
	BasicUsername string `json:"basic_username,omitempty"`
	// MetaKey is the target `_meta` key path for Form=="meta", dot-separated for
	// nesting. Required for that form; no reserved segment; redaction-covered leaf.
	MetaKey string `json:"meta_key,omitempty"`
}

// Clone returns a defensive copy of the injection descriptor (nil for nil).
func (d *MCPCredentialInjectionDescriptor) Clone() *MCPCredentialInjectionDescriptor {
	if d == nil {
		return nil
	}
	c := *d
	return &c
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

// OAuthProviderDescriptor is the ZERO-URL, Protocol-installed OAuth provider
// descriptor recorded in a config revision so the provider is part of the
// agent's versioned desired state (diff / rollback). It carries NO URL of any
// kind, NO env-var name, and NO literal secret: the credential-plane invariant
// is that no admin-writable field may determine where a credential is
// sent, so every credential-sink-determining value (the token endpoint, the
// credential-pull endpoint, the allowed downstream hosts, the audience, and the
// scope ceiling) is pinned at boot on the NAMED credential broker (the boot broker's
// broker list) the descriptor references by non-secret NAME. The wire struct
// exposes NO field that is a URL or an env-var name; a decode carrying
// `token_url` / `auth_url` / `client_id_env` / `client_secret_env` / `remote`
// is rejected BY NAME (DisallowUnknownFields — the field cannot exist).
type OAuthProviderDescriptor struct {
	// Name is the unique provider name (the binding a connection's
	// oauth_provider references). Required.
	Name string `json:"name"`
	// Driver is the OAuth flow driver — validated to be exactly
	// "tokenexchange" (the only writable driver; the non-interactive PULL
	// exchange). Required.
	Driver string `json:"driver"`
	// CredentialSource is the credential-source seam — validated to be exactly
	// "remote" (broker-pull). An empty value is a LOUD reject (in config
	// "" means the `env` source, which this shape forbids). Required.
	CredentialSource string `json:"credential_source"`
	// CredentialBroker names a boot-declared credential broker that
	// pins the runtime's OWN credential custody. Required in BOTH the name-only
	// and the dev-gated wire shape — the wire descriptor still names a boot broker
	// so no credential-source URL or secret ever rides the wire. NON-SECRET.
	CredentialBroker string `json:"credential_broker"`
	// Scopes is the requested OAuth scope subset. NON-SECRET. Clamped to the
	// broker's boot scope ceiling at build time — a scope outside the ceiling is
	// dropped, never honoured (an installed descriptor can never widen scope).
	// Optional.
	Scopes []string `json:"scopes,omitempty"`
	// TokenURL is the dev-gated wire-carried RFC-8693 token-exchange endpoint of
	// the NEW server. Persisted (NON-SECRET, a URL); accepted only behind the
	// `allow_wire_oauth_descriptor` boot opt-in and dialed through the
	// token-exchange SSRF backstop. Empty for the name-only shape.
	TokenURL string `json:"token_url,omitempty"`
	// Audience is the dev-gated wire-carried exchanged-token audience of the NEW
	// server. Persisted (NON-SECRET). Empty for the name-only shape.
	Audience string `json:"audience,omitempty"`
	// AllowedDownstreamHosts is the DERIVED downstream sink allow-list for a
	// wire-installed provider — set by the runtime from the bound connection's
	// own URL, NEVER from a wire field. Persisted (NON-SECRET) so a rollback /
	// reconcile rebuilds the provider with the same derived sink. Empty for the
	// name-only shape (its sink is the boot broker's).
	AllowedDownstreamHosts []string `json:"allowed_downstream_hosts,omitempty"`
}

// LLMProviderDescriptor is the domain shape of one Protocol-installed,
// ZERO-URL inference provider binding — the transport shape the
// inference-plane provider installer seam consumes. It carries NO URL and NO
// env-var name: every sink-determining value lives on the boot-declared
// inference broker it references by non-secret name (the credential-plane
// invariant). Distinct from [OAuthProviderDescriptor]: a different
// credential plane (an LLM provider KEY, not an OAuth client credential).
type LLMProviderDescriptor struct {
	// Name is the unique provider-binding name. Required.
	Name string `json:"name"`
	// Provider is the LLM provider the pulled key authenticates (e.g.
	// "openai"). Required.
	Provider string `json:"provider"`
	// CredentialSource is validated to be exactly "remote" (broker-pull).
	// Required.
	CredentialSource string `json:"credential_source"`
	// InferenceBroker names a boot-declared inference broker that pins every
	// credential sink. Required; an unknown name fails loud. NON-SECRET.
	InferenceBroker string `json:"inference_broker"`
	// ModelAllow is the optional model-name allowlist. NON-SECRET.
	ModelAllow []string `json:"model_allow,omitempty"`
}

// OAuthProvidersSection is the Protocol-installed OAuth-provider section of the
// config envelope: the set of ZERO-URL provider descriptors an admin has
// installed over the control plane. Declared as its own section so an install /
// uninstall REPLACES only this section, preserving every sibling (the
// section-merge invariant).
type OAuthProvidersSection struct {
	// Providers is the set of installed provider descriptors, keyed by Name.
	// Canonicalised sorted-by-name with a name-unique invariant so a
	// re-ordering does not perturb the content hash.
	Providers []OAuthProviderDescriptor `json:"providers,omitempty"`
}

// RunCompletionHook is the durable, versioned run-completion hook
// configuration in a config revision: the catalog tool the run transcript is
// dispatched to at the run loop's terminal boundary, plus an optional
// dispatch timeout. It mirrors the static `runtime.hooks.run_completion`
// yaml; resolution at run start is agent-config over yaml over no hook.
type RunCompletionHook struct {
	// Tool is the catalog tool name the transcript is dispatched to. An empty
	// tool normalises the RunCompletion to nil BUT preserves the enclosing
	// hooks section (a present section with no tool is the explicit per-agent
	// no-hook that overrides a yaml fleet hook at run start).
	Tool string `json:"tool"`
	// TimeoutMS is the dispatch timeout in milliseconds. Zero falls through
	// to the runtime default at run start. Negative is rejected at set time.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// HooksSection is the run-lifecycle-hook section of the config envelope.
// Declared as its own section so a set REPLACES only this section,
// preserving the sibling sections (the section-merge invariant).
//
// Section PRESENCE is authoritative (mirrors NamingSection): a NIL section
// means "no per-agent hook policy" (the yaml `runtime.hooks.run_completion`
// fleet default, then no hook, resolves at run start). A PRESENT section with
// a non-nil RunCompletion (non-empty tool) pins the agent's hook; a PRESENT
// section with a nil / empty-tool RunCompletion is an explicit per-agent
// NO-HOOK that OVERRIDES a yaml-on fleet default — normalization
// preserves any non-nil section (never an inert-drop).
type HooksSection struct {
	// RunCompletion pins the agent's run-completion hook when non-nil with a
	// non-empty tool. Nil in a PRESENT section is the explicit no-hook.
	RunCompletion *RunCompletionHook `json:"run_completion,omitempty"`
}

// NamingSection is the session auto-naming policy section of the config
// envelope. Declared as its own optional section so a set REPLACES only this
// section, preserving the sibling sections (the section-merge invariant).
// Opt-in, default off: a NIL section means "no per-agent policy" (the yaml
// `runtime.naming` fleet default, then off, resolves at run start). A PRESENT
// section is authoritative either way: Auto=true enables, Auto=false is an
// explicit per-agent OPT-OUT that overrides a yaml-on fleet default — section
// presence is the operator's signal, and normalization preserves any non-nil
// section verbatim (never an inert-drop).
type NamingSection struct {
	// Auto enables session auto-naming for the agent. False (the zero value)
	// in a PRESENT section is an explicit off that overrides the yaml fleet
	// default.
	Auto bool `json:"auto,omitempty"`
	// AfterTurns is the completed-run count at which the FIRST auto-name
	// fires (fire on the Nth completed run). Zero resolves to the default
	// (1) at run start; a negative value is rejected at set time.
	AfterTurns int `json:"after_turns,omitempty"`
	// RepeatEvery, when > 0, re-names every N completed turns after the
	// first; 0 (the default) names once only. Negative is rejected at set
	// time.
	RepeatEvery int `json:"repeat_every,omitempty"`
	// MaxRepetitions is the hard cap on the TOTAL number of auto-namings
	// (including the first). It is REQUIRED ≥ 1 whenever RepeatEvery > 0 —
	// no unlimited value exists, so unbounded periodic re-naming is
	// unrepresentable. Ignored when RepeatEvery == 0 (naming happens once).
	MaxRepetitions int `json:"max_repetitions,omitempty"`
	// MaxTitleLen bounds the auto title (runes). Zero resolves to the
	// default (80) at run start; a set value must be in [8, 200].
	MaxTitleLen int `json:"max_title_len,omitempty"`
	// Model, when set, is the model the auto-naming Complete call requests
	// (validated against ModelProfiles at set time); empty uses the run's
	// effective model.
	Model string `json:"model,omitempty"`
}

// ConfigPayload is the forward-compatible config envelope. Every section
// is an optional pointer so later consumers extend the envelope without a
// schema break; only Skills is wired in the first wave.
type ConfigPayload struct {
	PromptLayers   *PromptLayers          `json:"prompt_layers,omitempty"`
	ToolExposure   *ToolExposure          `json:"tool_exposure,omitempty"`
	Skills         *SkillsSelection       `json:"skills,omitempty"`
	Connections    *ConnectionsSection    `json:"connections,omitempty"`
	OAuthProviders *OAuthProvidersSection `json:"oauth_providers,omitempty"`
	LLMParams      *LLMParams             `json:"llm_params,omitempty"`
	Hooks          *HooksSection          `json:"hooks,omitempty"`
	Naming         *NamingSection         `json:"naming,omitempty"`
	// ExtraSystemBlocks, when non-nil, pins the agent's ORDERED list of
	// named additive prompt blocks. Absent (nil) contributes nothing and
	// leaves the system prompt byte-identical.
	ExtraSystemBlocks *ExtraSystemBlocks `json:"extra_system_blocks,omitempty"`
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
	// OAuthProviders is the structured set-diff of the Protocol-installed
	// OAuth-provider descriptors (by name).
	OAuthProviders OAuthProvidersDiff
	// LLMParams is the per-field delta of the per-agent LLM-parameter
	// section (model / temperature / max-tokens / reasoning-effort).
	LLMParams LLMParamsDiff
	// Hooks is the per-field delta of the run-lifecycle-hook section
	// (the run-completion hook's tool + timeout).
	Hooks HooksDiff
	// Naming is the per-field delta of the session auto-naming policy
	// section.
	Naming NamingDiff
	// ExtraSystemBlocks is the structured delta of the ordered additive
	// prompt blocks (added / removed / body-changed by name, plus the
	// order-only reorder flag).
	ExtraSystemBlocks ExtraSystemBlocksDiff
}

// Registry is the durable, identity-scoped, versioned desired-state
// store. It is a compiled artifact: immutable after construction and safe
// for concurrent reuse (the concurrent-reuse contract).
type Registry interface {
	// SetRevision writes a NEW immutable revision pinning payload and
	// advances the active pointer to it. An idempotent re-set of
	// byte-identical canonical content (relative to the current active
	// revision) is a no-op that returns the existing active revision.
	//
	// opts carries the optional precondition. Its zero value is the
	// unconditional write. When opts.ExpectedContentHash is set, the
	// precondition is evaluated BEFORE the idempotent-re-set
	// short-circuit, so a stale expectation is never converted into a
	// misleading success by a payload that happens to equal the current
	// content; a failed precondition returns ErrRevisionConflict and
	// persists nothing.
	SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope ConfigScope, payload ConfigPayload, opts SetOptions) (Revision, error)
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
	//
	// opts carries the same optional precondition SetRevision takes: when
	// opts.ExpectedContentHash is set, the CURRENTLY-ACTIVE revision must
	// carry that content hash or the repoint is refused with
	// ErrRevisionConflict and the pointer is left where it was.
	Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope ConfigScope, opts SetOptions) (Revision, error)
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
			PausedServers:      sortDedup(p.ToolExposure.PausedServers),
			DisabledTools:      sortDedup(p.ToolExposure.DisabledTools),
			ServerLoadingModes: normalizeLoadingModes(p.ToolExposure.ServerLoadingModes),
			ToolLoadingModes:   normalizeLoadingModes(p.ToolExposure.ToolLoadingModes),
		}
	}
	if p.Connections != nil {
		servers := normalizeConnections(p.Connections.Servers)
		if len(servers) > 0 {
			out.Connections = &ConnectionsSection{Servers: servers}
		}
	}
	if p.OAuthProviders != nil {
		providers := normalizeOAuthProviders(p.OAuthProviders.Providers)
		if len(providers) > 0 {
			out.OAuthProviders = &OAuthProvidersSection{Providers: providers}
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
	if p.Hooks != nil {
		// A non-nil hooks section is ALWAYS preserved (mirrors the naming
		// section) — section PRESENCE is the operator's signal. A present hooks
		// section with no/empty run-completion tool is an explicit per-agent
		// NO-HOOK that overrides a yaml fleet hook at run start; dropping
		// it as "inert" would silently discard the opt-out and keep dispatching
		// the yaml hook (leaking the run transcript the operator meant to stop).
		// A run-completion hook with a non-empty tool is preserved (whitespace
		// trimmed; a negative TimeoutMS normalises to 0, defaulted at run start —
		// set-time validation is where a negative is rejected); an empty /
		// whitespace-only tool canonicalises the RunCompletion to nil so
		// `{run_completion:{tool:""}}` and a bare `{}` share ONE canonical form
		// (equal states hash equal, no spurious revision), while both stay
		// distinguishable from an ABSENT (nil) section.
		hs := &HooksSection{}
		if p.Hooks.RunCompletion != nil {
			tool := strings.TrimSpace(p.Hooks.RunCompletion.Tool)
			if tool != "" {
				timeout := p.Hooks.RunCompletion.TimeoutMS
				if timeout < 0 {
					timeout = 0
				}
				hs.RunCompletion = &RunCompletionHook{Tool: tool, TimeoutMS: timeout}
			}
		}
		out.Hooks = hs
	}
	if p.ExtraSystemBlocks != nil {
		// ORDER-PRESERVING by construction. normalizeNamedBlocks trims and
		// de-duplicates but NEVER sorts — the declared order is the render
		// order, so a re-ordering must change the content hash and appear in
		// the diff. An all-empty section drops out of the canonical form.
		blocks := normalizeNamedBlocks(p.ExtraSystemBlocks.Blocks)
		if len(blocks) > 0 {
			out.ExtraSystemBlocks = &ExtraSystemBlocks{Blocks: blocks}
		}
	}
	if p.Naming != nil {
		// A non-nil naming section is ALWAYS preserved (model trimmed to the
		// canonical "inherit the run's effective model" empty form) — section
		// PRESENCE is the operator's signal, so presence-vs-absence stays
		// distinguishable through normalization. A bare `{auto: false}`
		// section is an explicit per-agent OPT-OUT that must override a yaml
		// fleet default at run start; dropping it as "inert" (the hooks
		// empty-tool posture) would silently discard the opt-out and keep the
		// agent auto-naming (and spending). The run-start projection's
		// section-present branch treats Auto=false as the explicit off.
		out.Naming = &NamingSection{
			Auto:           p.Naming.Auto,
			AfterTurns:     p.Naming.AfterTurns,
			RepeatEvery:    p.Naming.RepeatEvery,
			MaxRepetitions: p.Naming.MaxRepetitions,
			MaxTitleLen:    p.Naming.MaxTitleLen,
			Model:          strings.TrimSpace(p.Naming.Model),
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

// normalizeLoadingModes returns a defensive copy of m with empty-key entries
// dropped (an empty catalog name / source id is never a meaningful override
// key) so an all-empty or all-dropped map normalizes to nil — a phantom
// override never perturbs the content hash. Values are passed through
// unvalidated here (the `agent_config.set_tool_exposure` wire edge is the
// fail-loud validation gate, CLAUDE.md §13); Go's canonical JSON encoding
// already sorts map keys, so no explicit key-sort is needed for hash
// stability.
func normalizeLoadingModes(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
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
			Name:                         d.Name,
			Transport:                    d.Transport,
			Command:                      append([]string(nil), d.Command...),
			URL:                          d.URL,
			OAuthProvider:                d.OAuthProvider,
			MetaAnnotations:              cloneStringMap(d.MetaAnnotations),
			OAuthDiscoveryAllowedOrigins: append([]string(nil), d.OAuthDiscoveryAllowedOrigins...),
			Injection:                    d.Injection.Clone(),
			// The egress-substitution declaration is part of the versioned
			// desired state, so the canonicaliser must carry it. A field
			// omitted here is silently dropped on the way to the revision —
			// the connection reads back non-eligible and the operator's
			// declaration evaporates between the door that accepted it and
			// the spine that stores it.
			ArtifactByteEligible: d.ArtifactByteEligible,
			ArtifactParams:       d.CloneArtifactParams(),
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

// normalizeNamedBlocks returns a name-unique copy of the additive prompt
// blocks IN THEIR DECLARED ORDER. Blocks with an empty (or whitespace-only)
// name or body are dropped — a phantom block never perturbs the content
// hash. The FIRST occurrence of a name wins and holds its position (the
// write door refuses a duplicate outright, so this is a defensive
// canonicalisation for the direct SetRevision door, not a merge rule).
//
// It deliberately does NOT sort. Skills.Names is sortDedup'd and
// OAuthProviders is sorted by name, both because "a re-ordering of a set
// does not change the hash" (ContentHash's own godoc). For blocks a
// re-ordering DOES change the rendered prompt, so it MUST change the hash.
// Adding a sort here — to make the section "consistent with its siblings" —
// is the mutation the order-preservation and order-is-semantic tests exist
// to turn red.
//
// A nil input returns nil; an all-dropped input returns nil so an
// all-empty section drops out of the canonical form.
func normalizeNamedBlocks(in []NamedBlock) []NamedBlock {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]NamedBlock, 0, len(in))
	for _, b := range in {
		name := strings.TrimSpace(b.Name)
		if name == "" || strings.TrimSpace(b.Body) == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, NamedBlock{Name: name, Body: b.Body})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeOAuthProviders returns a name-unique, sorted-by-name copy of the
// installed provider descriptors so a re-ordering or a re-install of the same
// name does not perturb the content hash. The LAST descriptor for a given name
// wins (a re-install replaces a prior descriptor of the same name). Scopes are
// sorted+deduplicated. A nil input returns nil; an empty result returns nil so
// an all-empty section drops out of the canonical form.
func normalizeOAuthProviders(in []OAuthProviderDescriptor) []OAuthProviderDescriptor {
	if len(in) == 0 {
		return nil
	}
	byName := make(map[string]OAuthProviderDescriptor, len(in))
	names := make([]string, 0, len(in))
	for _, d := range in {
		if strings.TrimSpace(d.Name) == "" {
			continue
		}
		if _, seen := byName[d.Name]; !seen {
			names = append(names, d.Name)
		}
		var hosts []string
		if len(d.AllowedDownstreamHosts) > 0 {
			hosts = sortDedup(d.AllowedDownstreamHosts)
		}
		byName[d.Name] = OAuthProviderDescriptor{
			Name:                   d.Name,
			Driver:                 d.Driver,
			CredentialSource:       d.CredentialSource,
			CredentialBroker:       d.CredentialBroker,
			Scopes:                 sortDedup(d.Scopes),
			TokenURL:               d.TokenURL,
			Audience:               d.Audience,
			AllowedDownstreamHosts: hosts,
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]OAuthProviderDescriptor, 0, len(names))
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

// ServerLoadingModes returns the agent's per-server loading-mode override map
// from a payload, or nil when the payload pins no tool-exposure section. A
// convenience for the tool-exposure consumer + the run-start projection.
func (p ConfigPayload) ServerLoadingModes() map[string]string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.ServerLoadingModes
}

// ToolLoadingModes returns the agent's per-tool loading-mode override map
// from a payload, or nil when the payload pins no tool-exposure section.
func (p ConfigPayload) ToolLoadingModes() map[string]string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.ToolLoadingModes
}

// RunCompletionHookView returns the agent's run-completion hook from a
// payload and whether it is set. A nil hooks section or a nil RunCompletion
// returns (nil, false). The returned pointer is the payload's own
// (read-only) section — callers that mutate must copy first. A convenience
// for the run-completion-hook run-start projection.
func (p ConfigPayload) RunCompletionHookView() (*RunCompletionHook, bool) {
	if p.Hooks == nil || p.Hooks.RunCompletion == nil {
		return nil, false
	}
	return p.Hooks.RunCompletion, true
}

// NamingView returns the agent's session auto-naming section from a payload
// and whether it is set. A nil section returns (nil, false). The returned
// pointer is the payload's own (read-only) section — callers that mutate must
// copy first. A convenience for the auto-naming run-start projection.
func (p ConfigPayload) NamingView() (*NamingSection, bool) {
	if p.Naming == nil {
		return nil, false
	}
	return p.Naming, true
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

// OAuthProviderDescriptors returns the installed OAuth-provider descriptors
// from a payload, or nil when the payload pins no OAuth-providers section. A
// convenience for the install/uninstall consumer, the reconcile leg, and the
// diff.
func (p ConfigPayload) OAuthProviderDescriptors() []OAuthProviderDescriptor {
	if p.OAuthProviders == nil {
		return nil
	}
	return p.OAuthProviders.Providers
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

// ExtraSystemBlockList returns the agent's ordered additive prompt blocks
// from a payload, or nil when the payload pins no blocks section. The
// returned slice is the payload's own (read-only) backing array — callers
// that mutate must copy first. A convenience for the run-start projection
// and the diff.
func (p ConfigPayload) ExtraSystemBlockList() []NamedBlock {
	if p.ExtraSystemBlocks == nil {
		return nil
	}
	return p.ExtraSystemBlocks.Blocks
}

// ExtraSystemBlocksDiff is the structured delta of the additive prompt
// blocks across two revisions. Added / Removed / Changed are sorted so the
// diff is deterministic; Reordered is the ORDER-only signal that has no
// analogue on the sorted sibling sections, and it is the reason the
// normalizer must not sort.
type ExtraSystemBlocksDiff struct {
	// Added are the block names present only in the to-revision.
	Added []string
	// Removed are the block names present only in the from-revision.
	Removed []string
	// Changed are the names present in both whose BODY differs.
	Changed []string
	// Reordered reports that the two revisions carry the SAME name set in a
	// DIFFERENT order. Order is render order, so a pure re-ordering is a
	// real prompt change and must be visible in the diff.
	Reordered bool
}

// Changed reports whether the blocks section differs between the two
// revisions, including an order-only change.
func (d ExtraSystemBlocksDiff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Changed) > 0 || d.Reordered
}

// DiffExtraSystemBlocks computes the structured delta of two block
// sections. Exported so the diff is one canonical implementation shared by
// the driver and tests. An absent section compares as an empty list.
func DiffExtraSystemBlocks(from, to ConfigPayload) ExtraSystemBlocksDiff {
	fromBlocks := from.ExtraSystemBlockList()
	toBlocks := to.ExtraSystemBlockList()

	fromBody := make(map[string]string, len(fromBlocks))
	fromOrder := make([]string, 0, len(fromBlocks))
	for _, b := range fromBlocks {
		fromBody[b.Name] = b.Body
		fromOrder = append(fromOrder, b.Name)
	}
	toBody := make(map[string]string, len(toBlocks))
	toOrder := make([]string, 0, len(toBlocks))
	for _, b := range toBlocks {
		toBody[b.Name] = b.Body
		toOrder = append(toOrder, b.Name)
	}

	var d ExtraSystemBlocksDiff
	for _, name := range toOrder {
		body, ok := fromBody[name]
		switch {
		case !ok:
			d.Added = append(d.Added, name)
		case body != toBody[name]:
			d.Changed = append(d.Changed, name)
		}
	}
	for _, name := range fromOrder {
		if _, ok := toBody[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)

	// Reordered is the SAME-NAME-SET, different-position signal. It is
	// computed only when neither side added nor removed a name, so an
	// add/remove that necessarily shifts positions does not also raise a
	// misleading reorder flag.
	if len(d.Added) == 0 && len(d.Removed) == 0 {
		for i := range toOrder {
			if fromOrder[i] != toOrder[i] {
				d.Reordered = true
				break
			}
		}
	}
	return d
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

// HooksDiff is the per-field delta of the run-lifecycle-hook section across
// two revisions. Reports whether the run-completion hook's tool / timeout
// changed plus their from / to display values (an unset hook renders "").
type HooksDiff struct {
	// SectionPresentChanged reports whether the hooks-section PRESENCE differs
	// (absent vs present). A present-but-empty section (the explicit per-agent
	// no-hook) renders no tool and no timeout, so WITHOUT this dimension
	// a yaml-hook-on agent toggling to an explicit `{}` opt-out would be an
	// invisible revision in agent_config.diff (a phantom revision in the
	// Console diff view). Mirrors NamingDiff's tri-state Auto. Present renders
	// "present"; absent renders "".
	SectionPresentChanged bool
	SectionPresentFrom    string
	SectionPresentTo      string
	// RunCompletionToolChanged reports whether the hook's target tool differs.
	RunCompletionToolChanged bool
	RunCompletionToolFrom    string
	RunCompletionToolTo      string
	// RunCompletionTimeoutChanged reports whether the hook's timeout differs.
	RunCompletionTimeoutChanged bool
	RunCompletionTimeoutFrom    string
	RunCompletionTimeoutTo      string
}

// Changed reports whether the run-lifecycle-hook section differs between the
// two revisions.
func (d HooksDiff) Changed() bool {
	return d.SectionPresentChanged || d.RunCompletionToolChanged || d.RunCompletionTimeoutChanged
}

// DiffHooks computes the per-field delta of two run-lifecycle-hook states.
// Exported so the diff is one canonical implementation shared by the driver
// and tests. An ABSENT section renders no presence + empty tool/timeout; a
// PRESENT-but-empty section (explicit no-hook) renders presence "present" with
// empty tool/timeout — so absent→`{}` (and its inverse) registers as a change
// even though tool and timeout are empty on both sides.
func DiffHooks(from, to ConfigPayload) HooksDiff {
	fp := hooksSectionPresence(from)
	tp := hooksSectionPresence(to)
	ft, ftimeout := runCompletionHookStrings(from)
	tt, ttimeout := runCompletionHookStrings(to)
	return HooksDiff{
		SectionPresentChanged:       fp != tp,
		SectionPresentFrom:          fp,
		SectionPresentTo:            tp,
		RunCompletionToolChanged:    ft != tt,
		RunCompletionToolFrom:       ft,
		RunCompletionToolTo:         tt,
		RunCompletionTimeoutChanged: ftimeout != ttimeout,
		RunCompletionTimeoutFrom:    ftimeout,
		RunCompletionTimeoutTo:      ttimeout,
	}
}

// hooksSectionPresence renders a payload's hooks-section presence as the
// diffable display string: "present" when the section is present (with or
// without a run-completion tool), "" when absent. Presence is semantic — a
// present bare `{}` is an explicit per-agent no-hook that overrides a yaml
// fleet hook.
func hooksSectionPresence(p ConfigPayload) string {
	if p.Hooks != nil {
		return "present"
	}
	return ""
}

// runCompletionHookStrings renders a payload's run-completion hook to its
// canonical display strings (tool, timeout-ms); an unset hook (absent section
// OR a present section with no run-completion tool) renders ("", "").
func runCompletionHookStrings(p ConfigPayload) (tool, timeout string) {
	rc, ok := p.RunCompletionHookView()
	if !ok {
		return "", ""
	}
	tool = rc.Tool
	if rc.TimeoutMS != 0 {
		timeout = strconv.Itoa(rc.TimeoutMS)
	}
	return tool, timeout
}

// NamingDiff is the per-field delta of the session auto-naming policy
// section across two revisions. Each dimension reports whether it changed
// plus its from / to display values (an unset section renders every field
// as ""). Deterministic.
//
// Auto is a TRI-STATE display string — "" (section ABSENT) / "false" /
// "true" — because section PRESENCE is semantic: a present bare
// `{auto: false}` section is an explicit per-agent opt-out that overrides a
// yaml-on fleet default, while an absent section falls through to yaml. A
// two-state bool would render absent and bare-opt-out identically, making
// exactly that revision invisible to `agent_config.diff` (a phantom
// revision in the Console diff view).
type NamingDiff struct {
	AutoChanged bool
	AutoFrom    string
	AutoTo      string

	AfterTurnsChanged bool
	AfterTurnsFrom    string
	AfterTurnsTo      string

	RepeatEveryChanged bool
	RepeatEveryFrom    string
	RepeatEveryTo      string

	MaxRepetitionsChanged bool
	MaxRepetitionsFrom    string
	MaxRepetitionsTo      string

	MaxTitleLenChanged bool
	MaxTitleLenFrom    string
	MaxTitleLenTo      string

	ModelChanged bool
	ModelFrom    string
	ModelTo      string
}

// Changed reports whether any auto-naming dimension differs between the two
// revisions.
func (d NamingDiff) Changed() bool {
	return d.AutoChanged || d.AfterTurnsChanged || d.RepeatEveryChanged ||
		d.MaxRepetitionsChanged || d.MaxTitleLenChanged || d.ModelChanged
}

// DiffNaming computes the per-field delta of two session auto-naming states.
// Exported so the diff is one canonical implementation shared by the driver
// and tests. An ABSENT section renders every dimension "" — including Auto
// (the tri-state), so absent→bare-`{auto:false}` ("" → "false") and its
// inverse register as changes even though every OTHER dimension is
// zero-valued on both sides.
func DiffNaming(from, to ConfigPayload) NamingDiff {
	fa, faft, frep, fmax, fmtl, fmodel := namingStrings(from)
	ta, taft, trep, tmax, tmtl, tmodel := namingStrings(to)
	return NamingDiff{
		AutoChanged: fa != ta,
		AutoFrom:    fa,
		AutoTo:      ta,

		AfterTurnsChanged: faft != taft,
		AfterTurnsFrom:    faft,
		AfterTurnsTo:      taft,

		RepeatEveryChanged: frep != trep,
		RepeatEveryFrom:    frep,
		RepeatEveryTo:      trep,

		MaxRepetitionsChanged: fmax != tmax,
		MaxRepetitionsFrom:    fmax,
		MaxRepetitionsTo:      tmax,

		MaxTitleLenChanged: fmtl != tmtl,
		MaxTitleLenFrom:    fmtl,
		MaxTitleLenTo:      tmtl,

		ModelChanged: fmodel != tmodel,
		ModelFrom:    fmodel,
		ModelTo:      tmodel,
	}
}

// namingStrings renders a payload's auto-naming section to its canonical
// display values (auto, after_turns, repeat_every, max_repetitions,
// max_title_len, model); an ABSENT section renders ("", "", "", "", "", "").
// Auto is the tri-state "" / "false" / "true" — a PRESENT section always
// renders "false" or "true", so section presence itself is diffable (the
// bare `{auto: false}` opt-out registers against an absent section). A
// numeric zero renders "" so a set→unset transition registers as a change.
func namingStrings(p ConfigPayload) (auto, afterTurns, repeatEvery, maxReps, maxTitleLen, model string) {
	n, ok := p.NamingView()
	if !ok {
		return "", "", "", "", "", ""
	}
	itoaOrEmpty := func(v int) string {
		if v == 0 {
			return ""
		}
		return strconv.Itoa(v)
	}
	return strconv.FormatBool(n.Auto), itoaOrEmpty(n.AfterTurns), itoaOrEmpty(n.RepeatEvery),
		itoaOrEmpty(n.MaxRepetitions), itoaOrEmpty(n.MaxTitleLen), n.Model
}

// LoadingModeChange is one entry in a loading-mode override diff: the
// override at Key (a catalog name for a ToolLoadingChanges entry, a
// ToolSourceID for a ServerLoadingChanges entry) changed from From to To.
// An empty From/To means the override was absent (unset) on that side of
// the compare — a set→unset or unset→set transition still registers as a
// change.
type LoadingModeChange struct {
	Key  string
	From string
	To   string
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
	// ServerLoadingChanges / ToolLoadingChanges are the structured deltas of
	// the per-server / per-tool loading-mode override maps. Sorted by Key.
	ServerLoadingChanges []LoadingModeChange
	ToolLoadingChanges   []LoadingModeChange
}

// Changed reports whether the tool exposure differs between the two
// revisions.
func (d ToolExposureDiff) Changed() bool {
	return len(d.PausedAdded) > 0 || len(d.PausedResumed) > 0 ||
		len(d.DisabledAdded) > 0 || len(d.DisabledEnabled) > 0 ||
		len(d.ServerLoadingChanges) > 0 || len(d.ToolLoadingChanges) > 0
}

// DiffToolExposure computes the structured set-diff of two tool-exposure
// states. Exported so the diff is one canonical implementation shared by
// the driver and tests.
func DiffToolExposure(from, to ConfigPayload) ToolExposureDiff {
	pAdded, pResumed := setDiff(from.PausedServers(), to.PausedServers())
	dAdded, dEnabled := setDiff(from.DisabledTools(), to.DisabledTools())
	return ToolExposureDiff{
		PausedAdded:          pAdded,
		PausedResumed:        pResumed,
		DisabledAdded:        dAdded,
		DisabledEnabled:      dEnabled,
		ServerLoadingChanges: diffLoadingModes(from.ServerLoadingModes(), to.ServerLoadingModes()),
		ToolLoadingChanges:   diffLoadingModes(from.ToolLoadingModes(), to.ToolLoadingModes()),
	}
}

// diffLoadingModes computes the structured per-key delta of two loading-mode
// override maps: every key present in either map whose value differs (an
// absent key compares as "") registers as one LoadingModeChange. Sorted by
// Key for a deterministic result.
func diffLoadingModes(from, to map[string]string) []LoadingModeChange {
	keys := make(map[string]struct{}, len(from)+len(to))
	for k := range from {
		keys[k] = struct{}{}
	}
	for k := range to {
		keys[k] = struct{}{}
	}
	var out []LoadingModeChange
	for k := range keys {
		f, t := from[k], to[k]
		if f != t {
			out = append(out, LoadingModeChange{Key: k, From: f, To: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
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

// OAuthProvidersDiff is the structured set-diff of the Protocol-installed
// OAuth-provider descriptors across two revisions, by name. Deterministic:
// every slice is sorted.
type OAuthProvidersDiff struct {
	// Added are the provider names present in the to-revision but not the
	// from-revision.
	Added []string
	// Removed are the provider names present in the from-revision but not the
	// to-revision.
	Removed []string
}

// Changed reports whether the installed-provider set differs between the two
// revisions.
func (d OAuthProvidersDiff) Changed() bool { return len(d.Added) > 0 || len(d.Removed) > 0 }

// DiffOAuthProviders computes the structured set-diff (by name) of two
// installed-provider states. Exported so the diff is one canonical
// implementation shared by the driver and tests.
func DiffOAuthProviders(from, to ConfigPayload) OAuthProvidersDiff {
	added, removed := setDiff(oauthProviderNames(from.OAuthProviderDescriptors()), oauthProviderNames(to.OAuthProviderDescriptors()))
	return OAuthProvidersDiff{Added: added, Removed: removed}
}

// oauthProviderNames projects a descriptor slice onto its name set.
func oauthProviderNames(in []OAuthProviderDescriptor) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, d := range in {
		out = append(out, d.Name)
	}
	return out
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
