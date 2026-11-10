package skillpkg

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

// skillmd.go — the SKILL.md validation primitives.
//
// A complete skill package carries EXACTLY ONE root-level, case-exact
// `SKILL.md`. The two primitives here enforce that shape at the
// archive level (ValidateRootSkillEntries) and at the document level
// (ValidateSkillMarkdown). The full Skills.md parse (frontmatter
// field extraction, body-section normalization, attachment
// resolution) stays in the importer; this core is the canonical gate
// every consumer — the importer today, resolvers later — must agree
// on.

// SKILL.md sentinel errors. Compare via errors.Is.
var (
	// ErrSkillMDMissing — no SKILL.md anywhere in the archive.
	ErrSkillMDMissing = errors.New("skillpkg: SKILL.md missing")
	// ErrSkillMDMultiple — more than one root-level SKILL.md.
	ErrSkillMDMultiple = errors.New("skillpkg: multiple root SKILL.md entries")
	// ErrSkillMDNotRoot — a SKILL.md lives outside the archive root.
	ErrSkillMDNotRoot = errors.New("skillpkg: SKILL.md is not at the archive root")
	// ErrSkillMDCaseMismatch — a root-level document is a case
	// variant of SKILL.md (skill.md, SKILL.MD, ...).
	ErrSkillMDCaseMismatch = errors.New("skillpkg: SKILL.md is not case-exact")
	// ErrSkillMDTooLarge — the SKILL.md document exceeds the size
	// bound.
	ErrSkillMDTooLarge = errors.New("skillpkg: SKILL.md exceeds the size bound")
	// ErrSkillMDNotUTF8 — the SKILL.md document is not valid UTF-8.
	// A SKILL.md document must be UTF-8 (the frontmatter and the body
	// are text).
	ErrSkillMDNotUTF8 = errors.New("skillpkg: SKILL.md is not valid UTF-8")
	// ErrSkillMDFrontmatterMissing — SKILL.md has no YAML frontmatter
	// fences.
	ErrSkillMDFrontmatterMissing = errors.New("skillpkg: SKILL.md frontmatter missing")
	// ErrSkillMDMalformedYAML — SKILL.md frontmatter does not parse.
	ErrSkillMDMalformedYAML = errors.New("skillpkg: SKILL.md frontmatter malformed")
	// ErrSkillMDMissingTrigger — SKILL.md frontmatter has neither a
	// non-empty trigger nor a non-empty description to derive it from
	// (the planner match cue is mandatory).
	ErrSkillMDMissingTrigger = errors.New("skillpkg: SKILL.md trigger missing")
	// ErrSkillMDEmptySteps — SKILL.md body is blank: it declares no
	// procedural steps and carries no instruction content.
	ErrSkillMDEmptySteps = errors.New("skillpkg: SKILL.md steps empty")
)

// MarkdownLimits bounds the SKILL.md document scan.
type MarkdownLimits struct {
	// MaxBytes bounds the document size (default
	// MaxPackageSkillMDBytes).
	MaxBytes int64
}

// Normalize fills zero-valued limit fields with the canonical
// defaults.
func (l MarkdownLimits) Normalize() MarkdownLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = MaxPackageSkillMDBytes
	}
	return l
}

// ValidateRootSkillEntries enforces the one-root-SKILL.md invariant
// over the validated entry list:
//
//   - exactly one root-level, case-exact "SKILL.md";
//   - a case variant of SKILL.md anywhere → ErrSkillMDCaseMismatch;
//   - a SKILL.md inside a subdirectory → ErrSkillMDNotRoot;
//   - none at all → ErrSkillMDMissing; more than one → (defensively)
//     ErrSkillMDMultiple.
//
// The caller must run ValidateArchive first: this function operates
// on the canonical entry list and assumes paths are already
// canonicalized and collision-free.
func ValidateRootSkillEntries(entries []ArchiveEntry) error {
	exactRoot := 0
	for _, e := range entries {
		last := e.Path
		if i := strings.LastIndexByte(e.Path, '/'); i >= 0 {
			last = e.Path[i+1:]
		}
		if last == RootSkillFileName {
			if strings.Contains(e.Path, "/") {
				return fmt.Errorf("%w: %q must live at the archive root", ErrSkillMDNotRoot, e.Path)
			}
			exactRoot++
			continue
		}
		if strings.EqualFold(last, RootSkillFileName) {
			if strings.Contains(e.Path, "/") {
				return fmt.Errorf("%w: %q (non-root case variant)", ErrSkillMDNotRoot, e.Path)
			}
			return fmt.Errorf("%w: %q must be exactly %q", ErrSkillMDCaseMismatch, e.Path, RootSkillFileName)
		}
	}
	switch {
	case exactRoot == 0:
		return ErrSkillMDMissing
	case exactRoot > 1:
		return ErrSkillMDMultiple
	default:
		return nil
	}
}

// ValidateSkillMarkdown is the canonical document-level gate for the
// root SKILL.md bytes. It enforces:
//
//   - size bound (ErrSkillMDTooLarge);
//   - valid UTF-8 (ErrSkillMDNotUTF8 — the document is text);
//   - YAML frontmatter fences at the very start and a closing fence
//     (ErrSkillMDFrontmatterMissing / ErrSkillMDMalformedYAML);
//   - the frontmatter parses and carries a non-empty `trigger`, or a
//     non-empty `description` the trigger is derived from
//     (ErrSkillMDMalformedYAML / ErrSkillMDMissingTrigger);
//   - the body is not blank (ErrSkillMDEmptySteps). A canonical body
//     declares its steps under a `## Steps`-family heading; any other
//     non-blank body is free-form instruction content the importer
//     keeps verbatim.
//
// The full parse (field extraction, section normalization, attachment
// resolution, round-trip) remains the importer's; this gate is the
// shared structural minimum every consumer enforces.
func ValidateSkillMarkdown(b []byte, limits MarkdownLimits) error {
	limits = limits.Normalize()
	if int64(len(b)) > limits.MaxBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrSkillMDTooLarge, len(b), limits.MaxBytes)
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("%w: %d bytes are not valid UTF-8", ErrSkillMDNotUTF8, len(b))
	}
	b = NormalizeNewlines(b)
	if !bytes.HasPrefix(b, []byte("---\n")) {
		return ErrSkillMDFrontmatterMissing
	}
	raw, body, ok := scanFrontmatterFences(b)
	if !ok {
		return ErrSkillMDMalformedYAML
	}
	var fm struct {
		Trigger     string `yaml:"trigger"`
		Description string `yaml:"description"`
	}
	if err := UnmarshalFrontmatter(raw, &fm); err != nil {
		return fmt.Errorf("%w: %w", ErrSkillMDMalformedYAML, err)
	}
	if DeriveTrigger(fm.Trigger, fm.Description) == "" {
		return ErrSkillMDMissingTrigger
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return ErrSkillMDEmptySteps
	}
	return nil
}

// DeriveTrigger returns the planner match cue of a SKILL.md document.
// An explicit `trigger` wins verbatim. When it is blank, the trigger is
// derived from the frontmatter `description` with every whitespace run
// (newlines included) collapsed to one space, so a description-only
// document (the common external SKILL.md shape) imports with a
// deterministic single-line trigger. A derived trigger is capped at
// MaxPackageTextRunes on a rune boundary (ellipsis included), keeping
// the whole "use when" cue of an ordinary description. Returns "" when
// both are blank.
func DeriveTrigger(trigger, description string) string {
	if strings.TrimSpace(trigger) != "" {
		return trigger
	}
	return truncateRunes(strings.Join(strings.Fields(description), " "), MaxPackageTextRunes)
}

// truncateRunes returns s when it fits max runes, otherwise its first
// max-1 runes plus an ellipsis.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// NormalizeNewlines rewrites CRLF line endings to LF. SKILL.md
// documents written on Windows parse exactly like their LF form (the
// package hash covers the parsed fields, so both hash identically); an
// LF document is returned unchanged.
func NormalizeNewlines(b []byte) []byte {
	if !bytes.Contains(b, []byte("\r\n")) {
		return b
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// lenientScalarKeys are the descriptive top-level frontmatter keys
// whose unquoted values may carry `: ` (a common hand-written form,
// e.g. `description: Use for: reports`). Authority-bearing and list
// keys are never repaired.
var lenientScalarKeys = map[string]bool{"name": true, "description": true, "license": true, "compatibility": true}

// RepairPlainScalars quotes the value of a top-level `name` /
// `description` / `license` / `compatibility` line whose plain value
// contains `: ` — YAML reads that as a nested mapping and refuses it.
// A value that starts with a YAML indicator is left alone (it is not a
// plain prose scalar).
// Every other line is returned verbatim, and ok reports whether any
// line changed. Callers retry a failed strict decode with the repaired
// bytes only.
func RepairPlainScalars(raw []byte) (out []byte, ok bool) {
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		key, value, found := strings.Cut(line, ": ")
		if !found || !lenientScalarKeys[key] {
			continue
		}
		value = strings.TrimSpace(value)
		if !strings.Contains(value, ": ") || strings.ContainsAny(value[:1], "\"'|>[{&*!%@`:?-,#") {
			continue
		}
		quoted, err := yaml.Marshal(value)
		if err != nil {
			continue
		}
		lines[i] = key + ": " + strings.TrimSuffix(string(quoted), "\n")
		ok = true
	}
	if !ok {
		return raw, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// UnmarshalFrontmatter decodes raw frontmatter into v, retrying once
// with RepairPlainScalars when the strict decode fails and a repair
// applies. The original error is returned when no repair applies or
// the repaired document still fails.
func UnmarshalFrontmatter(raw []byte, v any, opts ...yaml.DecodeOption) error {
	err := yaml.UnmarshalWithOptions(raw, v, opts...)
	if err == nil {
		return nil
	}
	if repaired, ok := RepairPlainScalars(raw); ok {
		if retryErr := yaml.UnmarshalWithOptions(repaired, v, opts...); retryErr == nil {
			return nil
		}
	}
	return err
}

// MaxDescriptionSummaryRunes bounds the description excerpt that skill
// listings and import reviews carry. The full description stays on the
// stored skill.
const MaxDescriptionSummaryRunes = 500

// DescriptionSummary returns the human-visible excerpt of a skill
// description for listings and reviews: the description verbatim when
// it fits MaxDescriptionSummaryRunes, otherwise its first runes plus an
// ellipsis.
func DescriptionSummary(desc string) string {
	r := []rune(desc)
	if len(r) <= MaxDescriptionSummaryRunes {
		return desc
	}
	return string(r[:MaxDescriptionSummaryRunes-1]) + "…"
}

// scanFrontmatterFences extracts the raw frontmatter slice (between
// the fences, excluding them) and the body that follows the closing
// fence. Returns ok=false when the closing fence is absent.
func scanFrontmatterFences(b []byte) (raw, body []byte, ok bool) {
	// Opening fence is "---\n" at the very start (verified by the
	// caller); find the closing fence: a line that is exactly "---".
	start := len("---\n")
	idx := bytes.Index(b[start:], []byte("\n---"))
	if idx < 0 {
		return nil, nil, false
	}
	fenceAt := start + idx + 1
	raw = b[start:fenceAt]
	// Body begins after the closing fence's newline (if present).
	bodyStart := fenceAt + len("---")
	if bodyStart < len(b) && b[bodyStart] == '\n' {
		bodyStart++
	}
	return raw, b[bodyStart:], true
}
