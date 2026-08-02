package agentcfg

// cloneSignedOAuthMCPPair returns a deep canonical copy of immutable pair
// state. Scope and tool policy membership are sets, so ordering cannot create a
// second authority-bearing revision for the same capability.
func cloneSignedOAuthMCPPair(in *SignedOAuthMCPPair) *SignedOAuthMCPPair {
	if in == nil {
		return nil
	}
	out := *in
	out.Scopes = sortDedup(in.Scopes)
	out.Connection.ToolAllowlist = sortDedup(in.Connection.ToolAllowlist)
	out.Connection.ToolDenylist = sortDedup(in.Connection.ToolDenylist)
	return &out
}

// SignedOAuthMCPPairView returns a defensive copy of the immutable pair.
func (p ConfigPayload) SignedOAuthMCPPairView() (*SignedOAuthMCPPair, bool) {
	if p.SignedOAuthMCPPair == nil {
		return nil, false
	}
	return cloneSignedOAuthMCPPair(p.SignedOAuthMCPPair), true
}
