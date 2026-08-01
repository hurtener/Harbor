// providers.go — the owner-tagged OAuth provider SET.
//
// A ProviderSet HOLDS constructed provider INSTANCES (not driver factories —
// that is registry.go). It is the runtime home the MCP attach path resolves a
// connection's `oauth_provider` binding against, and the home a
// Protocol-installed provider descriptor lands its constructed provider into.
//
// # Not the driver registry, not a §4.4 seam
//
// registry.go is the OAuth flow-strategy DRIVER registry (interface +
// drivers/<name>/ + Resolve factory). This file is a different concern: it
// holds INSTANCES keyed by bare name. It is deliberately NOT a §4.4 driver seam
// — it has no drivers/ subdirectory and no internal/drivers/prod registration,
// because a set of instances has nothing to dispatch. Naming it for what it
// holds (a provider set) keeps a call site from confusing auth.Register (a
// driver) with ProviderSet.Install (an instance).
//
// # Process-global bare-name resolution + owner-tagged reconcile
//
// Resolution is by BARE NAME across every session (the tool catalog +
// MCP registry stay process-global and deployment-shared; the set mirrors that
// model for provider instances). Runtime-installed entries additionally carry
// an OWNER tag (tenant, agent) used for EXACTLY one thing beyond nothing:
// scoping the run-start reconcile VIEW so a run only ever uninstalls ITS OWN
// owner's installed providers — never a boot-seeded provider, never another
// owner's install. The owner is NOT an isolation principal and never a dispatch
// or storage key (agent_id is not part of the isolation tuple).
//
// # Concurrent reuse
//
// A ProviderSet is a compiled artifact: safe to share across N goroutines. The
// only mutable state is the entry map, guarded by an RWMutex and documented
// internally-synchronised. Uninstall removes the entry under the write lock and
// closes the provider AFTER releasing it, so a Get racing an Uninstall returns
// the live provider or `false` — never a half-closed handle observed through
// the map.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"
)

// ProviderSet is the runtime home of constructed OAuth provider instances the
// MCP attach path resolves a binding against. Resolution is by bare name
// (process-global); runtime-installed entries carry an owner tag for
// reconcile-view scoping only. See the file doc for the full contract.
type ProviderSet interface {
	// Get resolves a provider by BARE name (boot-seeded or runtime-installed).
	// The bool reports presence.
	Get(name string) (OAuthProvider, bool)
	// Names returns every resolvable provider name (boot + installed), sorted —
	// for a fail-loud "registered: …" message.
	Names() []string
	// InstalledFor returns the names of the RUNTIME-INSTALLED providers owned by
	// owner (never boot-seeded, never another owner's), sorted — the owner-scoped
	// reconcile view. A zero owner returns nil.
	InstalledFor(owner Owner) []string
	// Install adds (or upserts) a runtime-installed provider under name, tagged
	// with owner. A name colliding with a boot-seeded provider fails loud
	// (ErrProviderBootCollision); a name colliding with a DIFFERENT owner's
	// install fails loud (ErrProviderOwnerCollision); a re-install by the SAME
	// owner replaces (and closes) the prior instance. owner must be non-zero.
	Install(owner Owner, name string, p OAuthProvider) error
	// Stage installs a provider without closing the displaced same-owner
	// instance and returns an exact commit/rollback receipt.
	Stage(owner Owner, name string, p OAuthProvider) (ProviderSwap, error)
	// Uninstall removes the runtime-installed provider under name IFF owner
	// matches the installed entry's owner, and CLOSES it (so a still-bound
	// connection's next call fails LOUD, never an unauthenticated dial). A
	// missing name is an idempotent no-op; a boot-seeded (zero-owner) name is
	// refused (ErrProviderBootProtected) — boot providers are edited in yaml +
	// restart, never uninstalled over the control plane; a cross-owner drop is
	// refused (ErrProviderOwnerCollision).
	//
	// Owner-scoping is enforced HERE, at the store boundary, independently of
	// caller-side owner resolution — a second, independent owner check
	// (defense in depth). The
	// `agent_config.remove_oauth_provider` handler already resolves the name
	// within the caller's OWN active agent-config revision (under lockAgent), so
	// the store check is the second, independent gate: the store refuses a drop
	// whose owner does not match the entry's owner, mirroring Install's
	// owner-collision refusal. Never weakens the credential-sink invariant
	// (owner is a reconcile-view tag, never a credential sink).
	Uninstall(ctx context.Context, owner Owner, name string) error
}

// ProviderSwap is a reversible provider-set publication receipt.
type ProviderSwap interface {
	Commit(ctx context.Context) error
	Rollback() error
}

// ProviderSet sentinel errors.
var (
	// ErrProviderNameEmpty — Install was called with an empty name.
	ErrProviderNameEmpty = errors.New("auth: provider set install: name is empty")
	// ErrProviderNilInstance — Install was called with a nil provider.
	ErrProviderNilInstance = errors.New("auth: provider set install: provider instance is nil")
	// ErrProviderOwnerMissing — Install was called with a zero owner (a
	// runtime-installed entry must carry its (tenant, agent) reconcile owner).
	ErrProviderOwnerMissing = errors.New("auth: provider set install: owner is zero (a runtime-installed provider must carry its (tenant, agent) owner)")
	// ErrProviderBootCollision — Install named a boot-seeded provider. Boot
	// wins; a runtime install never shadows a boot-declared provider.
	ErrProviderBootCollision = errors.New("auth: provider set install: name collides with a boot-declared provider (boot wins; edit yaml + restart)")
	// ErrProviderOwnerCollision — a runtime-installed name is owned by a
	// DIFFERENT owner than the caller. Install refuses shadowing another owner's
	// name; Uninstall refuses dropping another owner's provider (the
	// defense-in-depth store-boundary owner check). In a shared runtime a
	// runtime-installed name is deployment-global (the bounded guarantee: no
	// silent shadow, no cross-owner drop).
	ErrProviderOwnerCollision = errors.New("auth: provider set: name owned by a different owner (a runtime-installed name is deployment-global)")
	// ErrProviderBootProtected — Uninstall named a boot-seeded provider. A boot
	// provider is edited in yaml + restart, never uninstalled over the control
	// plane.
	ErrProviderBootProtected = errors.New("auth: provider set uninstall: name is a boot-declared provider (edit yaml + restart)")
)

// providerEntry is one set member: the instance plus its owner. A zero owner
// denotes a boot-seeded entry (never reconcile-enumerated, never uninstallable).
type providerEntry struct {
	prov  OAuthProvider
	owner Owner
}

// providerSet is the internally-synchronised concrete ProviderSet.
type providerSet struct {
	mu          sync.RWMutex
	entries     map[string]providerEntry
	generations map[string]uint64
}

// NewProviderSet builds a ProviderSet seeded from the boot-built provider map
// (BuildProviders' result plus any injected providers). Seeded entries are
// boot-owned (zero owner): resolvable by bare name, never reconcile-enumerated,
// never uninstallable over the control plane. A nil seed yields an empty set.
func NewProviderSet(seed map[string]OAuthProvider) ProviderSet {
	entries := make(map[string]providerEntry, len(seed))
	for name, p := range seed {
		if name == "" || p == nil {
			continue
		}
		entries[name] = providerEntry{prov: p}
	}
	return &providerSet{entries: entries, generations: make(map[string]uint64)}
}

func (s *providerSet) Get(name string) (OAuthProvider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[name]
	if !ok {
		return nil, false
	}
	return e.prov, true
}

func (s *providerSet) Names() []string {
	s.mu.RLock()
	out := make([]string, 0, len(s.entries))
	for name := range s.entries {
		out = append(out, name)
	}
	s.mu.RUnlock()
	sort.Strings(out)
	return out
}

func (s *providerSet) InstalledFor(owner Owner) []string {
	if owner.IsZero() {
		return nil
	}
	s.mu.RLock()
	var out []string
	for name, e := range s.entries {
		if e.owner == owner {
			out = append(out, name)
		}
	}
	s.mu.RUnlock()
	sort.Strings(out)
	return out
}

func (s *providerSet) Install(owner Owner, name string, p OAuthProvider) error {
	swap, err := s.Stage(owner, name, p)
	if err != nil {
		return err
	}
	// Install's publication is already committed; retain the historical
	// contract that displaced-provider cleanup cannot turn it into a reported
	// install refusal. Prepared callers use the receipt directly and warn.
	// Install predates the context-aware staged API. Bridge that legacy
	// non-context boundary with a bounded cleanup context so a displaced
	// provider cannot stall installation indefinitely.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := swap.Commit(cleanupCtx); err != nil {
		slog.Warn("auth: provider installed but displaced provider cleanup failed", "provider", name, "error", err.Error())
	}
	return nil
}

func (s *providerSet) Stage(owner Owner, name string, p OAuthProvider) (ProviderSwap, error) {
	if name == "" {
		return nil, ErrProviderNameEmpty
	}
	if p == nil {
		return nil, ErrProviderNilInstance
	}
	if owner.IsZero() || owner.Tenant == "" || owner.Agent == "" {
		return nil, fmt.Errorf("%w (name=%q tenant=%q agent=%q)", ErrProviderOwnerMissing, name, owner.Tenant, owner.Agent)
	}
	s.mu.Lock()
	existing, ok := s.entries[name]
	if ok {
		switch {
		case existing.owner.IsZero():
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %q", ErrProviderBootCollision, name)
		case existing.owner != owner:
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %q (installed by tenant=%q agent=%q)", ErrProviderOwnerCollision, name, existing.owner.Tenant, existing.owner.Agent)
		}
		// Same-owner re-install (upsert): replace and close the prior instance
		// AFTER releasing the lock.
	}
	s.entries[name] = providerEntry{prov: p, owner: owner}
	s.generations[name]++
	generation := s.generations[name]
	s.mu.Unlock()
	return &providerSwap{set: s, name: name, prior: existing, hadPrior: ok, staged: p, generation: generation}, nil
}

type providerSwap struct {
	mu         sync.Mutex
	set        *providerSet
	name       string
	prior      providerEntry
	hadPrior   bool
	staged     OAuthProvider
	generation uint64
	done       bool
}

func (s *providerSwap) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	if s.hadPrior && s.prior.prov != nil && !sameOAuthProvider(s.prior.prov, s.staged) {
		return s.prior.prov.Close(ctx)
	}
	return nil
}

func (s *providerSwap) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.set.mu.Lock()
	defer s.set.mu.Unlock()
	if s.set.generations[s.name] != s.generation {
		return fmt.Errorf("auth: provider rollback refused for %q: staged provider is no longer current", s.name)
	}
	if s.hadPrior {
		s.set.entries[s.name] = s.prior
	} else {
		delete(s.set.entries, s.name)
	}
	s.set.generations[s.name]++
	s.done = true
	return nil
}

func sameOAuthProvider(a, b OAuthProvider) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() || !av.Type().Comparable() {
		return false
	}
	return av.Interface() == bv.Interface()
}

// Uninstall implements ProviderSet.Uninstall. Owner-scoping is enforced HERE at
// the store boundary: the drop is refused unless owner matches the
// entry's owner, mirroring Install's owner-collision refusal — a second,
// independent gate on top of the caller-side owner resolution the
// remove_oauth_provider handler already performs.
func (s *providerSet) Uninstall(ctx context.Context, owner Owner, name string) error {
	s.mu.Lock()
	e, ok := s.entries[name]
	if !ok {
		s.mu.Unlock()
		return nil // idempotent — already absent.
	}
	if e.owner.IsZero() {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrProviderBootProtected, name)
	}
	if e.owner != owner {
		// Cross-owner drop refused at the store boundary: an owner may only
		// uninstall its OWN installed provider (defense in depth — never trust
		// caller-side owner resolution alone). The entry is untouched.
		s.mu.Unlock()
		return fmt.Errorf("%w: %q (installed by tenant=%q agent=%q)", ErrProviderOwnerCollision, name, e.owner.Tenant, e.owner.Agent)
	}
	delete(s.entries, name)
	s.generations[name]++
	s.mu.Unlock()
	// Close the released instance so a still-bound connection's next call fails
	// LOUD (never an unauthenticated dial). Close is idempotent.
	if e.prov != nil {
		if err := e.prov.Close(ctx); err != nil {
			return fmt.Errorf("auth: provider set uninstall %q: close: %w", name, err)
		}
	}
	return nil
}
