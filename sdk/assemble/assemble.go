// Package assemble is the public SDK facade over Harbor's
// internal/runtime/assemble package — the ONE composition fan-out
// that turns a validated config into a running headless stack
// (RFC §3.6, §6.4; D-204/D-197). Alias-based re-exports only: no
// behavior lives here. Blank-import sdk/drivers/prod alongside this
// package so every Open the assembly performs resolves the
// production driver set; see docs/recipes/embed-harbor-headless.md
// for the full embedding walkthrough.
package assemble

import (
	internal "github.com/hurtener/Harbor/internal/runtime/assemble"
)

// Stack is the composed runtime bundle Assemble returns (stores,
// bus, LLM, memory, skills, tasks, catalog, executor, planner,
// run loop) with reverse-order Close.
type Stack = internal.Stack

// Options carries Assemble's injection points (logger, LLM snapshot
// override, planner override, skip knobs).
type Options = internal.Options

// DefaultMCPIdentity is the fallback transport-event identity for
// attached MCP servers.
var DefaultMCPIdentity = internal.DefaultMCPIdentity

// Assemble composes the full dependency-ordered runtime from a
// validated config, with partial-failure cleanup (a failed Assemble
// returns the partial Stack so the caller can drain it via Close).
var Assemble = internal.Assemble
