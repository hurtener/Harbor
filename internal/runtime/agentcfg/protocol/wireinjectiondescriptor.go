package protocol

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// wireinjectiondescriptor.go — the DEV-GATED per-user credential-INJECTION
// mapping carried over `agent_config.add_mcp_connection` for a receiver-style
// MCP server.
//
// # The gate, mirroring the wire-OAuth descriptor's posture
//
// A runtime-added connection MAY carry an `injection` object that NAMES a
// boot-declared `tools.oauth_providers[]` broker and declares WHERE the per-user
// credential is placed on the outbound request (a header / an
// `Authorization: Basic` value / a `_meta` key). The mapping is NON-SECRET — the
// credential is still pulled per-user from the named broker at call time; nothing
// secret rides the wire. But wiring a credential to a receiver at runtime is an
// admin-writable field that determines credential delivery, so it is accepted
// ONLY behind the fail-closed, boot-only `tools.allow_wire_injection` opt-in
// (config flag OR `HARBOR_ALLOW_WIRE_INJECTION` boot env, default off). With the
// opt-in OFF (all of production) a connection carrying ANY injection field is
// REJECTED, fail-loud, naming the opt-in key. This gate is INDEPENDENT of the
// wire-OAuth-descriptor opt-in — an operator may enable one without the other.
//
// # The credential-plane invariant stays honest even when opted in
//
//   - The injection `provider` NAMES a boot-declared broker; no credential source
//     or secret rides the wire.
//   - The connection host is validated against the named broker's boot-declared
//     allow-list — the reachable sink is DERIVED from `connection.url`, never a
//     wire-supplied host list (there is no host field on this descriptor). That
//     derivation + allow-list check happens at attach time in the ONE injection
//     engine (`resolveInjectionBinding`), shared with the boot path.
//   - Every declared target key must be redaction-covered: validation here
//     rejects a header / `_meta` leaf the audit redactor would not hold to `***`,
//     using the SAME `config.IsReceiverInjectionCredentialKey` predicate the
//     redactor and boot validation consult — so a wire-declared key an operator
//     may set is exactly a key the redactor redacts (fail-closed).

// ErrWireInjectionNotAllowed — a connection descriptor carries a per-user
// credential-injection mapping (`injection`), which is accepted only behind the
// fail-closed `tools.allow_wire_injection` opt-in, but the opt-in is off. The
// wire handler maps it onto a canonical Protocol Code + 400; in-process callers
// compare with errors.Is.
var ErrWireInjectionNotAllowed = errors.New("agentcfg/protocol: wire-carried credential-injection mapping is not allowed (the fail-closed tools.allow_wire_injection opt-in is off)")

// maxInjectionMetaKeyDepth caps the dot-segment count of a `meta_key` so the
// injected credential always nests ABOVE the audit redactor's deep-walk cap
// (`audit.MaxDepth` = 64). The redactor stops walking at that depth, so a
// credential nested at or beyond it would sit BELOW the redactor's walk and
// could ride an audit payload uncredacted. 16 leaves comfortable headroom for
// the `_meta` base depth plus any wrapper the payload adds around the injected
// map — far below 64 — while never constraining a real receiver key (which
// nests one or two segments, e.g. `vendor.api_key`).
const maxInjectionMetaKeyDepth = 16

// gateAndValidateInjectionDescriptor is the single entry point the add-connection
// handler uses for a wire injection mapping. It (1) applies the fail-closed
// opt-in gate — a connection carrying an injection mapping with the opt-in off is
// rejected loud — then (2) validates + normalises the mapping shape (form,
// target-key redaction coverage, reserved-segment rules), returning the domain
// descriptor to persist alongside the connection revision. The broker-name
// resolution + downstream-sink derivation are enforced at attach time in the
// shared injection engine (the authority with the real provider registry); this
// layer gates the opt-in and validates the shape so a malformed mapping fails
// before any dial. A nil input returns (nil, nil) — no injection binding.
func (s *Service) gateAndValidateInjectionDescriptor(in *prototypes.AgentConfigMCPCredentialInjectionDescriptor) (*agentcfg.MCPCredentialInjectionDescriptor, error) {
	if in == nil {
		return nil, nil
	}
	if !s.allowWireInjection {
		return nil, fmt.Errorf(
			"%w: connection carries a credential-injection mapping (injection), which is accepted only behind the fail-closed dev opt-in tools.allow_wire_injection (or the HARBOR_ALLOW_WIRE_INJECTION boot env)",
			ErrWireInjectionNotAllowed)
	}
	return validateWireInjectionDescriptor(in)
}

// gateAndValidateConnectionsInjection applies the SAME fail-closed opt-in gate +
// shape validation the add_mcp_connection path enforces to EVERY connection
// descriptor in a full-payload set (`agent_config.set_revision`). set_revision is
// a SECOND persistence door: without this, an admin set_revision carrying a
// `connections.servers[].injection` mapping would persist it un-gated and
// un-validated — falsifying the fail-closed invariant (opt-in off ⇒ rejected,
// nothing persisted) and skipping every shape check the add path enforces
// (Authorization-lane / non-redaction-covered key / reserved `_meta` segment /
// meta-depth / stdio / bearer mutual-exclusivity). It runs BEFORE any registry
// write and rejects the WHOLE set loud on the first offending connection, so a
// malformed or gated mapping never reaches a persisted revision. A nil section or
// a set with no injection mappings is a no-op (the byte-for-byte prior posture).
func (s *Service) gateAndValidateConnectionsInjection(conns *prototypes.AgentConfigConnections) error {
	if conns == nil {
		return nil
	}
	for i := range conns.Servers {
		c := conns.Servers[i]
		if c.Injection == nil {
			continue
		}
		// One auth mode per connection — injection is mutually exclusive with the
		// bearer bindings (mirrors the add path's validateConnection).
		if strings.TrimSpace(c.OAuthProvider) != "" {
			return fmt.Errorf("%w: connection %q: injection is mutually exclusive with oauth_provider (one auth mode per connection)", ErrInvalidConnection, c.Name)
		}
		if c.OAuth != nil {
			return fmt.Errorf("%w: connection %q: injection is mutually exclusive with oauth (inline binding) (one auth mode per connection)", ErrInvalidConnection, c.Name)
		}
		// A receiver-style server is reached over HTTP — stdio carries no request
		// to inject into (mirrors the add path's stdio rejection).
		if agentcfg.MCPTransport(strings.TrimSpace(c.Transport)) == agentcfg.MCPTransportStdio {
			return fmt.Errorf("%w: connection %q: stdio transport must not carry an injection mapping (a receiver-style server is reached over HTTP; the pulled credential is delivered on the outbound request)", ErrInvalidConnection, c.Name)
		}
		// The fail-closed opt-in gate + shape validation — the SAME entry point the
		// add path uses (one gate applied at both doors). The `%w` preserves the
		// sentinel so errors.Is (and the wire handler's 400 mapping) still fire.
		if _, err := s.gateAndValidateInjectionDescriptor(c.Injection); err != nil {
			return fmt.Errorf("connection %q: %w", c.Name, err)
		}
	}
	return nil
}

// validateWireInjectionDescriptor validates the wire injection mapping and
// returns the normalised domain descriptor. It enforces: a non-empty broker
// name; a known form (header / basic / meta); and — per form — a
// redaction-covered, non-Authorization header, or a redaction-covered leaf with
// no reserved `_meta` segment. It reuses the SAME predicates the boot-time
// validation and the audit redactor consult (`config.IsReceiverInjectionCredentialKey`
// / `config.IsReservedMCPMetaKey`) so a wire-declared key that passes here is
// exactly a key the redactor holds to `***`. It deliberately does NOT resolve the
// provider against a registry or derive the downstream sink — that is the attach
// engine's authority (the real provider set) and re-enforced there.
func validateWireInjectionDescriptor(in *prototypes.AgentConfigMCPCredentialInjectionDescriptor) (*agentcfg.MCPCredentialInjectionDescriptor, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return nil, fmt.Errorf("%w: injection.provider must name a declared tools.oauth_providers[] broker", ErrInvalidConnection)
	}
	out := &agentcfg.MCPCredentialInjectionDescriptor{Provider: provider}
	switch in.Form {
	case config.MCPInjectionFormHeader:
		header := strings.TrimSpace(in.Header)
		if header == "" {
			return nil, fmt.Errorf("%w: injection.header must name a target request header for form=header", ErrInvalidConnection)
		}
		if strings.EqualFold(header, "authorization") {
			return nil, fmt.Errorf("%w: injection.header must not target the Authorization header (use form=basic for an Authorization: Basic value)", ErrInvalidConnection)
		}
		if !config.IsReceiverInjectionCredentialKey(header) {
			return nil, fmt.Errorf("%w: injection.header %q is not a redaction-covered credential key — the audit redactor could not hold the injected value to *** (name it with a credential segment such as -api-key / -token / -secret)", ErrInvalidConnection, header)
		}
		out.Form = config.MCPInjectionFormHeader
		out.Header = header
	case config.MCPInjectionFormBasic:
		// Authorization: Basic — the redactor already covers the Authorization
		// lane + the Basic scheme; BasicUsername is non-secret and optional.
		out.Form = config.MCPInjectionFormBasic
		out.BasicUsername = in.BasicUsername
	case config.MCPInjectionFormMeta:
		metaKey := strings.TrimSpace(in.MetaKey)
		if metaKey == "" {
			return nil, fmt.Errorf("%w: injection.meta_key must name a target _meta key path for form=meta", ErrInvalidConnection)
		}
		segs := strings.Split(metaKey, ".")
		if len(segs) > maxInjectionMetaKeyDepth {
			return nil, fmt.Errorf("%w: injection.meta_key has %d path segments, exceeding the cap of %d — a credential nested that deep can outrun the audit redactor's deep-walk (audit.MaxDepth) and ride an audit payload uncredacted", ErrInvalidConnection, len(segs), maxInjectionMetaKeyDepth)
		}
		for _, seg := range segs {
			if strings.TrimSpace(seg) == "" {
				return nil, fmt.Errorf("%w: injection.meta_key must not contain an empty path segment", ErrInvalidConnection)
			}
			if config.IsReservedMCPMetaKey(seg) {
				return nil, fmt.Errorf("%w: injection.meta_key segment %q is reserved (tenant/user/session/agent_id/traceparent/tracestate and io.modelcontextprotocol/-prefixed keys are runtime-stamped)", ErrInvalidConnection, seg)
			}
		}
		if leaf := segs[len(segs)-1]; !config.IsReceiverInjectionCredentialKey(leaf) {
			return nil, fmt.Errorf("%w: injection.meta_key leaf %q is not a redaction-covered credential key — the audit redactor could not hold the injected value to *** (name the leaf with a credential segment such as api_key / token / secret)", ErrInvalidConnection, leaf)
		}
		out.Form = config.MCPInjectionFormMeta
		out.MetaKey = metaKey
	case "":
		return nil, fmt.Errorf("%w: injection.form must be set (header / basic / meta)", ErrInvalidConnection)
	default:
		return nil, fmt.Errorf("%w: injection.form %q is invalid (header / basic / meta)", ErrInvalidConnection, in.Form)
	}
	return out, nil
}
