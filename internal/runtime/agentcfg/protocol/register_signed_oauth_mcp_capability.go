package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// RegisterOAuthMCPCapability is D-401's bounded production registration
// operation. The provider is prepared privately and handed directly to MCP
// preparation; it is never installed in the generic ProviderSet.
func (s *Service) RegisterOAuthMCPCapability(ctx context.Context, req prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest) (prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	if s.preparer == nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	if s.signedOAuthMCPOperations == nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	providerPreparer, ok := s.providerInstaller.(SignedCapabilityProviderPreparer)
	if !ok || providerPreparer == nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	canonicalURL, sink, err := agentcfg.CanonicalOAuthMCPURL(req.Connection.URL)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: %w", ErrInvalidSignedCapabilityDescriptor, err)
	}
	connection, err := normalizeSignedCapabilityConnection(req.Connection, canonicalURL)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	scopes, err := agentcfg.CanonicalScopes(req.Scopes)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	providerName := strings.TrimSpace(req.ProviderName)
	broker := strings.TrimSpace(req.Broker)
	audience := strings.TrimSpace(req.Audience)
	if providerName == "" || broker == "" || audience == "" {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: provider_name, broker, and audience are required", ErrInvalidSignedCapabilityDescriptor)
	}
	authority, ok := s.signedOAuthMCPCapabilityAuthorities[broker]
	if !ok {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: broker %q has no enabled signed-capability trust anchor", ErrSignedCapabilityUnavailable, broker)
	}
	// The immutable capability revision is signed rather than caller-authored.
	// It is read only to build the exact expected binding and is then verified
	// along with the signature by the boot-pinned trust anchor.
	var unsafe agentcfg.SignedOAuthMCPAuthorityClaims
	if _, _, err := jwt.NewParser().ParseUnverified(req.AuthorityEnvelope, &unsafe); err != nil || strings.TrimSpace(unsafe.CapabilityRevision) == "" {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: malformed authority envelope", agentcfg.ErrSignedCapabilityAuthority)
	}
	binding := agentcfg.SignedOAuthMCPBinding{
		TenantID: id.TenantID, AgentID: req.AgentID, Broker: broker,
		ProviderName: providerName, CapabilityRevision: unsafe.CapabilityRevision,
		URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), Audience: audience, Scopes: scopes,
	}
	claims, err := authority.Verify(req.AuthorityEnvelope, s.now().UTC(), binding)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	kid, err := signedCapabilityEnvelopeKID(req.AuthorityEnvelope)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}

	operationKey := agentcfg.SignedOAuthMCPReplayKey{
		TenantID: id.TenantID, TrustAnchorName: broker, Issuer: claims.Issuer,
		KeyID: kid, JTI: claims.ID,
	}
	op, _, err := s.signedOAuthMCPOperations.Claim(ctx, operationKey, binding, claims.ExpiresAt.Time)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}

	defer s.lockAgent(id.TenantID, req.AgentID)()
	q := identity.Quadruple{Identity: id}
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	if !op.ExpiresAt.After(s.now().UTC()) && (op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed || op.Phase == agentcfg.SignedOAuthMCPPhaseRevisionCommitted) {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: incomplete authority operation has expired", agentcfg.ErrSignedCapabilityAuthority)
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if hasActive && signedCapabilityPairMatches(active.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
			return signedCapabilityResponse(active, providerName, connection.Name), nil
		}
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: published operation does not match active pair", agentcfg.ErrSignedCapabilityReplay)
	}
	if op.Phase != agentcfg.SignedOAuthMCPPhaseClaimed && op.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: operation is terminal in phase %q", agentcfg.ErrSignedCapabilityReplay, op.Phase)
	}
	if hasActive && active.Payload.SignedOAuthMCPPair != nil && !signedCapabilityPairMatches(active.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityPairExists
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed && hasActive && signedCapabilityPairMatches(active.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, active.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
		}
	}
	preparedProvider, err := providerPreparer.PrepareSignedCapabilityProvider(ctx, id.TenantID, req.AgentID, providerName, broker, audience, sink, scopes)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	preparedConnection, err := s.preparer.PrepareConnection(ctx, AttachRequest{
		Identity: id, AgentID: req.AgentID, Name: connection.Name, Transport: agentcfg.MCPTransportHTTP,
		URL: canonicalURL, OAuthProvider: providerName, OAuthProviderOverride: preparedProvider.Binding(),
	})
	if err != nil {
		_ = preparedProvider.Close(context.WithoutCancel(ctx))
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	closePrepared := func() {
		cleanup := context.WithoutCancel(ctx)
		_ = preparedConnection.Close(cleanup)
		_ = preparedProvider.Close(cleanup)
	}
	pair := &agentcfg.SignedOAuthMCPPair{
		ProviderName: providerName, Broker: broker, Audience: audience, Scopes: scopes,
		CapabilityRevision: claims.CapabilityRevision, URLDigest: binding.URLDigest, Sink: sink,
		Connection:      agentcfg.SignedOAuthMCPConnectionDescriptor{Name: connection.Name, URL: canonicalURL, ToolAllowlist: connection.ToolAllowlist, ToolDenylist: connection.ToolDenylist, ConnectTimeoutMS: connection.ConnectTimeoutMS, RequestTimeoutMS: connection.RequestTimeoutMS},
		AuthorityIssuer: claims.Issuer, AuthorityKeyID: kid, AuthorityJTIHash: signedCapabilityJTIHash(claims.ID),
	}
	var rev agentcfg.Revision
	if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed {
		payload := carrySiblingsForward(active, hasActive)
		payload.SignedOAuthMCPPair = pair
		rev, err = s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
		if err != nil {
			// A StateStore acknowledgement can be lost after the pointer commits.
			// Exact immutable pair equality, never a same-name heuristic, is the
			// recovery proof that allows this operation to continue.
			active, hasActive, readErr := s.registry.Active(context.WithoutCancel(ctx), q, req.AgentID, agentcfg.ConfigScopeAgent)
			if readErr != nil || !hasActive || !signedCapabilityPairMatches(active.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
				closePrepared()
				return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
			}
			rev = active
		}
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, rev.RevisionID)
		if err != nil {
			closePrepared()
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
		}
	} else {
		if !hasActive || !signedCapabilityPairMatches(active.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
			closePrepared()
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: revision-committed operation has no matching desired pair", agentcfg.ErrSignedCapabilityReplay)
		}
		rev = active
	}
	if err := preparedConnection.Activate(ctx); err != nil {
		closePrepared()
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	// No ProviderSet publication occurs. Commit records that the private
	// provider's ownership has moved to the activated MCP connection.
	preparedProvider.Commit(ctx)
	if _, err := s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePublished, rev.RevisionID); err != nil {
		// Data-plane publication is already visible. Returning the checkpoint
		// error preserves recovery: the exact JTI resumes by proving the active
		// pair and only writes the missing durable phase.
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	return signedCapabilityResponse(rev, providerName, connection.Name), nil
}

func signedCapabilityResponse(rev agentcfg.Revision, providerName, connectionName string) prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse {
	return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{Revision: revisionToWire(rev), ProviderName: providerName, ConnectionName: connectionName, ProtocolVersion: prototypes.ProtocolVersion}
}

func signedCapabilityPairMatches(pair *agentcfg.SignedOAuthMCPPair, tenant string, binding agentcfg.SignedOAuthMCPBinding) bool {
	if pair == nil {
		return false
	}
	return agentcfg.SignedOAuthMCPPairFingerprint(agentcfg.SignedOAuthMCPBinding{
		TenantID: tenant, AgentID: binding.AgentID, Broker: pair.Broker,
		ProviderName: pair.ProviderName, CapabilityRevision: pair.CapabilityRevision,
		URLDigest: pair.URLDigest, Audience: pair.Audience, Scopes: pair.Scopes,
	}) == agentcfg.SignedOAuthMCPPairFingerprint(binding)
}

func normalizeSignedCapabilityConnection(in prototypes.SignedOAuthMCPConnectionDescriptor, canonicalURL string) (prototypes.SignedOAuthMCPConnectionDescriptor, error) {
	out := in
	out.Name = strings.TrimSpace(out.Name)
	out.URL = canonicalURL
	if out.Name == "" || out.ConnectTimeoutMS < 0 || out.RequestTimeoutMS < 0 {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: name is required and timeouts must be non-negative", ErrInvalidSignedCapabilityDescriptor)
	}
	var err error
	if out.ToolAllowlist, err = agentcfg.CanonicalScopes(out.ToolAllowlist); err != nil {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: tool_allowlist: %v", ErrInvalidSignedCapabilityDescriptor, err)
	}
	if out.ToolDenylist, err = agentcfg.CanonicalScopes(out.ToolDenylist); err != nil {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: tool_denylist: %v", ErrInvalidSignedCapabilityDescriptor, err)
	}
	return out, nil
}

func signedCapabilityEnvelopeKID(raw string) (string, error) {
	token, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil || token == nil {
		return "", fmt.Errorf("%w: malformed authority envelope", agentcfg.ErrSignedCapabilityAuthority)
	}
	kid, _ := token.Header["kid"].(string)
	if strings.TrimSpace(kid) == "" {
		return "", fmt.Errorf("%w: authority envelope has no key id", agentcfg.ErrSignedCapabilityAuthority)
	}
	return kid, nil
}

func signedCapabilityJTIHash(jti string) string {
	sum := sha256.Sum256([]byte(jti))
	return hex.EncodeToString(sum[:])
}
