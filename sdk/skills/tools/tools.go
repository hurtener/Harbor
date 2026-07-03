// Package tools is the public SDK facade over Harbor's
// internal/skills/tools package — the planner-facing skill retrieval
// handlers (`skill_search` / `skill_get` / `skill_list`) with the
// capability filter, tool-name redaction, and token budgeter built in
// (RFC §3.6, §6.7). Alias-based re-exports only: no
// behavior lives here. These are the ONE skills retrieval surface
// (the built-ins delegate here), and
// the use-memory-and-skills-from-go recipe documents them for
// headless consumers. The redactor and budgeter internals are
// deliberately private.
package tools

import (
	internal "github.com/hurtener/Harbor/internal/skills/tools"
)

// Handler vocabulary — aliases of the internal types.
type (
	// Deps carries the handlers' shared dependencies (bus).
	Deps = internal.Deps
	// CapabilityContext is the default-deny tool-visibility envelope —
	// skills whose required_tools are not a subset of AllowedTools are
	// invisible.
	CapabilityContext = internal.CapabilityContext
	// SearchArgs is `skill_search`'s typed input.
	SearchArgs = internal.SearchArgs
	// SearchResult is `skill_search`'s typed output.
	SearchResult = internal.SearchResult
	// GetArgs is `skill_get`'s typed input.
	GetArgs = internal.GetArgs
	// GetResult is `skill_get`'s typed output.
	GetResult = internal.GetResult
	// ListArgs is `skill_list`'s typed input.
	ListArgs = internal.ListArgs
	// ListResult is `skill_list`'s typed output.
	ListResult = internal.ListResult
)

// ErrSkillTooLarge — a skill exceeds the token budget even after the
// budgeter's reduction ladder. Callers compare via errors.Is.
var ErrSkillTooLarge = internal.ErrSkillTooLarge

// Register installs the `skill_search` / `skill_get` / `skill_list`
// tools onto a catalog, wired against the store + deps.
var Register = internal.Register

// SearchHandler is the `skill_search` implementation — capability
// filter + redaction over the store's ranked search.
var SearchHandler = internal.SearchHandler

// GetHandler is the `skill_get` implementation — capability filter +
// redaction + the token budgeter.
var GetHandler = internal.GetHandler

// ListHandler is the `skill_list` implementation — capability filter
// + redaction over a paged enumeration.
var ListHandler = internal.ListHandler
