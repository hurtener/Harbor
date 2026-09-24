// Package session owns cumulative short-term execution memory. Serving and
// embedding share its admission, checkpoint, journal and historical projection
// implementation. It persists through StateStore and resolves result references
// through ArtifactStore; it does not own another transcript or long-term memory.
//
// The package is below runtime composition so memory inspection and mutation can
// use the same owner without importing run-context assembly. Planner policy and
// governed compaction remain outside this package. Historical evidence never
// grants authority to repeat an action, and recovery refuses unknown outcomes.
package session
