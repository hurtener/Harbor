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
	// ErrSkillMDMissingTrigger — SKILL.md frontmatter has no
	// non-empty trigger (the planner match cue is mandatory).
	ErrSkillMDMissingTrigger = errors.New("skillpkg: SKILL.md trigger missing")
	// ErrSkillMDEmptySteps — SKILL.md body declares no procedural
	// steps.
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
//   - the frontmatter parses and carries a non-empty `trigger`
//     (ErrSkillMDMalformedYAML / ErrSkillMDMissingTrigger);
//   - the body declares at least one procedural step under a
//     `## Steps`-family heading (ErrSkillMDEmptySteps).
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
	if !bytes.HasPrefix(b, []byte("---\n")) {
		return ErrSkillMDFrontmatterMissing
	}
	raw, body, ok := scanFrontmatterFences(b)
	if !ok {
		return ErrSkillMDMalformedYAML
	}
	var fm struct {
		Trigger string `yaml:"trigger"`
	}
	if err := yaml.Unmarshal(raw, &fm); err != nil {
		return fmt.Errorf("%w: %v", ErrSkillMDMalformedYAML, err)
	}
	if strings.TrimSpace(fm.Trigger) == "" {
		return ErrSkillMDMissingTrigger
	}
	if !bodyHasSteps(body) {
		return ErrSkillMDEmptySteps
	}
	return nil
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

// bodyHasSteps reports whether the body declares at least one
// procedural step: a `## Steps`-family heading (case-insensitive,
// optional trailing colon) followed by a list item (`- `). This is a
// minimal mirror of the importer's section classification — the
// importer's parser remains canonical for the full parse.
func bodyHasSteps(body []byte) bool {
	inSteps := false
	for _, line := range strings.Split(string(body), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			h := strings.TrimSpace(strings.TrimPrefix(t, "## "))
			h = strings.TrimSuffix(strings.TrimSpace(h), ":")
			inSteps = strings.ToLower(h) == "step" || strings.ToLower(h) == "steps"
			continue
		}
		if inSteps && strings.HasPrefix(t, "- ") {
			return true
		}
	}
	return false
}
