package protocol

import (
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// wireoauthdescriptor.go — the DEV-GATED full-binding OAuth-provider descriptor
// (carried over `agent_config.set_oauth_provider` / `add_mcp_connection`).
//
// # The two shapes, one gate
//
// A provider descriptor is EITHER the name-only shape (the default and all of
// production: `{name, driver, credential_source, credential_broker, scopes?}`) OR
// the dev-gated wire shape, which adds the NEW server's OAuth params
// (`{..., credential_broker, token_url, audience?, scopes?}`) while STILL naming
// a boot-declared `credential_broker`. The wire-carried fields (`token_url`,
// `audience`) are accepted ONLY behind the fail-closed
// `tools.allow_wire_oauth_descriptor` opt-in (config flag OR
// `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` boot env). With the opt-in OFF a
// descriptor carrying either wire field is REJECTED, fail-loud, naming the field
// + the opt-in key — the name-only posture is byte-for-byte unchanged.
//
// # The runtime's credential custody stays boot-declared
//
// The wire descriptor carries the NEW SERVER's public token endpoint only. The
// runtime's OWN credential custody — the coordinator credential-pull endpoint,
// the service-token env-var name, the org client credential — lives on the named
// `credential_broker` (a `tools.oauth_credential_brokers[]` boot entry) and NEVER
// rides the wire. A credential-source URL / env-var name (`remote`, `client_id_env`,
// `client_secret_env`, `auth_url`) has no field on the wire struct, so a
// `DisallowUnknownFields` decode rejects any of them BY NAME.
//
// # Why this is an explicit reject, not a decode reject
//
// Before the wire fields existed on the struct, a `DisallowUnknownFields` decode
// rejected them BY NAME. Now that `token_url` / `audience` ARE known fields, that
// decode no longer rejects them — so the default-off reject is an EXPLICIT
// validation here. It is fail-closed: a wire-bearing descriptor with the opt-in
// off returns an error, never a silent downgrade to the name-only shape.
//
// # The derived downstream sink (never a wire field)
//
// A wire descriptor carries NO `allowed_downstream_hosts` field — the credential
// sink is DERIVED by the runtime from the bound connection's own URL
// (`NormalizeDownstreamHost(connection.url)`), so an exchanged token can only ever
// be presented to the one endpoint the connection actually dials. A
// wire-supplied host list has no field to land in (a `DisallowUnknownFields`
// decode rejects one by name).

// oauthSinkFieldName returns the name of the first wire-carried field set on the
// descriptor, and whether any is set. The name-only shape sets none. (A raw
// credential-source URL / env-var name — the `remote` block — is NOT on the wire
// struct at all, so it can never be carried: the runtime's own credential
// custody stays 100% boot-declared on the named credential_broker.)
func oauthSinkFieldName(p prototypes.AgentConfigOAuthProviderDescriptor) (string, bool) {
	switch {
	case strings.TrimSpace(p.TokenURL) != "":
		return "token_url", true
	case strings.TrimSpace(p.Audience) != "":
		return "audience", true
	default:
		return "", false
	}
}

// gateAndValidateOAuthProviderDescriptor is the single entry point both provider
// verbs use. It (1) applies the fail-closed wire opt-in gate — a sink-bearing
// descriptor with the opt-in off is rejected loud — then (2) validates the
// descriptor in whichever shape it is (name-only or dev-gated wire), returning
// the normalised domain descriptor. The returned wire descriptor's derived
// downstream sink is left EMPTY here (there is no bound connection at this
// layer); a connection binding derives + fills it.
func (s *Service) gateAndValidateOAuthProviderDescriptor(p prototypes.AgentConfigOAuthProviderDescriptor) (agentcfg.OAuthProviderDescriptor, error) {
	field, isWire := oauthSinkFieldName(p)
	if isWire && !s.allowWireOAuthDescriptor {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf(
			"%w: descriptor carries credential-sink field %q, which is accepted only behind the fail-closed dev opt-in tools.allow_wire_oauth_descriptor (or the HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR boot env); the default zero-URL name-only binding carries no sink field",
			ErrWireDescriptorNotAllowed, field)
	}
	if isWire {
		return validateWireOAuthProviderDescriptor(p)
	}
	return validateOAuthProviderDescriptor(p)
}

// validateWireOAuthProviderDescriptor validates the DEV-GATED wire shape and
// returns the domain descriptor carrying the wire fields (the derived downstream
// sink is filled by a connection binding, not here). It enforces: a non-empty
// name; driver EXACTLY "tokenexchange"; credential_source EXACTLY "remote"; a
// non-empty credential_broker (the wire descriptor still NAMES a boot broker —
// the runtime's own credential custody stays 100% boot-declared, no
// credential-source URL or secret ever rides the wire); and a non-empty token_url
// (the NEW server's exchange endpoint). Scopes + audience are carried verbatim
// (scopes clamped at build time). There is deliberately no `remote` field on the
// wire struct to validate — a credential-source URL / env-var name cannot be
// wire-carried.
func validateWireOAuthProviderDescriptor(p prototypes.AgentConfigOAuthProviderDescriptor) (agentcfg.OAuthProviderDescriptor, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: name is empty", ErrInvalidProvider)
	}
	if strings.TrimSpace(p.Driver) != providerWritableDriver {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: driver %q not installable — only %q (the non-interactive broker-pull exchange) is Protocol-installable", ErrInvalidProvider, p.Driver, providerWritableDriver)
	}
	source := strings.TrimSpace(p.CredentialSource)
	if source == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_source is empty — a wire descriptor must declare credential_source %q", ErrInvalidProvider, providerWritableSource)
	}
	if source != providerWritableSource {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_source %q not installable — only %q (broker-pull) is Protocol-installable", ErrInvalidProvider, p.CredentialSource, providerWritableSource)
	}
	broker := strings.TrimSpace(p.CredentialBroker)
	if broker == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_broker is empty — a wire descriptor still NAMES a boot-declared broker for the runtime's own credential custody (no credential-source URL or secret rides the wire)", ErrInvalidProvider)
	}
	tokenURL := strings.TrimSpace(p.TokenURL)
	if tokenURL == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: token_url is empty — a wire descriptor must declare the NEW server's RFC-8693 token-exchange endpoint", ErrInvalidProvider)
	}
	scopes := make([]string, 0, len(p.Scopes))
	for _, sc := range p.Scopes {
		if s := strings.TrimSpace(sc); s != "" {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		scopes = nil
	}
	return agentcfg.OAuthProviderDescriptor{
		Name:             name,
		Driver:           providerWritableDriver,
		CredentialSource: providerWritableSource,
		CredentialBroker: broker,
		Scopes:           scopes,
		TokenURL:         tokenURL,
		Audience:         strings.TrimSpace(p.Audience),
	}, nil
}
