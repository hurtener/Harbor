// artifact_search is the LLM-facing grep over one stored session artifact.
// It finds lines in a large stored result (or an upload) so the model can
// read around them with artifact_fetch instead of paging the whole payload.
//
// Identity and isolation are artifact_fetch's: the store resolves on the
// `(tenant, user, session)` triple, and an out-of-scope ref is reported as
// not found. CPU and bytes are bounded: at most searchMaxScanBytes are
// scanned per call, patterns are RE2 (linear time) and size-capped, matches
// and context are capped, and long lines are clipped in the response.

package builtin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

const (
	searchMaxScanBytes     = 16 << 20
	searchMaxPatternBytes  = 512
	searchDefaultMatches   = 20
	searchMaxMatches       = 100
	searchDefaultContext   = 2
	searchMaxContext       = 10
	searchMaxLineBytes     = 512
	searchCancelCheckLines = 4096
)

// ArtifactSearchArgs is the typed input shape.
type ArtifactSearchArgs struct {
	// Ref is the artifact reference to search.
	Ref string `json:"ref"`
	// Pattern is the text to find. It is a literal string unless Regex is
	// true, in which case it is an RE2 regular expression.
	Pattern string `json:"pattern"`
	// Regex interprets Pattern as an RE2 regular expression.
	Regex bool `json:"regex,omitempty"`
	// CaseInsensitive matches without regard to letter case.
	CaseInsensitive bool `json:"case_insensitive,omitempty"`
	// ContextLines is how many lines before and after each match to return
	// (default 2, at most 10).
	ContextLines *int `json:"context_lines,omitempty"`
	// MaxMatches caps the number of matches returned (default 20, at most 100).
	MaxMatches int `json:"max_matches,omitempty"`
	// Offset is the byte offset to start searching from; pass a previous
	// response's next_offset to continue after a truncated search.
	Offset int64 `json:"offset,omitempty"`
}

// ArtifactSearchMatch is one matching line.
type ArtifactSearchMatch struct {
	// Line is the 1-based line number in the artifact.
	Line int `json:"line"`
	// ByteOffset is the byte offset of the line's start; pass it to
	// artifact_fetch as `offset` to read around the match.
	ByteOffset int64 `json:"byte_offset"`
	// Text is the matching line (clipped to 512 bytes).
	Text string `json:"text"`
	// Before and After are the surrounding context lines, oldest first.
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// ArtifactSearchOut is the typed return shape. Error is a soft-error
// channel the model reads as the observation.
type ArtifactSearchOut struct {
	Ref       string                `json:"ref"`
	MIME      string                `json:"mime,omitempty"`
	SizeBytes int64                 `json:"size_bytes"`
	Matches   []ArtifactSearchMatch `json:"matches,omitempty"`
	// Truncated reports that more matches may exist: the match cap was hit
	// or the scan stopped at its byte bound. Continue with NextOffset.
	Truncated bool `json:"truncated,omitempty"`
	// NextOffset is the byte offset to resume from when Truncated is true.
	NextOffset int64  `json:"next_offset,omitempty"`
	Error      string `json:"error,omitempty"`
}

func registerArtifactSearch(cat tools.ToolCatalog, store artifacts.ArtifactStore) error {
	return inproc.RegisterFunc[ArtifactSearchArgs, ArtifactSearchOut](
		cat, "artifact_search",
		func(ctx context.Context, args ArtifactSearchArgs) (ArtifactSearchOut, error) {
			return artifactSearch(ctx, store, args)
		},
		tools.WithDescription(
			"Search inside a stored session artifact (a large tool result or an upload) "+
				"for a text pattern, like grep. Use it before paging with artifact_fetch: "+
				"each match returns its 1-based `line`, its `byte_offset` (pass it to "+
				"artifact_fetch as `offset` to read around the match) and surrounding "+
				"context lines. `pattern` is a literal string by default; set `regex` "+
				"for an RE2 regular expression and `case_insensitive` to ignore case. "+
				"`context_lines` (default 2, max 10) and `max_matches` (default 20, max "+
				"100) bound the response; when `truncated` is true, call again with "+
				"`offset` set to `next_offset`. Works on text artifacts only (text/*, "+
				"JSON, XML, YAML, CSV); for a PDF, image or other binary artifact call "+
				"a tool that declares an artifact-reference parameter instead."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "artifact"),
	)
}

// textLikeMIME reports whether an artifact's media type is searchable text.
func textLikeMIME(mime string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch {
	case strings.HasPrefix(base, "text/"):
		return true
	case strings.HasSuffix(base, "+json"), strings.HasSuffix(base, "+xml"):
		return true
	}
	switch base {
	case "application/json", "application/x-ndjson", "application/ndjson", "application/xml",
		"application/yaml", "application/x-yaml", "application/javascript", "application/csv",
		"application/sql", "application/toml", "application/x-sh":
		return true
	}
	return false
}

func artifactSearch(ctx context.Context, store artifacts.ArtifactStore, args ArtifactSearchArgs) (ArtifactSearchOut, error) {
	if store == nil {
		return ArtifactSearchOut{}, errors.New("artifact_search: backing ArtifactStore is nil (operator misconfiguration: builtin.RegistryContext.ArtifactStore was not threaded)")
	}
	id, err := requireIdentity(ctx)
	if err != nil {
		return ArtifactSearchOut{}, err
	}
	args.Ref = strTrim(args.Ref)
	out := ArtifactSearchOut{Ref: args.Ref}
	switch {
	case args.Ref == "":
		out.Error = "artifact_search: ref field is empty"
		return out, nil
	case args.Pattern == "":
		out.Error = "artifact_search: pattern field is empty"
		return out, nil
	case len(args.Pattern) > searchMaxPatternBytes:
		out.Error = fmt.Sprintf("artifact_search: pattern is %d bytes; at most %d are allowed", len(args.Pattern), searchMaxPatternBytes)
		return out, nil
	case args.Offset < 0:
		out.Error = "artifact_search: offset must not be negative"
		return out, nil
	}
	match, perr := searchMatcher(args)
	if perr != nil {
		out.Error = perr.Error()
		return out, nil //nolint:nilerr // soft error the model reads
	}
	contextLines := searchDefaultContext
	if args.ContextLines != nil {
		contextLines = min(max(*args.ContextLines, 0), searchMaxContext)
	}
	maxMatches := searchDefaultMatches
	if args.MaxMatches > 0 {
		maxMatches = min(args.MaxMatches, searchMaxMatches)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	ref, found, err := store.GetRef(ctx, scope, args.Ref)
	if err != nil {
		out.Error = fmt.Sprintf("artifact_search: GetRef failed: %v", err)
		return out, nil
	}
	if !found {
		out.Error = fmt.Sprintf("artifact_search: ref %q not found in this session's scope", args.Ref)
		return out, nil
	}
	out.Ref, out.MIME, out.SizeBytes = ref.ID, ref.MimeType, ref.SizeBytes
	if !textLikeMIME(ref.MimeType) {
		out.Error = fmt.Sprintf("artifact_search: artifact %q (mime %q) is not text, so it cannot be searched. "+
			"To act on it, call a tool that declares an artifact-reference parameter and pass it %q.", ref.ID, ref.MimeType, ref.ID)
		return out, nil
	}
	blob, found, err := store.Get(ctx, scope, ref.ID)
	if err != nil {
		out.Error = fmt.Sprintf("artifact_search: Get failed: %v", err)
		return out, nil
	}
	if !found {
		out.Error = fmt.Sprintf("artifact_search: ref %q disappeared during search", args.Ref)
		return out, nil
	}
	return searchBlob(ctx, out, blob, args.Offset, match, contextLines, maxMatches)
}

func searchMatcher(args ArtifactSearchArgs) (func(string) bool, error) {
	if args.Regex {
		expr := args.Pattern
		if args.CaseInsensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("artifact_search: invalid RE2 pattern: %w", err)
		}
		return re.MatchString, nil
	}
	if args.CaseInsensitive {
		needle := strings.ToLower(args.Pattern)
		return func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }, nil
	}
	return func(line string) bool { return strings.Contains(line, args.Pattern) }, nil
}

// searchBlob scans lines from offset, bounded by searchMaxScanBytes. Line
// numbers count from the start of the artifact, so an offset continuation
// still reports absolute lines.
func searchBlob(ctx context.Context, out ArtifactSearchOut, blob []byte, offset int64, match func(string) bool, contextLines, maxMatches int) (ArtifactSearchOut, error) {
	total := int64(len(blob))
	if offset >= total {
		return out, nil
	}
	// Align the start to a line boundary at or after offset; count the lines
	// before it for absolute numbering.
	start := offset
	if start > 0 && blob[start-1] != '\n' {
		next := indexByteFrom(blob, start, '\n')
		if next < 0 {
			return out, nil
		}
		start = next + 1
	}
	lineNo := 1 + countNewlines(blob[:start])
	limit := min(total, start+searchMaxScanBytes)
	var before []string
	var pending []int // indices of matches still collecting After context
	pos := start
	for scanned := 0; pos < limit; scanned++ {
		if scanned%searchCancelCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return ArtifactSearchOut{}, err
			}
		}
		end := indexByteFrom(blob, pos, '\n')
		next := end + 1
		if end < 0 {
			end, next = total, total
		}
		line := clipLine(blob[pos:end])
		kept := pending[:0]
		for _, i := range pending {
			out.Matches[i].After = append(out.Matches[i].After, line)
			if len(out.Matches[i].After) < contextLines {
				kept = append(kept, i)
			}
		}
		pending = kept
		if match(line) {
			if len(out.Matches) == maxMatches {
				out.Truncated, out.NextOffset = true, pos
				return out, nil
			}
			out.Matches = append(out.Matches, ArtifactSearchMatch{Line: lineNo, ByteOffset: pos, Text: line, Before: append([]string(nil), before...)})
			if contextLines > 0 {
				pending = append(pending, len(out.Matches)-1)
			}
		}
		if contextLines > 0 {
			before = append(before, line)
			if len(before) > contextLines {
				before = before[1:]
			}
		}
		lineNo++
		pos = next
	}
	if pos < total {
		out.Truncated, out.NextOffset = true, pos
	}
	return out, nil
}

func indexByteFrom(b []byte, from int64, c byte) int64 {
	for i := from; i < int64(len(b)); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// clipLine renders one line as valid text of at most searchMaxLineBytes.
func clipLine(b []byte) string {
	b = []byte(strings.TrimSuffix(string(b), "\r"))
	clipped := len(b) > searchMaxLineBytes
	if clipped {
		cut := searchMaxLineBytes
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		b = b[:cut]
	}
	s := strings.ToValidUTF8(string(b), "�")
	if clipped {
		s += "…"
	}
	return s
}
