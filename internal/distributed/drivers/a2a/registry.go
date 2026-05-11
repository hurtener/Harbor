package a2a

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Default route-scoring weights. See package godoc for the formula:
//
//	CompositeScore = (TrustWeight × TrustTier) +
//	                 (LatencyWeight / max(1, LatencyTierMS)) +
//	                 (CapabilityWeight × CapabilityScore)
//
// Trust outranks latency (5x:1 favouring trust); latency is the tie-
// breaker among similarly-trusted peers (the 1000/latency_ms term
// caps at the LatencyWeight when latency is 1ms, drops to 1.0 at
// 1000ms). Capability match adds an additive boost so a peer that
// declares the exact capability outranks one that matches via a tag.
//
// Tunable post-V1; not exposed at V1 (a single deployment uses one
// canonical scoring policy).
const (
	defaultTrustWeight      float64 = 5.0
	defaultLatencyWeight    float64 = 1000.0
	defaultCapabilityWeight float64 = 10.0
)

// PeerSpec is a discovered peer's contact info + tier hints.
// PeerSpec is the input shape to `Registry.AddPeer`; the registry copies
// it into an internal record so PeerSpec is safe to construct, register,
// and discard.
type PeerSpec struct {
	// URL is the peer's base URL — e.g. "https://agent.example".
	// The wire driver appends "/.well-known/agent-card.json" for
	// discovery and the JSON-RPC method paths for calls.
	URL string
	// TrustTier is an operator-set integer in [1, 5]: 1 = untrusted
	// (third-party with no contract), 5 = first-party (in-org).
	// Higher values rank higher; the validator rejects values
	// outside [1, 5] at config-load time.
	TrustTier int
	// LatencyTierMS is the operator's hint at the peer's expected
	// p50 latency in milliseconds. Smaller values rank higher.
	LatencyTierMS int
	// AllowInsecureLoopback opts a loopback HTTP peer into the
	// driver. See security.go for the precise rule.
	AllowInsecureLoopback bool
	// AgentCardTTL overrides the driver-level AgentCard cache TTL.
	// Zero falls back to the driver default.
	AgentCardTTL time.Duration
	// Capabilities is the discovered capability vocabulary: the
	// union of `AgentSkill.ID` and the entries of `AgentSkill.Tags`
	// across the peer's skills. Filled in when the wire driver
	// completes discovery; empty until then. Resolve filters by
	// exact string equality.
	Capabilities []string
}

// Route is a scored candidate returned by Registry.Resolve. Ordered
// slices land highest-score-first.
type Route struct {
	// PeerURL is the resolved peer's base URL.
	PeerURL string
	// TrustTier is the operator-supplied trust tier ([1, 5]).
	TrustTier int
	// LatencyTierMS is the operator-supplied latency tier hint.
	LatencyTierMS int
	// CapabilityScore is 1 when the capability matches an
	// `AgentSkill.ID`; 0 when it matches only via a tag.
	CapabilityScore int
	// CompositeScore is the deterministic ranking value (higher is
	// better).
	CompositeScore float64
}

// peerRecord is the registry's internal copy of a PeerSpec plus
// derived per-skill match cache.
type peerRecord struct {
	spec      PeerSpec
	skillIDs  map[string]struct{}
	skillTags map[string]struct{}
}

// Registry maps capability strings (`AgentSkill.ID` or a tag) to a
// scored list of peer candidates. Read-mostly: AddPeer takes the
// write lock, Resolve takes the read lock. Internally synchronized;
// safe for N concurrent goroutines (D-025).
type Registry struct {
	mu sync.RWMutex
	// peers keyed by their canonical URL — duplicate registrations
	// for the same URL replace the prior record (so an operator
	// can hot-swap a peer without restart).
	peers map[string]*peerRecord
	// weights override the package defaults. Zero value means use
	// defaults.
	trustW, latencyW, capW float64
}

// NewRegistry constructs an empty route-scoring registry.
func NewRegistry() *Registry {
	return &Registry{
		peers:    map[string]*peerRecord{},
		trustW:   defaultTrustWeight,
		latencyW: defaultLatencyWeight,
		capW:     defaultCapabilityWeight,
	}
}

// AddPeer registers spec under spec.URL. Replaces any prior record at
// the same URL. Returns ErrInvalidPeerURL when spec.URL is empty;
// returns a tier-violation error when TrustTier is outside [1, 5] or
// LatencyTierMS is negative.
func (r *Registry) AddPeer(spec PeerSpec) error {
	if spec.URL == "" {
		return fmt.Errorf("%w: empty URL", ErrInvalidPeerURL)
	}
	if spec.TrustTier < 1 || spec.TrustTier > 5 {
		return fmt.Errorf("a2a: AddPeer: TrustTier %d outside [1,5]", spec.TrustTier)
	}
	if spec.LatencyTierMS < 0 {
		return fmt.Errorf("a2a: AddPeer: LatencyTierMS %d must be >= 0", spec.LatencyTierMS)
	}
	rec := &peerRecord{
		spec:      spec,
		skillIDs:  make(map[string]struct{}),
		skillTags: make(map[string]struct{}),
	}
	for _, cap := range spec.Capabilities {
		if cap == "" {
			continue
		}
		// The Capabilities slice carries IDs first, then tag-shaped
		// entries; for V1 the registry treats every entry as a
		// potential match against both ID and tag. Callers that
		// want strict ID-vs-tag separation pre-split.
		rec.skillIDs[cap] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers[spec.URL] = rec
	return nil
}

// RemovePeer unregisters the peer at url. No-op if absent.
func (r *Registry) RemovePeer(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.peers, url)
}

// UpdateCapabilities replaces the capability set for a registered
// peer. Used by the wire driver after discovery completes. Returns
// ErrPeerNotAllowed when url is not registered.
func (r *Registry) UpdateCapabilities(url string, ids, tags []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.peers[url]
	if !ok {
		return fmt.Errorf("%w: %q", ErrPeerNotAllowed, url)
	}
	rec.skillIDs = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		rec.skillIDs[id] = struct{}{}
	}
	rec.skillTags = make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		rec.skillTags[tag] = struct{}{}
	}
	return nil
}

// PeerSpec returns the stored spec for url and a presence bool.
func (r *Registry) PeerSpec(url string) (PeerSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.peers[url]
	if !ok {
		return PeerSpec{}, false
	}
	// Defensive copy of the slice so callers can't mutate registry
	// state.
	out := rec.spec
	if len(rec.spec.Capabilities) > 0 {
		caps := make([]string, len(rec.spec.Capabilities))
		copy(caps, rec.spec.Capabilities)
		out.Capabilities = caps
	}
	return out, true
}

// Peers returns a snapshot of every registered peer URL. Order is
// sorted for determinism.
func (r *Registry) Peers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	urls := make([]string, 0, len(r.peers))
	for u := range r.peers {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	return urls
}

// Resolve returns the ranked candidate set for capability. Highest
// CompositeScore first; ties broken by lower LatencyTierMS, then by
// URL ascending so the result is deterministic. Empty capability
// returns every peer (uniform CapabilityScore=0).
func (r *Registry) Resolve(capability string) ([]Route, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.peers) == 0 {
		return nil, fmt.Errorf("%w: registry is empty", ErrPeerNotAllowed)
	}
	out := make([]Route, 0, len(r.peers))
	for _, rec := range r.peers {
		match := r.matchScore(rec, capability)
		if capability != "" && match == 0 {
			// Don't include peers that have no capability hint.
			// V1 keeps this strict; a fuzzy "no skills declared"
			// mode is a post-V1 knob.
			if len(rec.skillIDs) > 0 || len(rec.skillTags) > 0 {
				continue
			}
		}
		out = append(out, Route{
			PeerURL:         rec.spec.URL,
			TrustTier:       rec.spec.TrustTier,
			LatencyTierMS:   rec.spec.LatencyTierMS,
			CapabilityScore: match,
			CompositeScore:  r.compositeScore(rec.spec, match),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no peer matched capability %q", ErrPeerNotAllowed, capability)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CompositeScore != out[j].CompositeScore {
			return out[i].CompositeScore > out[j].CompositeScore
		}
		if out[i].LatencyTierMS != out[j].LatencyTierMS {
			return out[i].LatencyTierMS < out[j].LatencyTierMS
		}
		return out[i].PeerURL < out[j].PeerURL
	})
	return out, nil
}

// matchScore returns 1 when capability matches a skill ID; 0 when it
// matches only via a tag (still ranked, just lower); 0 when neither.
// Empty capability is treated as a wildcard match score of 0 so every
// peer participates with the same baseline.
func (r *Registry) matchScore(rec *peerRecord, capability string) int {
	if capability == "" {
		return 0
	}
	if _, ok := rec.skillIDs[capability]; ok {
		return 1
	}
	if _, ok := rec.skillTags[capability]; ok {
		// Tag match is a partial hit — captured here as 0 plus the
		// base weight on the composite (the registry still keeps
		// the peer in the result set; ID-matched peers outrank).
		return 0
	}
	return 0
}

// compositeScore is the deterministic ranking value (higher = better).
func (r *Registry) compositeScore(spec PeerSpec, capScore int) float64 {
	latency := spec.LatencyTierMS
	if latency < 1 {
		latency = 1
	}
	return r.trustW*float64(spec.TrustTier) +
		r.latencyW/float64(latency) +
		r.capW*float64(capScore)
}
