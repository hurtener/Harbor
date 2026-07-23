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
// A provider descriptor is EITHER the zero-URL name-only shape (the default and
// all of production: `{name, driver, credential_source, credential_broker,
// scopes?}`) OR the dev-gated full-binding shape (`{name, driver,
// credential_source, token_url, audience?, scopes?, remote{}}`). The
// credential-sink fields (`token_url`, `audience`, `remote`) are accepted ONLY
// behind the fail-closed `tools.allow_wire_oauth_descriptor` opt-in (config flag
// OR `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` boot env). With the opt-in OFF a
// descriptor carrying ANY sink field is REJECTED, fail-loud, naming the field +
// the opt-in key — the zero-URL name-only posture is byte-for-byte unchanged.
//
// # Why this is an explicit reject, not a decode reject
//
// Before the wire fields existed on the struct, a `DisallowUnknownFields` decode
// rejected them BY NAME. Now that `token_url` / `audience` / `remote` ARE known
// fields, that decode no longer rejects them — so the default-off reject is an
// EXPLICIT validation here. It is fail-closed: a sink-bearing descriptor with the
// opt-in off returns an error, never a silent downgrade to the name-only shape.
//
// # The derived downstream sink (never a wire field)
//
// A wire descriptor carries NO `allowed_downstream_hosts` field — the credential
// sink is DERIVED by the runtime from the bound connection's own URL
// (`NormalizeDownstreamHost(connection.url)`), so an exchanged token can only ever
// be presented to the one endpoint the connection actually dials. A
// wire-supplied host list has no field to land in (a `DisallowUnknownFields`
// decode rejects one by name).

// oauthSinkFieldName returns the name of the first credential-sink field set on
// the descriptor, and whether any is set. The zero-URL name-only shape sets
// none.
func oauthSinkFieldName(p prototypes.AgentConfigOAuthProviderDescriptor) (string, bool) {
	switch {
	case strings.TrimSpace(p.TokenURL) != "":
		return "token_url", true
	case strings.TrimSpace(p.Audience) != "":
		return "audience", true
	case p.Remote != nil:
		return "remote", true
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

// validateWireOAuthProviderDescriptor validates the DEV-GATED full-binding shape
// and returns the domain descriptor carrying the wire fields (the derived
// downstream sink is filled by a connection binding, not here). It enforces: a
// non-empty name; driver EXACTLY "tokenexchange"; credential_source EXACTLY
// "remote"; NO credential_broker (mutually exclusive with the wire fields — a
// wire descriptor pins its own sinks, a name-only descriptor pins a boot broker);
// a non-empty token_url; and a complete remote block (url + auth_token_env — no
// secret, an env-var NAME). Scopes are carried verbatim (clamped at build time).
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
	if strings.TrimSpace(p.CredentialBroker) != "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_broker is mutually exclusive with the wire fields (token_url/remote) — a wire descriptor pins its own token endpoint + credential pull, a name-only descriptor references a boot broker", ErrInvalidProvider)
	}
	tokenURL := strings.TrimSpace(p.TokenURL)
	if tokenURL == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: token_url is empty — a wire descriptor must declare its RFC-8693 token-exchange endpoint", ErrInvalidProvider)
	}
	if p.Remote == nil {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: remote{} is required — a wire descriptor pulls its org client credential from a coordinator endpoint (url + auth_token_env, no secret on the wire)", ErrInvalidProvider)
	}
	remoteURL := strings.TrimSpace(p.Remote.URL)
	if remoteURL == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: remote.url is empty — the credential-pull endpoint is required", ErrInvalidProvider)
	}
	authTokenEnv := strings.TrimSpace(p.Remote.AuthTokenEnv)
	if authTokenEnv == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: remote.auth_token_env is empty — a wire descriptor names (never carries) the env var holding the runtime service token", ErrInvalidProvider)
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
		Scopes:           scopes,
		TokenURL:         tokenURL,
		Audience:         strings.TrimSpace(p.Audience),
		Remote:           &agentcfg.OAuthRemoteDescriptor{URL: remoteURL, AuthTokenEnv: authTokenEnv},
	}, nil
}
