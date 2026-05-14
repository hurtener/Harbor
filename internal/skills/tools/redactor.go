package tools

import (
	"regexp"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/capfilter"
)

// piiPlaceholder is the canonical redacted marker for any PII pattern
// hit. Single sentinel so audit consumers can grep without
// enumerating variants.
//
// The disallowed-tool-name replacement variants and the tool-name
// scrub itself live in [capfilter] — the single source shared with
// the virtual directory (internal/skills, Phase 39).
const piiPlaceholder = "[REDACTED-PII]"

// PII patterns. Compiled once at package load — concurrent reuse is
// safe (compiled `*regexp.Regexp` values are read-only after compile,
// see stdlib godoc).
var (
	// piiEmail catches `local@domain.tld`-shaped emails. Conservative
	// — does NOT attempt to validate every RFC 5322 corner case;
	// the audit-time pattern is "obvious email shape," not
	// "deliverable email."
	piiEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// piiBearer catches `Bearer <token>` and `Authorization: Bearer
	// <token>` headers in skill text. Case-insensitive.
	piiBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
	// piiPhone catches NANP-shaped numbers + international `+`-prefix
	// shapes. Phone-number canonicalisation is not RFC-perfect; the
	// pattern catches the common operator-controllable shapes.
	piiPhone = regexp.MustCompile(`(?:\+?\d{1,3}[\s\-.]?)?\(?\d{3,4}\)?[\s\-.]?\d{3,4}[\s\-.]?\d{3,4}`)
	// piiURLQuery catches `?key=value(&key=value)*` query strings
	// that often carry tokens / credentials. Replaced atomically so
	// the URL path remains visible.
	piiURLQuery = regexp.MustCompile(`\?[A-Za-z0-9_\-]+=[^\s]+(?:&[A-Za-z0-9_\-]+=[^\s]+)*`)
)

// Redact returns a copy of `s` with disallowed tool names scrubbed
// from every text field AND (when `cap.RedactPII=true`) PII patterns
// rewritten to `[REDACTED-PII]`.
//
// Skill fields rewritten:
//
//   - Title
//   - Description
//   - Trigger
//   - Steps (every entry)
//   - Preconditions (every entry)
//   - FailureModes (every entry)
//
// Slices on the returned skill are fresh allocations so the caller
// may not perturb the SkillStore's cached row.
//
// Tool-name redaction matches tool names with word-boundary regex so
// a tool named `email` doesn't false-positive on `"emails"`. The
// regex set is built per-call from `cap.AllowedTools` — at the
// per-skill scale, the cost is dominated by the underlying string
// rewrite, not the regex compilation.
//
// Concurrent-safe by construction: pure function over value inputs;
// no shared mutable state.
func Redact(s skills.Skill, cap CapabilityContext) skills.Skill {
	out := s

	// Build the disallowed-tool-name redactor closure. The closure
	// captures the resolved set so subsequent field rewrites stay
	// in lockstep.
	//
	// Brief 04 §4.5: the scrub operates on the planner-facing skill
	// text, NOT on the skill's `RequiredTools` slice itself —
	// callers reading provenance from `RequiredTools` still see the
	// true list. The set/disallowed/replacement/scrub primitives all
	// live in [capfilter], shared with the virtual directory.
	allowedTools := capfilter.BuildSet(cap.AllowedTools)
	toolReplacement := capfilter.Replacement(allowedTools)
	disallowed := capfilter.DisallowedNames(s.RequiredTools, allowedTools)

	rewrite := func(text string) string {
		text = capfilter.Scrub(text, disallowed, toolReplacement)
		if cap.RedactPII {
			text = scrubPII(text)
		}
		return text
	}

	out.Title = rewrite(s.Title)
	out.Description = rewrite(s.Description)
	out.Trigger = rewrite(s.Trigger)
	out.Steps = rewriteSlice(s.Steps, rewrite)
	out.Preconditions = rewriteSlice(s.Preconditions, rewrite)
	out.FailureModes = rewriteSlice(s.FailureModes, rewrite)

	return out
}

// scrubPII applies the four canonical PII regexes to `text`,
// replacing every hit with `piiPlaceholder`. Pattern ordering is
// deterministic so the output is stable across runs (important for
// golden tests).
func scrubPII(text string) string {
	if text == "" {
		return text
	}
	// URL query first — its pattern is more specific than the
	// generic `?key=value` portion that email/phone might otherwise
	// see.
	text = piiURLQuery.ReplaceAllString(text, piiPlaceholder)
	text = piiBearer.ReplaceAllString(text, piiPlaceholder)
	text = piiEmail.ReplaceAllString(text, piiPlaceholder)
	text = piiPhone.ReplaceAllString(text, piiPlaceholder)
	return text
}

// rewriteSlice returns a fresh slice with every entry passed through
// `fn`. Nil input → nil output; empty input → an empty (non-nil)
// slice so JSON-marshallers emit `[]` rather than `null`.
func rewriteSlice(in []string, fn func(string) string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fn(s)
	}
	return out
}

// normalizeSkill ensures every nil slice / map field on `s` is an
// empty (non-nil) value so the inproc driver's reflection-derived
// JSON Schema (which marks every non-pointer field as required and
// rejects `null` for `type: array` / `type: object`) accepts the
// marshalled output. Without this, a `nil` `Tags` slice marshalls as
// JSON `null` and the schema validator fails the response.
//
// Implementation detail of the planner-tool layer — the Phase 37
// `Skill` struct is intentionally tag-less (it is the storage
// envelope, not a wire shape); the inproc driver's strict-schema
// validation forces us to materialise nil → empty here.
func normalizeSkill(s skills.Skill) skills.Skill {
	if s.Tags == nil {
		s.Tags = []string{}
	}
	if s.Steps == nil {
		s.Steps = []string{}
	}
	if s.Preconditions == nil {
		s.Preconditions = []string{}
	}
	if s.FailureModes == nil {
		s.FailureModes = []string{}
	}
	if s.RequiredTools == nil {
		s.RequiredTools = []string{}
	}
	if s.RequiredNS == nil {
		s.RequiredNS = []string{}
	}
	if s.RequiredTags == nil {
		s.RequiredTags = []string{}
	}
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	return s
}
