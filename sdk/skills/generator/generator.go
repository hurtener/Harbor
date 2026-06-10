// Package generator is the public SDK facade over Harbor's
// internal/skills/generator package — the in-runtime `skill_propose`
// tool that lets an agent persist a reusable skill from a successful
// trajectory (RFC §3.6, §6.7; D-204/D-206). Alias-based re-exports
// only: no behavior lives here. Added in Phase 112b alongside
// sdk/skills/tools (D-201: one skills surface; the recipe registers
// the generator next to the retrieval handlers). The proposal
// validation and audit emission internals are deliberately private.
package generator

import (
	internal "github.com/hurtener/Harbor/internal/skills/generator"
)

// Deps carries the generator's shared dependencies (bus, redactor).
type Deps = internal.Deps

// Register installs the `skill_propose` tool onto a catalog, wired
// against the store + deps.
var Register = internal.Register
