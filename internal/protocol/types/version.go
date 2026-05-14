// Package types is the single source of truth for Harbor Protocol wire
// types (CLAUDE.md §8). Every Protocol message struct lives here; other
// packages import these types, and nothing else defines a Protocol
// message struct. The Phase 58 lint formalises this — Phase 54 lays the
// foundation so that lint is a no-op formalisation, not a cleanup.
//
// # What Phase 54 ships
//
// Phase 54 ships the Protocol task control surface's wire types: the
// flat IdentityScope every request carries, the StartRequest /
// StartResponse pair, and the ControlRequest / ControlResponse pair the
// nine steering-control methods share. The wire shapes are deliberately
// flat (string identity fields + a payload map) — a Protocol type that
// re-exported an internal runtime Go struct would be the RFC §5.1
// reject-on-sight smell ("a Protocol method that maps 1:1 to an internal
// Go function signature"). The runtime-facing translation lives in the
// protocol package's ControlSurface, not in these types.
//
// # Versioning
//
// ProtocolVersion is pinned here. Bumping it is an RFC change (RFC §5.3,
// CLAUDE.md §8) — the Protocol surface is versioned independently of the
// Runtime implementation so third-party Consoles are not whipsawed by a
// Runtime refactor.
package types

// ProtocolVersion is the pinned Harbor Protocol version. Bumping this
// constant is an RFC change (RFC §5.3): the Protocol surface is versioned
// independently of the Runtime, and a breaking change requires a
// deprecation window so third-party Consoles are not whipsawed.
//
// V1 ships 0.1.0 — the task control surface (Phase 54) is the first
// Protocol surface to land; the streaming-events / state-snapshot /
// topology / artifacts / traces / metrics surfaces (RFC §5.2) extend it
// in later phases without bumping the major while V1 is in flight.
const ProtocolVersion = "0.1.0"
