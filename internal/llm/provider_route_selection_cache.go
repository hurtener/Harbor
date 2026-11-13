package llm

import (
	"context"
	"sync"
	"time"
)

const (
	// DefaultProviderRouteSelectionCacheEntries bounds the selection cache.
	// One entry is a few hundred bytes of opaque identifiers, so the default
	// bound stays well under 1 MiB.
	DefaultProviderRouteSelectionCacheEntries = 1024
	// providerRouteSelectionReuseMargin is the minimum remaining lifetime a
	// cached selection must have to be reused. The selection only fixes the
	// credential-free provider/model decision: the leaf still resolves and
	// exact-matches a fresh credential for every attempt, so the margin only
	// keeps a reused selection from expiring before the outer wrapper's own
	// receipt-time expiry recheck.
	providerRouteSelectionReuseMargin = 5 * time.Second
)

// ProviderRouteSelectionKey identifies one credential-free route decision.
// It binds the full trusted principal (tenant, user, effective Agent) and
// the exact opaque route coordinates, so a selection is never reused across
// principals, routes, or generations. Run, task, call, and session IDs are
// deliberately excluded: the selection is a function of the principal and the
// route, and every attempt still resolves a fresh, attempt-bound credential.
type ProviderRouteSelectionKey struct {
	TenantID                     string
	UserID                       string
	SessionOwner                 string
	EffectiveAgentID             string
	RouteID                      string
	RouteGeneration              uint64
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	CredentialAssetGeneration    uint64
	ModelSelector                string
	ModelProfileSupported        bool
	Purpose                      ProviderRoutePurpose
}

// ProviderRouteSelectionKeyFor derives the cache key from a trusted request.
func ProviderRouteSelectionKeyFor(req ProviderRouteRequest) ProviderRouteSelectionKey {
	return ProviderRouteSelectionKey{
		TenantID: req.TenantID, UserID: req.UserID, SessionOwner: req.SessionOwner, EffectiveAgentID: req.EffectiveAgentID,
		RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID:         req.ProviderConnectionID,
		ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration:    req.CredentialAssetGeneration,
		ModelSelector:                req.ModelSelector,
		ModelProfileSupported:        req.ModelProfileSupported,
		Purpose:                      req.Purpose,
	}
}

// ProviderRouteSelectionCache retains credential-free route selections until
// their resolver-issued expiry. It never stores credentials: only
// SelectedProviderRoute values, which by construction carry none. It is
// internally synchronized and bounded; a nil cache is a valid no-op.
type ProviderRouteSelectionCache struct {
	mu      sync.Mutex
	max     int
	entries map[ProviderRouteSelectionKey]SelectedProviderRoute
}

// NewProviderRouteSelectionCache builds a bounded cache. A non-positive max
// uses DefaultProviderRouteSelectionCacheEntries.
func NewProviderRouteSelectionCache(maxEntries int) *ProviderRouteSelectionCache {
	if maxEntries <= 0 {
		maxEntries = DefaultProviderRouteSelectionCacheEntries
	}
	return &ProviderRouteSelectionCache{max: maxEntries, entries: make(map[ProviderRouteSelectionKey]SelectedProviderRoute)}
}

// Lookup returns a defensive copy of a cached selection that remains valid
// for at least the reuse margin. An expired entry is evicted.
func (c *ProviderRouteSelectionCache) Lookup(key ProviderRouteSelectionKey, now time.Time) (SelectedProviderRoute, bool) {
	if c == nil {
		return SelectedProviderRoute{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	selected, ok := c.entries[key]
	if !ok {
		return SelectedProviderRoute{}, false
	}
	if !selected.ExpiresAt.After(now.Add(providerRouteSelectionReuseMargin)) {
		delete(c.entries, key)
		return SelectedProviderRoute{}, false
	}
	return cloneSelectedProviderRoute(selected), true
}

// Store retains a selection already validated against key's request. When
// the cache is full it first drops expired entries, then the entry closest
// to expiry, so memory stays bounded regardless of principal churn.
func (c *ProviderRouteSelectionCache) Store(key ProviderRouteSelectionKey, selected SelectedProviderRoute, now time.Time) {
	if c == nil || !selected.ExpiresAt.After(now.Add(providerRouteSelectionReuseMargin)) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		var oldestKey ProviderRouteSelectionKey
		var oldest time.Time
		found := false
		for k, v := range c.entries {
			if !v.ExpiresAt.After(now) {
				delete(c.entries, k)
				continue
			}
			if !found || v.ExpiresAt.Before(oldest) {
				oldestKey, oldest, found = k, v.ExpiresAt, true
			}
		}
		if len(c.entries) >= c.max && found {
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = cloneSelectedProviderRoute(selected)
}

// Len reports the number of retained entries (diagnostics and tests).
func (c *ProviderRouteSelectionCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func cloneSelectedProviderRoute(selected SelectedProviderRoute) SelectedProviderRoute {
	out := selected
	if selected.Endpoint != nil {
		endpoint := *selected.Endpoint
		out.Endpoint = &endpoint
	}
	out.ModelProfile = cloneProviderModelProfile(selected.ModelProfile)
	return out
}

// selectProviderRouteCached consults the configured selection cache before
// calling the resolver's credential-free selection leg. It reports whether
// the cache answered.
func selectProviderRouteCached(ctx context.Context, cfg ProviderRouteConfig, req ProviderRouteRequest, now time.Time) (SelectedProviderRoute, bool, error) {
	key := ProviderRouteSelectionKeyFor(req)
	if selected, ok := cfg.SelectionCache.Lookup(key, now); ok {
		return selected, true, nil
	}
	selected, err := SelectProviderRoute(ctx, cfg, req, now)
	if err != nil {
		return SelectedProviderRoute{}, false, err
	}
	cfg.SelectionCache.Store(key, selected, now)
	return selected, false, nil
}

// PrepareProviderRouteSelection performs the credential-free selection leg
// ahead of a run and retains the result in cfg.SelectionCache until its
// expiry. The request must be a trusted, runtime-assembled run-purpose request
// (identity and Agent reach already admitted); its run/task/call IDs are
// opaque preparation correlation values. It returns cached=true when a valid
// selection was already retained and no resolver call was made. Credential
// resolution is untouched: it remains per attempt at the leaf.
func PrepareProviderRouteSelection(ctx context.Context, cfg ProviderRouteConfig, req ProviderRouteRequest) (cached bool, err error) {
	if cfg.SelectionCache == nil {
		return false, ErrProviderRouteResolverUnavailable
	}
	if req.Purpose != ProviderRoutePurposeRun {
		return false, ErrProviderRouteInvalid
	}
	_, cached, err = selectProviderRouteCached(ctx, cfg, req, time.Now())
	return cached, err
}
