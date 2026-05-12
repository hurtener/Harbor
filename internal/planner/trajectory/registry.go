package trajectory

import "sync"

// HandleRegistry holds the non-serialisable half of ToolContext —
// the live callbacks, loggers, sockets, and file descriptors the
// runtime carries through tool invocations. The Trajectory only
// stores HandleIDs; the registry stores the underlying values by ID.
//
// V1 ships one driver: process-local, backed by sync.Map. Resume MUST
// run in the same Runtime process. A distributed handle directory is
// a post-V1 RFC concern (RFC §6.3 + RFC §12 + brief 02 §4).
//
// Concurrent reuse contract (D-025): every method is safe to call
// from N goroutines on a single shared instance. The process-local
// driver is sync.Map-backed; concurrent_test.go is the gate.
//
// Get on a missing HandleID returns ErrToolContextLost — never
// (nil, nil). This is the load-bearing fail-loudly contract.
type HandleRegistry interface {
	// Set installs value under id. Re-installing an existing id
	// overwrites silently (standard map semantics). The runtime is
	// responsible for collision-free HandleID generation (ULIDs are
	// the recommended convention).
	Set(id HandleID, value any)

	// Get retrieves the value under id. Returns
	// (nil, ErrToolContextLost{Handle: id}) on miss — never
	// (nil, nil). This is the fail-loudly contract that closes the
	// predecessor's silent-tool-context-loss bug.
	Get(id HandleID) (any, error)

	// Delete removes the mapping for id. Deleting a non-existent id
	// is a no-op (returns no error).
	Delete(id HandleID)
}

// processLocalRegistry is the V1 driver — a sync.Map keyed by HandleID.
// Reads / writes / deletes are all O(1) amortised; sync.Map's tuned
// for read-heavy workloads, which matches the registry's access
// pattern (one Set on tool dispatch, many Gets across pause/resume).
type processLocalRegistry struct {
	m sync.Map // map[HandleID]any
}

// NewProcessLocalRegistry constructs the V1 process-local HandleRegistry
// driver. Returns a non-nil HandleRegistry ready for concurrent use.
func NewProcessLocalRegistry() HandleRegistry {
	return &processLocalRegistry{}
}

// Set installs value under id. Concurrent Set/Get/Delete is safe.
func (r *processLocalRegistry) Set(id HandleID, value any) {
	r.m.Store(id, value)
}

// Get retrieves the value under id. Returns ErrToolContextLost on miss
// — never (nil, nil). The fail-loudly path is the bug closure.
func (r *processLocalRegistry) Get(id HandleID) (any, error) {
	v, ok := r.m.Load(id)
	if !ok {
		return nil, ErrToolContextLost{Handle: id}
	}
	return v, nil
}

// Delete removes the mapping for id. Deleting a non-existent id is a
// no-op (sync.Map.Delete is itself a no-op on missing keys).
func (r *processLocalRegistry) Delete(id HandleID) {
	r.m.Delete(id)
}
