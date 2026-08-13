package skillpkg

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// refs.go — the ordinary-Markdown support-reference scanner.
//
// A complete skill package's logical body may reference its support
// files with the ordinary Markdown link/image forms: inline
// `[text](dest)` / `![alt](dest)`, reference-style
// `[text][label]` / `![alt][label]` (plus the collapsed `[x][]` and
// the shortcut `[x]` / `![x]` forms), and reference definitions
// `[label]: dest`. ScanSupportRefs parses ALL of these — not a
// bespoke image-only syntax — and resolves each occurrence to its
// destination, returning the byte span that materialization rewrites
// (the inline destination or the definition's destination).
//
// Classification helpers (SplitDest / CanonicalizeSupportDest) turn a
// raw destination into the reference class the importer's policy
// applies: scheme / absolute / fragment-only / ambiguous destinations
// are distinguished from the relative package paths that must exist
// in the manifest.
//
// Boundedness: the scanner is a single linear pass over the input
// with fixed-size regexp matches; no recursion, no unbounded
// expansion. Fenced code blocks (``` and ~~~) are skipped so a code
// example cannot smuggle a false reference. Inline code spans and
// indented code blocks are NOT fence-aware (matching the file-import
// path's scanner); refs inside them are treated as body refs.

// SupportRefKind classifies a support reference occurrence.
type SupportRefKind int

const (
	// SupportRefLink — an ordinary link (`[text](dest)`,
	// `[text][label]`, shortcut `[text]`, or a definition used as a
	// link).
	SupportRefLink SupportRefKind = iota
	// SupportRefImage — an image (`![alt](dest)`, `![alt][label]`,
	// shortcut `![alt]`, or a definition used as an image).
	SupportRefImage
)

// SupportRef is one resolved support-reference occurrence.
type SupportRef struct {
	// Kind is the reference class (link or image).
	Kind SupportRefKind
	// Dest is the resolved destination text: the raw inline
	// destination, or the destination of the definition the
	// reference-style usage resolved through.
	Dest string
	// Start/End bound the rewrite-able destination span in the
	// scanned text: the inline destination inside `(...)` or the
	// definition's destination token. Both zero for reference-style
	// USAGES — their destination lives in the definition, which
	// carries the span.
	Start, End int
	// InDefinition reports whether Dest is a reference-definition
	// destination (`[label]: dest`) rather than an inline one.
	InDefinition bool
}

// DestKind classifies a raw Markdown destination for the package
// reference policy.
type DestKind int

const (
	// DestRelative — a relative path (optionally with a trailing
	// `#fragment` anchor). This is the only class that can name a
	// manifest entry.
	DestRelative DestKind = iota
	// DestScheme — a URI scheme (`http:`, `data:`, `mailto:`, ...).
	DestScheme
	// DestAbsolute — a `/`-leading absolute path.
	DestAbsolute
	// DestFragment — a `#fragment`-only document anchor.
	DestFragment
	// DestAmbiguous — a destination that cannot name a manifest entry
	// (empty, backslash, query-bearing, or otherwise unsupported).
	DestAmbiguous
)

// Support-ref sentinel errors (materialization / dematerialization /
// resolution). Compare via errors.Is.
var (
	// ErrSupportRefDangling — a relative support reference (or a
	// resolved URI path) does not name an entry of the package's
	// support manifest.
	ErrSupportRefDangling = errors.New("skillpkg: support reference not present in the package manifest")
	// ErrSupportRefForeignURI — a `skillpkg://` URI in a body carries
	// a package hash other than the exact package being
	// dematerialized or resolved.
	ErrSupportRefForeignURI = errors.New("skillpkg: support URI belongs to a different package")
	// ErrSupportRefMalformedURI — a `skillpkg://` token in a body is
	// not a parseable package support URI.
	ErrSupportRefMalformedURI = errors.New("skillpkg: malformed support URI in body")
)

// span is a byte range in the scanned text.
type span struct{ start, end int }

// refDefinition is one `[label]: dest` definition.
type refDefinition struct {
	// dest is the definition's destination token text.
	dest string
	// destStart/destEnd bound the destination token (or its
	// angle-bracket contents) for rewriting.
	destStart, destEnd int
}

// defEntry pairs a folded label with its definition, in line order.
type defEntry struct {
	label string
	def   refDefinition
}

// ScanSupportRefs parses the ordinary Markdown link/image references
// in text (a logical package body) and returns every resolved
// occurrence: inline refs, reference-style usages (resolved through
// their definitions), and the definitions themselves. Reference
// labels are case-insensitive and whitespace-collapsed per CommonMark.
// Undefined reference labels render literally in Markdown, so they
// are NOT returned — only occurrences that resolve to a destination.
// The returned slice is deterministic (definitions appear in line
// order).
func ScanSupportRefs(text string) []SupportRef {
	var out []SupportRef
	entries := scanDefinitions(text)
	defs := make(map[string]refDefinition, len(entries))
	for _, e := range entries {
		defs[e.label] = e.def
	}
	// usageKinds records, per folded label, whether any usage of the
	// definition resolved as an image (the definition itself is then
	// classified as an image — the stricter class).
	usageKinds := map[string]SupportRefKind{}

	var occupied []span
	// Fenced code blocks (``` / ~~~) are not reference content.
	fences := fencedSpans(text)
	inFence := func(start int) bool {
		for _, f := range fences {
			if start >= f.start && start < f.end {
				return true
			}
		}
		return false
	}

	// 1. Inline links and images: (!?)[...](dest).
	for _, m := range inlineRefRe.FindAllStringSubmatchIndex(text, -1) {
		if inFence(m[0]) {
			continue
		}
		isImage := m[2] < m[3]
		destText := text[m[6]:m[7]]
		dest, destStart, destEnd := extractInlineDest(destText, m[6])
		out = append(out, SupportRef{
			Kind:  refKind(isImage),
			Dest:  dest,
			Start: destStart,
			End:   destEnd,
		})
		occupied = append(occupied, span{m[0], m[1]})
	}

	// 2. Reference usages: (!?)[...][label] and the collapsed
	//    (!?)[...][] form (the label is then the text).
	for _, m := range refUsageRe.FindAllStringSubmatchIndex(text, -1) {
		if inFence(m[0]) {
			continue
		}
		isImage := m[2] < m[3]
		textLabel := text[m[4]:m[5]]
		refLabel := strings.TrimSpace(text[m[6]:m[7]])
		label := foldLabel(textLabel)
		if refLabel != "" {
			label = foldLabel(refLabel)
		}
		def, ok := defs[label]
		if !ok {
			continue // undefined label renders literally — not a support ref
		}
		out = append(out, SupportRef{
			Kind: refKind(isImage),
			Dest: def.dest,
		})
		occupied = append(occupied, span{m[0], m[1]})
		if isImage {
			usageKinds[label] = SupportRefImage
		}
	}

	// 3. Shortcut references: [text] / ![alt] — a reference only when
	//    the label is defined, and never when the match is part of a
	//    larger construct or a definition line.
	for _, m := range shortcutRe.FindAllStringSubmatchIndex(text, -1) {
		if inFence(m[0]) {
			continue
		}
		if containsSpan(occupied, m[0]) {
			continue
		}
		if m[1] < len(text) {
			switch text[m[1]] {
			case ':', '(', '[':
				continue // definition, inline, or reference construct
			}
		}
		isImage := m[2] < m[3]
		label := foldLabel(text[m[4]:m[5]])
		def, ok := defs[label]
		if !ok {
			continue
		}
		out = append(out, SupportRef{
			Kind: refKind(isImage),
			Dest: def.dest,
		})
		occupied = append(occupied, span{m[0], m[1]})
		if isImage {
			usageKinds[label] = SupportRefImage
		}
	}

	// 4. Definitions themselves (so unused definitions are validated,
	//    and materialization rewrites the definition destinations), in
	//    deterministic line order.
	for _, e := range entries {
		out = append(out, SupportRef{
			Kind:         definitionKind(usageKinds[e.label]),
			Dest:         e.def.dest,
			Start:        e.def.destStart,
			End:          e.def.destEnd,
			InDefinition: true,
		})
	}
	return out
}

// definitionKind picks a definition's reference kind: an image when
// any usage of the label resolved as an image (the stricter class),
// otherwise a link (the lenient default for an unused definition).
func definitionKind(usage SupportRefKind) SupportRefKind {
	if usage == SupportRefImage {
		return SupportRefImage
	}
	return SupportRefLink
}

// refKind maps the `!` flag to the reference kind.
func refKind(isImage bool) SupportRefKind {
	if isImage {
		return SupportRefImage
	}
	return SupportRefLink
}

// inlineRefRe matches `[text](dest)` and `![alt](dest)` on one line.
// Group 1 is the optional `!`, group 2 the text/alt, group 3 the
// destination.
var inlineRefRe = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\(([^)\n]+)\)`)

// refUsageRe matches `[text][label]` and `![alt][label]` (and the
// collapsed `[text][]` / `![alt][]` forms, where group 3 is empty).
var refUsageRe = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\[([^\]\n]*)\]`)

// shortcutRe matches a bare `[text]` or `![alt]`.
var shortcutRe = regexp.MustCompile(`(!?)\[([^\]\n]*)\]`)

// extractInlineDest isolates the destination token of an inline
// reference. The captured span covers the whole destination text; an
// angle-bracket destination (`<dest>`) narrows the span to its
// contents, and a trailing title (`dest "title"`) narrows it to the
// token. A destination with a bare space that is NOT a title is left
// whole (the canonicalizer rejects it — no partial rewrite).
func extractInlineDest(destText string, base int) (dest string, start, end int) {
	start, end = base, base+len(destText)
	switch {
	case strings.HasPrefix(destText, "<"):
		if ci := strings.IndexByte(destText, '>'); ci >= 0 {
			return destText[1:ci], base + 1, base + ci
		}
		return destText, start, end
	default:
		if ws := strings.IndexAny(destText, " \t"); ws >= 0 {
			rest := strings.TrimLeft(destText[ws:], " \t")
			if rest != "" && (rest[0] == '"' || rest[0] == '\'' || rest[0] == '(') {
				return destText[:ws], start, base + ws
			}
		}
		return destText, start, end
	}
}

// scanDefinitions finds every `[label]: dest` definition line and
// returns the entries in line order. A definition's destination is the
// first token after the colon (or the angle-bracket contents when the
// destination is `<dest>`); a trailing title (`"..."`, `'...'`,
// `(...)`) is not part of the destination.
func scanDefinitions(text string) []defEntry {
	var defs []defEntry
	for lineStart := 0; lineStart <= len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		var raw string
		var next int
		if lineEnd < 0 {
			raw = text[lineStart:]
			next = len(text) + 1
		} else {
			raw = text[lineStart : lineStart+lineEnd]
			next = lineStart + lineEnd + 1
		}
		trimmed := strings.TrimLeft(raw, " \t")
		lead := len(raw) - len(trimmed)
		// Find the definition's closing bracket and the mandatory ':'.
		cb := strings.IndexByte(trimmed, ']')
		if cb <= 0 || trimmed[0] != '[' || cb+1 >= len(trimmed) || trimmed[cb+1] != ':' {
			lineStart = next
			continue
		}
		label := foldLabel(trimmed[1:cb])
		pos := cb + 2
		for pos < len(trimmed) && (trimmed[pos] == ' ' || trimmed[pos] == '\t') {
			pos++
		}
		abs := func(p int) int { return lineStart + lead + p }
		if pos >= len(trimmed) {
			defs = append(defs, defEntry{label: label, def: refDefinition{dest: "", destStart: abs(pos), destEnd: abs(pos)}})
			lineStart = next
			continue
		}
		if trimmed[pos] == '<' {
			if end := strings.IndexByte(trimmed[pos+1:], '>'); end >= 0 {
				defs = append(defs, defEntry{label: label, def: refDefinition{
					dest:      trimmed[pos+1 : pos+1+end],
					destStart: abs(pos + 1),
					destEnd:   abs(pos + 1 + end),
				}})
				lineStart = next
				continue
			}
		}
		end := pos
		for end < len(trimmed) && trimmed[end] != ' ' && trimmed[end] != '\t' {
			end++
		}
		defs = append(defs, defEntry{label: label, def: refDefinition{
			dest:      trimmed[pos:end],
			destStart: abs(pos),
			destEnd:   abs(end),
		}})
		lineStart = next
	}
	// Deduplicate: CommonMark's last definition of a label wins; the
	// entry keeps its first position so the result stays deterministic.
	last := make(map[string]refDefinition, len(defs))
	for _, e := range defs {
		last[e.label] = e.def
	}
	seen := make(map[string]bool, len(defs))
	var out []defEntry
	for _, e := range defs {
		if seen[e.label] {
			continue
		}
		seen[e.label] = true
		out = append(out, defEntry{label: e.label, def: last[e.label]})
	}
	return out
}

// foldLabel is the CommonMark label fold: lowercase, whitespace
// collapsed.
func foldLabel(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// containsSpan reports whether start falls inside one of the occupied
// ranges.
func containsSpan(occupied []span, start int) bool {
	for _, s := range occupied {
		if start >= s.start && start < s.end {
			return true
		}
	}
	return false
}

// fencedSpans returns the byte ranges covered by fenced code blocks
// (lines starting with up to three spaces then ``` or ~~~).
func fencedSpans(text string) []span {
	var out []span
	var fence string
	var open int
	offset := 0
	for _, line := range strings.Split(text, "\n") {
		lineLen := len(line)
		if lineLen > 0 {
			lineLen++ // account for the stripped '\n' unless last
		}
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		var marker string
		if indent <= 3 && len(trimmed) >= 3 {
			switch trimmed[0] {
			case '`':
				marker = "```"
			case '~':
				marker = "~~~"
			}
		}
		if marker != "" && strings.HasPrefix(trimmed, marker) {
			if fence == "" {
				fence = marker
				open = offset
			} else if marker == fence {
				out = append(out, span{open, offset + lineLen})
				fence = ""
			}
		}
		offset += lineLen
	}
	if fence != "" { // unterminated fence closes at EOF
		out = append(out, span{open, offset})
	}
	return out
}

// SplitDest classifies a raw Markdown destination into its reference
// class, returning the path portion (everything before a `#` fragment)
// and the fragment for DestRelative.
func SplitDest(dest string) (DestKind, string, string) {
	if dest == "" {
		return DestAmbiguous, "", ""
	}
	if strings.HasPrefix(dest, "/") {
		return DestAbsolute, "", ""
	}
	if strings.HasPrefix(dest, "#") {
		return DestFragment, "", dest[1:]
	}
	if strings.ContainsRune(dest, '\\') {
		return DestAmbiguous, "", ""
	}
	if strings.ContainsRune(dest, '?') {
		return DestAmbiguous, "", ""
	}
	if i := strings.IndexByte(dest, ':'); i > 0 && !strings.ContainsRune(dest[:i], '/') && isSchemeName(dest[:i]) {
		return DestScheme, "", ""
	}
	if i := strings.IndexByte(dest, '#'); i >= 0 {
		if i == 0 {
			return DestFragment, "", dest[1:]
		}
		return DestRelative, dest[:i], dest[i+1:]
	}
	return DestRelative, dest, ""
}

func isSchemeName(s string) bool {
	if s == "" || !isSchemeAlpha(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '+', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

func isSchemeAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// CanonicalizeSupportDest normalizes a relative destination's path
// portion into the canonical manifest-path form, or returns an error
// when the destination cannot name a manifest entry: empty, absolute,
// backslash-bearing, `.`-only, or escaping the package root
// (`..` after cleaning), or outside the canonical path charset.
func CanonicalizeSupportDest(dest string) (string, error) {
	if dest == "" {
		return "", errors.New("empty reference")
	}
	if strings.HasPrefix(dest, "/") {
		return "", errors.New("not a root-relative path")
	}
	if strings.ContainsRune(dest, '\\') {
		return "", errors.New("contains a backslash")
	}
	cleaned := path.Clean(strings.TrimPrefix(dest, "./"))
	switch {
	case cleaned == ".":
		return "", errors.New("reference resolves to the package root")
	case cleaned == ".." || strings.HasPrefix(cleaned, "../"):
		return "", errors.New("reference escapes the package root")
	}
	if _, err := canonicalizePath(cleaned); err != nil {
		return "", fmt.Errorf("not a canonical package path: %v", err)
	}
	return cleaned, nil
}
