package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// RegisterOAuthMCPCapability is the bounded production registration
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
	if s.signedOAuthMCPOperations == nil || s.signedOAuthMCPFences == nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityUnavailable
	}
	physical, ok := s.registry.(physicalActiveRegistry)
	if !ok || physical == nil {
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
	domainConnection := agentcfg.SignedOAuthMCPConnectionDescriptor{
		Name: connection.Name, URL: canonicalURL, ToolAllowlist: connection.ToolAllowlist,
		ToolDenylist: connection.ToolDenylist, ConnectTimeoutMS: connection.ConnectTimeoutMS,
		RequestTimeoutMS: connection.RequestTimeoutMS,
	}
	binding := agentcfg.SignedOAuthMCPBinding{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, AgentID: req.AgentID, Broker: broker,
		ProviderName: providerName, CapabilityRevision: unsafe.CapabilityRevision,
		URLDigest: agentcfg.OAuthMCPURLDigest(canonicalURL), SinkDigest: agentcfg.OAuthMCPURLDigest(sink),
		Audience: audience, Scopes: scopes, Connection: domainConnection,
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
	operationKind, err := s.signedOAuthMCPOperations.Kind(operationKey)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}

	defer s.lockAgent(id.TenantID, req.AgentID)()
	// Claim precedes this process-local lock so independent runtimes compete at
	// the durable StateStore boundary. Refresh after waiting: a same-JTI caller
	// may already have advanced the exact receipt.
	op, err = s.signedOAuthMCPOperations.Load(ctx, operationKey)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if err := s.commitSignedOAuthMCPFence(ctx, id.TenantID, req.AgentID, operationKind, op); err != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
		}
	}
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	if !op.ExpiresAt.After(s.now().UTC()) && (op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed || op.Phase == agentcfg.SignedOAuthMCPPhaseRevisionCommitted) {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: incomplete authority operation has expired", agentcfg.ErrSignedCapabilityAuthority)
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhasePublished {
		if hasActive && signedCapabilityPairMatchesOperation(active.Payload.SignedOAuthMCPPair, id.TenantID, binding, operationKind) {
			return signedCapabilityResponse(active, providerName, connection.Name), nil
		}
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: published operation does not match active pair", agentcfg.ErrSignedCapabilityReplay)
	}
	if op.Phase != agentcfg.SignedOAuthMCPPhaseClaimed && op.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, fmt.Errorf("%w: operation is terminal in phase %q", agentcfg.ErrSignedCapabilityReplay, op.Phase)
	}
	if hasActive && active.Payload.SignedOAuthMCPPair != nil && !signedCapabilityPairMatchesOperation(active.Payload.SignedOAuthMCPPair, id.TenantID, binding, operationKind) {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, ErrSignedCapabilityPairExists
	}
	if err := s.rejectIncompletePriorSignedPairLifetime(ctx, id, req.AgentID, operationKind); err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed && hasActive && signedCapabilityPairMatchesOperation(active.Payload.SignedOAuthMCPPair, id.TenantID, binding, operationKind) {
		physicalRevision, physicalErr := requirePhysicalActiveRevision(ctx, physical, q, req.AgentID, active.RevisionID, active.ContentHash)
		if physicalErr != nil || !signedCapabilityPairMatches(physicalRevision.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(fmt.Errorf("%w: claimed operation does not match physical desired pair", agentcfg.ErrSignedCapabilityReplay), physicalErr)
		}
		active = physicalRevision
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, active.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
		}
	}
	exchangeBinding := signedCapabilityExchangeBinding(binding, sink)
	preparedProvider, err := providerPreparer.PrepareSignedCapabilityProvider(ctx, broker, exchangeBinding, scopes)
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	attachCtx := tools.WithInvokingAgent(ctx, req.AgentID)
	preparedConnection, err := s.preparer.PrepareConnection(attachCtx, AttachRequest{
		Identity: id, AgentID: req.AgentID, Name: connection.Name, Transport: agentcfg.MCPTransportHTTP,
		URL: canonicalURL, OAuthProvider: providerName, OAuthProviderOverride: preparedProvider.Binding(), OwnOAuthProvider: true,
		ToolAllowlist: connection.ToolAllowlist, ToolDenylist: connection.ToolDenylist,
		ConnectTimeoutMS: connection.ConnectTimeoutMS, RequestTimeoutMS: connection.RequestTimeoutMS,
		DescriptorFingerprint: signedCapabilityAttachmentFingerprint(domainConnection, operationKind),
	})
	if err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, closePreparedSignedCapability(ctx, nil, preparedProvider))
	}
	closePrepared := func() error {
		return closePreparedSignedCapability(ctx, preparedConnection, preparedProvider)
	}
	pair := &agentcfg.SignedOAuthMCPPair{
		ProviderName: providerName, Broker: broker, Audience: audience, Scopes: scopes,
		CapabilityRevision: claims.CapabilityRevision, URLDigest: binding.URLDigest, Sink: sink, SinkDigest: binding.SinkDigest,
		Connection:      domainConnection,
		AuthorityIssuer: claims.Issuer, AuthorityKeyID: kid, AuthorityJTIHash: signedCapabilityJTIHash(claims.ID),
		AuthorityOperationKind: operationKind, OwnerAgentID: req.AgentID, OwnerUserID: id.UserID, OwnerSessionID: id.SessionID,
	}
	var rev agentcfg.Revision
	if op.Phase == agentcfg.SignedOAuthMCPPhaseClaimed {
		payload := carrySiblingsForward(active, hasActive)
		payload.SignedOAuthMCPPair = pair
		normalized := agentcfg.NormalizePayload(payload)
		candidateHash, hashErr := agentcfg.ContentHash(normalized)
		if hashErr != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(hashErr, closePrepared())
		}
		priorRevisionID := ""
		if hasActive {
			priorRevisionID = active.RevisionID
		}
		if _, err := s.signedOAuthMCPFences.Begin(ctx, id.TenantID, req.AgentID, operationKind, op.Fingerprint, candidateHash, priorRevisionID); err != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, closePrepared())
		}
		fenceCtx := agentcfg.WithSignedOAuthMCPFenceOperation(ctx, operationKind)
		rev, err = s.registry.SetRevision(fenceCtx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
		if err != nil {
			// A StateStore acknowledgement can be lost after the pointer commits.
			// Exact immutable pair equality identifies the candidate, but immutable
			// history alone is not authority: compensation may have left an orphan.
			// The physical pointer must also name this exact revision.
			history, readErr := s.registry.ListRevisions(context.WithoutCancel(ctx), q, req.AgentID, agentcfg.ConfigScopeAgent, 0)
			for _, candidate := range history {
				if candidate.ContentHash == candidateHash && signedCapabilityPairMatches(candidate.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
					rev = candidate
					readErr = nil
					break
				}
			}
			if readErr != nil || rev.RevisionID == "" {
				return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, readErr, closePrepared())
			}
		}
		physicalRevision, physicalErr := requirePhysicalActiveRevision(context.WithoutCancel(ctx), physical, q, req.AgentID, rev.RevisionID, candidateHash)
		if physicalErr != nil || !signedCapabilityPairMatches(physicalRevision.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
			if physicalErr == nil {
				physicalErr = fmt.Errorf("%w: physical active candidate does not bind signed pair", agentcfg.ErrSignedCapabilityPending)
			}
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, physicalErr, closePrepared())
		}
		rev = physicalRevision
		op, err = s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, rev.RevisionID)
		if err != nil {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, closePrepared())
		}
	} else {
		physicalRevision, physicalErr := requirePhysicalActiveRevision(ctx, physical, q, req.AgentID, op.RevisionID, "")
		if physicalErr != nil || !signedCapabilityPairMatches(physicalRevision.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
			return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(fmt.Errorf("%w: revision-committed operation has no matching physical desired pair", agentcfg.ErrSignedCapabilityReplay), physicalErr, closePrepared())
		}
		rev = physicalRevision
	}
	physicalRevision, err := requirePhysicalActiveRevision(ctx, physical, q, req.AgentID, rev.RevisionID, rev.ContentHash)
	if err != nil || !signedCapabilityPairMatches(physicalRevision.Payload.SignedOAuthMCPPair, id.TenantID, binding) {
		if err == nil {
			err = fmt.Errorf("%w: physical active candidate changed before activation", agentcfg.ErrSignedCapabilityPending)
		}
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, closePrepared())
	}
	rev = physicalRevision
	if err := preparedConnection.Activate(ctx); err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, errors.Join(err, closePrepared())
	}
	// No ProviderSet publication occurs. Commit records that the private
	// provider's ownership has moved to the activated MCP connection.
	preparedProvider.Commit(ctx)
	publishedOp, err := s.signedOAuthMCPOperations.Advance(ctx, op, agentcfg.SignedOAuthMCPPhasePublished, rev.RevisionID)
	if err != nil {
		// Data-plane publication is already visible. Returning the checkpoint
		// error preserves recovery: the exact JTI resumes by proving the active
		// pair and only writes the missing durable phase.
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	if err := s.commitSignedOAuthMCPFence(ctx, id.TenantID, req.AgentID, operationKind, publishedOp); err != nil {
		return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{}, err
	}
	return signedCapabilityResponse(rev, providerName, connection.Name), nil
}

func signedCapabilityExchangeBinding(binding agentcfg.SignedOAuthMCPBinding, sink string) toolauth.SignedCapabilityExchangeBinding {
	return toolauth.SignedCapabilityExchangeBinding{
		TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID, AgentID: binding.AgentID, ProviderName: binding.ProviderName,
		CapabilityRevision: binding.CapabilityRevision, PairFingerprint: agentcfg.SignedOAuthMCPPairFingerprint(binding),
		URLDigest: binding.URLDigest, SinkDigest: binding.SinkDigest, Audience: binding.Audience, Resource: sink,
	}
}

func (s *Service) commitSignedOAuthMCPFence(ctx context.Context, tenant, agentID, operationKind string, op agentcfg.SignedOAuthMCPOperation) error {
	physical, ok := s.registry.(physicalActiveRegistry)
	if !ok || physical == nil {
		return ErrSignedCapabilityUnavailable
	}
	fence, err := s.signedOAuthMCPFences.Load(ctx, tenant, agentID)
	if err != nil {
		return err
	}
	if fence.OperationKind != operationKind || fence.Fingerprint != op.Fingerprint || fence.Phase == agentcfg.SignedOAuthMCPFenceAborted {
		return fmt.Errorf("%w: activation fence does not bind published operation", agentcfg.ErrSignedCapabilityPending)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: op.Binding.UserID, SessionID: op.Binding.SessionID}}
	if fence.Phase == agentcfg.SignedOAuthMCPFenceCommitted {
		candidate, getErr := s.registry.Get(ctx, q, agentID, op.RevisionID, agentcfg.ConfigScopeAgent)
		active, activeSet, activeErr := physical.PhysicalActive(ctx, q, agentID, agentcfg.ConfigScopeAgent)
		if getErr == nil && activeErr == nil && activeSet && fence.CandidateRevisionID == op.RevisionID && candidate.ContentHash == fence.CandidateContentHash &&
			signedCapabilityPairMatchesOperation(candidate.Payload.SignedOAuthMCPPair, tenant, op.Binding, operationKind) &&
			signedCapabilityPairMatchesOperation(active.Payload.SignedOAuthMCPPair, tenant, op.Binding, operationKind) {
			return nil
		}
		return errors.Join(fmt.Errorf("%w: committed fence or active revision does not bind published pair", agentcfg.ErrSignedCapabilityPending), getErr, activeErr)
	}
	revision, err := requirePhysicalActiveRevision(ctx, physical, q, agentID, op.RevisionID, fence.CandidateContentHash)
	if err != nil || !signedCapabilityPairMatchesOperation(revision.Payload.SignedOAuthMCPPair, tenant, op.Binding, operationKind) {
		return fmt.Errorf("%w: published operation candidate is not physically active", agentcfg.ErrSignedCapabilityPending)
	}
	_, err = s.signedOAuthMCPFences.Advance(ctx, fence, agentcfg.SignedOAuthMCPFenceCommitted, op.RevisionID)
	return err
}

func signedCapabilityResponse(rev agentcfg.Revision, providerName, connectionName string) prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse {
	return prototypes.AgentConfigRegisterOAuthMCPCapabilityResponse{Revision: revisionToWire(rev), ProviderName: providerName, ConnectionName: connectionName, ProtocolVersion: prototypes.ProtocolVersion}
}

func signedCapabilityPairMatches(pair *agentcfg.SignedOAuthMCPPair, tenant string, binding agentcfg.SignedOAuthMCPBinding) bool {
	if pair == nil {
		return false
	}
	return agentcfg.SignedOAuthMCPPairFingerprint(agentcfg.SignedOAuthMCPBinding{
		TenantID: tenant, UserID: pair.OwnerUserID, SessionID: pair.OwnerSessionID, AgentID: binding.AgentID, Broker: pair.Broker,
		ProviderName: pair.ProviderName, CapabilityRevision: pair.CapabilityRevision,
		URLDigest: pair.URLDigest, SinkDigest: pair.SinkDigest, Audience: pair.Audience, Scopes: pair.Scopes, Connection: pair.Connection,
	}) == agentcfg.SignedOAuthMCPPairFingerprint(binding)
}

func signedCapabilityPairMatchesOperation(pair *agentcfg.SignedOAuthMCPPair, tenant string, binding agentcfg.SignedOAuthMCPBinding, operationKind string) bool {
	return pair != nil && pair.AuthorityOperationKind == operationKind && signedCapabilityPairMatches(pair, tenant, binding)
}

func (s *Service) rejectIncompletePriorSignedPairLifetime(ctx context.Context, id identity.Identity, agentID, operationKind string) error {
	continuation := ""
	for {
		operations, next, err := s.signedOAuthMCPOperations.ScanTenantPage(ctx, id.TenantID, state.MaxStateScanLimit, continuation)
		if err != nil {
			return err
		}
		for _, operation := range operations {
			kind, err := s.signedOAuthMCPOperations.Kind(operation.ReplayKey)
			if err != nil {
				return err
			}
			if kind == operationKind || operation.Binding.UserID != id.UserID || operation.Binding.SessionID != id.SessionID || operation.Binding.AgentID != agentID {
				continue
			}
			switch operation.Phase {
			case agentcfg.SignedOAuthMCPPhasePublished, agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted,
				agentcfg.SignedOAuthMCPPhaseCatalogUnpublished, agentcfg.SignedOAuthMCPPhaseTeardownReceipted:
				return fmt.Errorf("%w: prior signed capability pair lifetime is still %q", agentcfg.ErrSignedCapabilityPending, operation.Phase)
			}
		}
		if next == "" {
			return nil
		}
		continuation = next
	}
}

const signedCapabilityCleanupTimeout = 5 * time.Second

func closePreparedSignedCapability(ctx context.Context, connection PreparedConnection, provider PreparedOAuthProvider) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), signedCapabilityCleanupTimeout)
	defer cancel()
	var connectionErr, providerErr error
	if connection != nil {
		connectionErr = connection.Close(cleanup)
	}
	if provider != nil {
		providerErr = provider.Close(cleanup)
	}
	return errors.Join(connectionErr, providerErr)
}

func signedCapabilityAttachmentFingerprint(connection agentcfg.SignedOAuthMCPConnectionDescriptor, operationKind string) string {
	descriptor := agentcfg.SignedOAuthMCPConnectionFingerprint(connection)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(operationKind), operationKind, len(descriptor), descriptor)))
	return hex.EncodeToString(sum[:])
}

func signedCapabilityPairAttachmentFingerprint(pair *agentcfg.SignedOAuthMCPPair) string {
	if pair == nil {
		return ""
	}
	return signedCapabilityAttachmentFingerprint(pair.Connection, pair.AuthorityOperationKind)
}

func normalizeSignedCapabilityConnection(in prototypes.SignedOAuthMCPConnectionDescriptor, canonicalURL string) (prototypes.SignedOAuthMCPConnectionDescriptor, error) {
	const maxSignedCapabilityTimeoutMS = int((5 * time.Minute) / time.Millisecond)
	out := in
	out.Name = strings.TrimSpace(out.Name)
	out.URL = canonicalURL
	if out.Name == "" || out.ConnectTimeoutMS < 0 || out.RequestTimeoutMS < 0 || out.ConnectTimeoutMS > maxSignedCapabilityTimeoutMS || out.RequestTimeoutMS > maxSignedCapabilityTimeoutMS {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: name is required and timeouts must be between 0 and %d ms", ErrInvalidSignedCapabilityDescriptor, maxSignedCapabilityTimeoutMS)
	}
	var err error
	if out.ToolAllowlist, err = agentcfg.CanonicalScopes(out.ToolAllowlist); err != nil {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: tool_allowlist: %w", ErrInvalidSignedCapabilityDescriptor, err)
	}
	if out.ToolDenylist, err = agentcfg.CanonicalScopes(out.ToolDenylist); err != nil {
		return prototypes.SignedOAuthMCPConnectionDescriptor{}, fmt.Errorf("%w: tool_denylist: %w", ErrInvalidSignedCapabilityDescriptor, err)
	}
	return out, nil
}

func signedCapabilityEnvelopeKID(raw string) (string, error) {
	token, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil || token == nil {
		return "", fmt.Errorf("%w: malformed authority envelope", agentcfg.ErrSignedCapabilityAuthority)
	}
	kidValue := token.Header["kid"]
	kid, ok := kidValue.(string)
	if !ok {
		return "", fmt.Errorf("%w: kid header is not a string", agentcfg.ErrSignedCapabilityAuthority)
	}
	if strings.TrimSpace(kid) == "" {
		return "", fmt.Errorf("%w: authority envelope has no key id", agentcfg.ErrSignedCapabilityAuthority)
	}
	return kid, nil
}

func signedCapabilityJTIHash(jti string) string {
	sum := sha256.Sum256([]byte(jti))
	return hex.EncodeToString(sum[:])
}
