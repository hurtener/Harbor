package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/skills"
)

// frontmatterFence is the YAML-frontmatter delimiter Skills.md uses.
const frontmatterFence = "---"

// artifactScheme is the in-body URI scheme the importer substitutes
// for inline `![alt](path)` references after attachment upload.
// Format: `artifact://<ArtifactRef.ID>`.
const artifactScheme = "artifact://"

// imageRefRegexp matches an inline image reference: `![alt](path)`.
// The `alt` group captures the alt text (may be empty); the `path`
// group captures the path/URL. Multi-line off — image refs are
// single-line in spec-compliant Skills.md.
var imageRefRegexp = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// frontmatterRaw is the literal slice of bytes between the
// frontmatter fences (excluding the fences themselves). Captured
// verbatim so Export round-trips byte-stable even when authors
// hand-order YAML keys.
type frontmatterRaw struct {
	// Bytes is the verbatim content between the fences. INCLUDES the
	// trailing newline of the last line so the Export concatenation
	// produces `---\n<bytes>---\n` byte-equal to the source.
	Bytes []byte
}

// frontmatterFields is the parsed YAML view used for field
// extraction. We keep both the raw bytes (round-trip) and the parsed
// view (validation). Field names match the Skills.md spec's lean
// frontmatter plus Harbor-specific extras.
type frontmatterFields struct {
	Name               string   `yaml:"name"`
	Title              string   `yaml:"title"`
	Trigger            string   `yaml:"trigger"`
	TaskType           string   `yaml:"task_type"`
	Scope              string   `yaml:"scope"`
	Tags               []string `yaml:"tags"`
	RequiredTools      []string `yaml:"required_tools"`
	RequiredNamespaces []string `yaml:"required_namespaces"`
	RequiredTags       []string `yaml:"required_tags"`
}

// canonicalSection is one of the body sections the importer
// normalises. Heading text variations map to the same canonical
// value; Export emits the canonical heading.
type canonicalSection int

const (
	sectionUnknown canonicalSection = iota
	sectionSteps
	sectionPreconditions
	sectionFailureModes
)

// canonicalHeading returns the Export-side heading text for a
// canonical section. Used in exporter.go.
func canonicalHeading(s canonicalSection) string {
	switch s {
	case sectionSteps:
		return "## Steps"
	case sectionPreconditions:
		return "## Preconditions"
	case sectionFailureModes:
		return "## Failure modes"
	default:
		return ""
	}
}

// classifySection maps a stripped heading line (e.g. "## steps:") to
// a canonical section. Returns sectionUnknown for headings that
// don't map. Accepts:
//
//   - case-insensitive match on "step" | "steps"
//   - case-insensitive match on "precondition" | "preconditions"
//   - case-insensitive match on "failure mode" | "failure modes"
//   - trailing `:` tolerated
//   - leading whitespace already stripped by the caller
func classifySection(heading string) canonicalSection {
	h := strings.TrimSpace(heading)
	h = strings.TrimPrefix(h, "## ")
	h = strings.TrimSpace(h)
	h = strings.TrimSuffix(h, ":")
	h = strings.ToLower(h)
	h = strings.TrimSpace(h)
	switch h {
	case "step", "steps":
		return sectionSteps
	case "precondition", "preconditions":
		return sectionPreconditions
	case "failure mode", "failure modes":
		return sectionFailureModes
	default:
		return sectionUnknown
	}
}

// scanFrontmatter extracts the raw frontmatter slice + the rest of
// the body. Returns wrapped ErrMissingFrontmatter if the source
// does not begin with `---\n`, and wrapped ErrMalformedYAML if the
// closing fence is absent.
//
// The returned `bodyStart` is the byte offset in `src` where the
// body begins (immediately after the closing fence's newline).
func scanFrontmatter(src []byte) (frontmatterRaw, []byte, error) {
	if !bytes.HasPrefix(src, []byte(frontmatterFence+"\n")) &&
		!bytes.Equal(src, []byte(frontmatterFence)) {
		return frontmatterRaw{}, nil, fmt.Errorf("%w: source must start with `---\\n`",
			ErrMissingFrontmatter)
	}

	// Find the closing fence: a line containing exactly "---".
	// Start scanning after the opening fence's newline.
	bodyStart := len(frontmatterFence) + 1 // "---\n"
	if bodyStart > len(src) {
		return frontmatterRaw{}, nil, fmt.Errorf("%w: truncated frontmatter",
			ErrMalformedYAML)
	}

	remaining := src[bodyStart:]
	// Look for "\n---\n" or "\n---" at the end of file.
	closingIdx := -1
	scanFrom := 0
	for scanFrom < len(remaining) {
		// Find next "---" preceded by a newline.
		idx := bytes.Index(remaining[scanFrom:], []byte("\n"+frontmatterFence))
		if idx < 0 {
			break
		}
		abs := scanFrom + idx + 1 // position of "-" of the fence
		// Verify it's at the start of a line and followed by \n or EOF.
		fenceEnd := abs + len(frontmatterFence)
		if fenceEnd == len(remaining) {
			// fence is at EOF (no trailing newline). Accept.
			closingIdx = abs
			break
		}
		if fenceEnd < len(remaining) && remaining[fenceEnd] == '\n' {
			closingIdx = abs
			break
		}
		// false alarm — advance past this candidate.
		scanFrom = fenceEnd
	}
	if closingIdx < 0 {
		return frontmatterRaw{}, nil, fmt.Errorf("%w: closing `---` fence missing",
			ErrMalformedYAML)
	}

	rawFM := remaining[:closingIdx]
	// Body begins after the closing fence's newline. If the closing
	// fence is at EOF, body is empty.
	bodyOffset := closingIdx + len(frontmatterFence)
	if bodyOffset < len(remaining) && remaining[bodyOffset] == '\n' {
		bodyOffset++
	}
	body := remaining[bodyOffset:]

	return frontmatterRaw{Bytes: append([]byte(nil), rawFM...)}, append([]byte(nil), body...), nil
}

// parseFrontmatter parses the raw frontmatter bytes through goccy
// go-yaml into a frontmatterFields. Returns wrapped
// ErrMalformedYAML on YAML parse failure.
func parseFrontmatter(raw []byte) (frontmatterFields, error) {
	var f frontmatterFields
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return frontmatterFields{}, fmt.Errorf("%w: %w", ErrMalformedYAML, err)
	}
	return f, nil
}

// bodyParse walks the body line-by-line, splitting into:
//
//   - description: prose between frontmatter end and the first `## `
//     heading (with attachment refs already substituted).
//   - sectionLists: ordered list items under each canonical section.
//
// Returns wrapped ErrUnknownSection when a `## Heading` doesn't
// classify into the canonical set.
func bodyParse(ctx context.Context, store artifacts.ArtifactStore, src ImportSource, body []byte) (
	description string,
	sections map[canonicalSection][]string,
	imports ImportArtifacts,
	err error,
) {
	lines := splitLinesKeepEmpty(body)
	sections = make(map[canonicalSection][]string, 3)
	// Track section ordering for clean section-occurrence checks.
	seen := make(map[canonicalSection]bool, 3)

	// State machine:
	//   descMode -> the line stream feeds the description.
	//   sectionMode -> the line stream feeds the current section.
	// Blank lines are tolerated in both modes (they are separators);
	// a `## Heading` line transitions between modes by selecting the
	// next section. Prose (non-blank, non-list-item) inside a
	// section is rejected.
	var descBuilder strings.Builder
	var currentSection canonicalSection
	var inSection bool

	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return "", nil, ImportArtifacts{}, err
		}
		if strings.HasPrefix(line, "## ") {
			sec := classifySection(line)
			if sec == sectionUnknown {
				return "", nil, ImportArtifacts{}, fmt.Errorf("%w: %q",
					ErrUnknownSection, strings.TrimSpace(line))
			}
			if seen[sec] {
				return "", nil, ImportArtifacts{}, fmt.Errorf("%w: duplicate section %q",
					ErrUnknownSection, strings.TrimSpace(line))
			}
			seen[sec] = true
			currentSection = sec
			inSection = true
			continue
		}
		if !inSection {
			// Description mode. Accept everything verbatim.
			descBuilder.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				descBuilder.WriteByte('\n')
			}
			continue
		}
		// In a section. List items begin with "- ".
		trimmedRight := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmedRight, "- ") {
			item := strings.TrimPrefix(trimmedRight, "- ")
			sections[currentSection] = append(sections[currentSection], item)
			continue
		}
		if strings.TrimSpace(trimmedRight) == "" {
			// Blank line inside section is a separator; ignore. The
			// next `## ` heading or end-of-file ends the section.
			continue
		}
		// Anything else inside a section is rejected.
		return "", nil, ImportArtifacts{}, fmt.Errorf("%w: non-list-item line in section (%q)",
			ErrUnknownSection, trimmedRight)
	}

	// Normalise description: strip trailing blank lines and the
	// final newline. The Export side re-adds exactly one trailing
	// newline before the first section heading (or end-of-file).
	description = stripTrailingBlankLines(descBuilder.String())

	// Resolve attachments in the description (and section items).
	resolved, imports, resolveErr := resolveAttachments(ctx, store, src, description, sections)
	if resolveErr != nil {
		return "", nil, ImportArtifacts{}, resolveErr
	}
	description = resolved.description
	sections = resolved.sections
	return description, sections, imports, nil
}

// resolvedBody is the intermediate after attachment-substitution.
type resolvedBody struct {
	sections    map[canonicalSection][]string
	description string
}

// resolveAttachments walks the description + every section list
// item, replaces each `![alt](path)` with `![alt](artifact://<ID>)`,
// uploads the bytes via store.PutBytes, and records the mapping.
// Duplicate paths in one source -> ErrInvalidAttachmentRef.
func resolveAttachments(
	ctx context.Context,
	store artifacts.ArtifactStore,
	src ImportSource,
	description string,
	sections map[canonicalSection][]string,
) (resolvedBody, ImportArtifacts, error) {
	imports := ImportArtifacts{}
	pathIndex := make(map[string]struct{})

	subst := func(s string) (string, error) {
		// Walk image refs left-to-right; substitute each in turn.
		var result strings.Builder
		idx := 0
		matches := imageRefRegexp.FindAllStringSubmatchIndex(s, -1)
		for _, m := range matches {
			fullStart, fullEnd := m[0], m[1]
			pathStart, pathEnd := m[4], m[5]
			altStart, altEnd := m[2], m[3]
			path := s[pathStart:pathEnd]
			// Skip absolute / scheme URIs — they don't resolve to
			// filesystem paths and stay verbatim.
			if hasSchemeOrAbs(path) {
				result.WriteString(s[idx:fullEnd])
				idx = fullEnd
				continue
			}
			if _, dup := pathIndex[path]; dup {
				return "", fmt.Errorf("%w: duplicate attachment path %q",
					ErrInvalidAttachmentRef, path)
			}
			ref, err := uploadAttachment(ctx, store, src, path)
			if err != nil {
				return "", err
			}
			pathIndex[path] = struct{}{}
			imports.PathToRef = append(imports.PathToRef, AttachmentMapping{
				Path: path,
				Ref:  ref,
			})
			// Emit the substituted form.
			result.WriteString(s[idx:fullStart])
			result.WriteString("![")
			result.WriteString(s[altStart:altEnd])
			result.WriteString("](")
			result.WriteString(artifactScheme)
			result.WriteString(ref.ID)
			result.WriteString(")")
			idx = fullEnd
		}
		result.WriteString(s[idx:])
		return result.String(), nil
	}

	newDesc, err := subst(description)
	if err != nil {
		return resolvedBody{}, ImportArtifacts{}, err
	}

	newSections := make(map[canonicalSection][]string, len(sections))
	for sec, items := range sections {
		newItems := make([]string, len(items))
		for i, item := range items {
			ni, err := subst(item)
			if err != nil {
				return resolvedBody{}, ImportArtifacts{}, err
			}
			newItems[i] = ni
		}
		newSections[sec] = newItems
	}

	return resolvedBody{description: newDesc, sections: newSections}, imports, nil
}

// uploadAttachment reads the file under src.AllowedRoot and uploads
// it via store.PutBytes. Returns wrapped errors for path-safety
// rejection or filesystem failures.
func uploadAttachment(
	ctx context.Context,
	store artifacts.ArtifactStore,
	src ImportSource,
	relPath string,
) (artifacts.ArtifactRef, error) {
	safe, err := resolveSafePath(src.AllowedRoot, relPath)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	data, err := os.ReadFile(safe)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("importer: read attachment %q: %w", relPath, err)
	}
	opts := artifacts.PutOpts{
		MimeType:  "application/octet-stream",
		Filename:  filepath.Base(safe),
		Namespace: "skills-importer",
	}
	ref, err := store.PutBytes(ctx, src.Scope, data, opts)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("importer: upload attachment %q: %w", relPath, err)
	}
	return ref, nil
}

// hasSchemeOrAbs reports whether the path is absolute or carries a
// URI scheme — the importer skips those (they don't resolve to
// filesystem files). Recognised schemes: `http://`, `https://`,
// `data:`, `artifact://` (already-substituted on re-import).
func hasSchemeOrAbs(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	for _, prefix := range []string{"http://", "https://", "data:", "artifact://"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// splitLinesKeepEmpty splits b on '\n' but keeps the trailing
// newline on each line (except the last if it doesn't end with one).
// Used by bodyParse to preserve line boundaries.
func splitLinesKeepEmpty(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var lines []string
	start := 0
	for i := range b {
		if b[i] == '\n' {
			lines = append(lines, string(b[start:i+1]))
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, string(b[start:]))
	}
	return lines
}

// stripTrailingBlankLines removes the trailing blank-line(s) from s
// and the final newline. Used to normalise the parsed description
// so Export reproduces the source-side spacing.
func stripTrailingBlankLines(s string) string {
	for strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\r") {
		s = s[:len(s)-1]
		// Also strip trailing blank lines (newlines from blank lines
		// loop here).
		if strings.HasSuffix(s, "\n") {
			continue
		}
		break
	}
	return s
}

// slugify mirrors the predecessor's pack_loader._slugify shape:
// lowercase, alphanumeric, `-` between non-alphanumeric runs.
// Used for the Name fallback when frontmatter lacks `name`.
func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := b.String()
	for strings.HasSuffix(out, "-") {
		out = out[:len(out)-1]
	}
	return out
}

// nameFallbackFromHint slugifies PathHint's basename minus `.md`.
func nameFallbackFromHint(hint string) string {
	if hint == "" {
		return ""
	}
	base := filepath.Base(hint)
	base = strings.TrimSuffix(base, ".md")
	return slugify(base)
}

// doImport is the top-level Import pipeline. Split out from the
// method so it's testable without the importer struct.
func doImport(ctx context.Context, store artifacts.ArtifactStore, src ImportSource) (skills.Skill, ImportArtifacts, error) {
	if len(src.Bytes) == 0 {
		return skills.Skill{}, ImportArtifacts{}, fmt.Errorf("%w: empty source",
			ErrMissingFrontmatter)
	}
	rawFM, body, err := scanFrontmatter(src.Bytes)
	if err != nil {
		return skills.Skill{}, ImportArtifacts{}, err
	}
	fields, err := parseFrontmatter(rawFM.Bytes)
	if err != nil {
		return skills.Skill{}, ImportArtifacts{}, err
	}
	if strings.TrimSpace(fields.Trigger) == "" {
		return skills.Skill{}, ImportArtifacts{}, fmt.Errorf("%w: %w",
			ErrMissingTrigger, skills.ErrInvalidSkill)
	}
	description, sections, imports, err := bodyParse(ctx, store, src, body)
	if err != nil {
		return skills.Skill{}, ImportArtifacts{}, err
	}
	steps := sections[sectionSteps]
	if len(steps) == 0 {
		return skills.Skill{}, ImportArtifacts{}, fmt.Errorf("%w: %w",
			ErrEmptySteps, skills.ErrInvalidSkill)
	}
	name := fields.Name
	if strings.TrimSpace(name) == "" {
		name = nameFallbackFromHint(src.PathHint)
	}
	scope := skills.Scope(fields.Scope)
	if scope == "" {
		scope = skills.ScopeProject
	}
	skill := skills.Skill{
		Name:          name,
		Title:         fields.Title,
		Description:   description,
		Trigger:       fields.Trigger,
		TaskType:      fields.TaskType,
		Tags:          fields.Tags,
		Steps:         steps,
		Preconditions: sections[sectionPreconditions],
		FailureModes:  sections[sectionFailureModes],
		RequiredTools: fields.RequiredTools,
		RequiredNS:    fields.RequiredNamespaces,
		RequiredTags:  fields.RequiredTags,
		Origin:        skills.OriginPack,
		Scope:         scope,
		Extra: map[string]any{
			// Stash the raw frontmatter so Export reproduces byte-stable.
			"_importer.frontmatter_raw": string(rawFM.Bytes),
			// Stash the source hash so callers (e.g. attachment-scope
			// stamping in Phase 60+) can derive a stable task ID.
			"_importer.source_sha256": sourceHashHex(src.Bytes),
		},
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	if err := skill.Validate(); err != nil {
		return skills.Skill{}, ImportArtifacts{}, err
	}
	return skill, imports, nil
}

// sourceHashHex returns the sha256 hex of the source bytes. Used
// for stable task-id stamping on attachments.
func sourceHashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
