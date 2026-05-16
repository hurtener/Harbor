// cmd/harbor/cmd_validate.go — `harbor validate` (Phase 68, D-088).
//
// Pre-boot tool that runs the in-process `internal/config` validator
// against a Harbor config file. Surfaces each Phase 02 rule as a
// stable, golden-pinnable error with file:line precision. Suitable
// as a CI pre-flight gate.
//
// Wire shape (`--json`):
//
//	{"error": "<summary>", "code": "validation_failed", "hint": "<hint>",
//	 "errors": [{"category": "<category>", "file": "<file>", "line": <n>,
//	             "message": "<msg>", "hint": "<hint>"}]}
//
// Human shape (default):
//
//	Error: harbor validate: <summary> (<hint>)
//	<file>:<line>: <category>: <message> (<hint>)
//	...
//
// Exit codes (pinned by D-088):
//   - 0 — valid.
//   - 1 — validation errors found (one or more entries in `errors[]`).
//   - 2 — unexpected / internal error (I/O, unsupported input).
//
// The subcommand does NOT boot the Runtime, the Protocol, the LLM
// client, or any external dependency. It only reads the file and runs
// the validator.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/config"
)

// Phase 68 — stable CLI error codes for the validate subcommand. New
// categories ADD entries here; existing entries are wire contracts
// pinned by golden tests.
const (
	// CodeValidationFailed is emitted when at least one validator rule
	// fired. Exit 1. The `errors[]` array carries the individual
	// findings.
	CodeValidationFailed = "validation_failed"
	// CodeValidationInternal is emitted on unrecoverable / unexpected
	// failures (read error, internal bug). Exit 2.
	CodeValidationInternal = "validation_internal_error"
)

// Validation error categories. These strings are wire-stable and are
// pinned by golden tests; renaming one is a breaking change.
const (
	// CategoryConfigParse — YAML parse / strict-decode failure (an
	// unknown field, a malformed document). Pre-validator.
	CategoryConfigParse = "config.parse"
	// CategoryConfigSemantic — a Phase 02 `validateXxx` rule fired
	// (bad enum, missing required, out-of-range numeric).
	CategoryConfigSemantic = "config.semantic"
	// CategoryIONotFound — the requested file does not exist.
	CategoryIONotFound = "io.not_found"
	// CategoryIORead — read failure other than "not found".
	CategoryIORead = "io.read"
)

// defaultConfigPath is the path `harbor validate` walks when no
// argument is supplied. `examples/harbor.yaml` is the canonical
// example shape; a real project keeps the same name at the repo root.
const defaultConfigPath = "harbor.yaml"

// validationFinding is one row in the `errors[]` array on the --json
// wire. The fields are wire-stable and pinned by golden tests.
type validationFinding struct {
	// Category names the failure class. One of CategoryConfig*,
	// CategoryIO* — see the const block above.
	Category string `json:"category"`
	// File is the source path the finding refers to. Empty when the
	// finding is not file-scoped (rare; today every finding carries
	// a file).
	File string `json:"file"`
	// Line is the 1-indexed YAML line the finding refers to. Zero
	// when the failure is "field missing" (no token to point at) or
	// when the parser did not produce a line. Operators reading 0
	// look at the named field path in Message and grep their YAML.
	Line int `json:"line"`
	// Message is the human-readable finding. Stable — pinned by
	// golden tests.
	Message string `json:"message"`
	// Hint is an optional follow-up: "see RFC §10", a doc link, etc.
	// Omitted from the wire when empty.
	Hint string `json:"hint,omitempty"`
}

// validationBody is the wire shape `harbor validate --json` emits when
// findings exist. Embeds the standard CLIError fields so the wire
// shape stays compatible with the Phase 63 contract — the new
// `errors` field is additive.
//
// Field order matches encoding/json's struct-definition order so the
// wire layout is stable for golden comparison.
type validationBody struct {
	Error  string              `json:"error"`
	Code   string              `json:"code"`
	Hint   string              `json:"hint,omitempty"`
	Errors []validationFinding `json:"errors"`
}

// validateFlags captures the parsed cobra flags. Built once per
// invocation; no mutable state survives between commands.
type validateFlags struct {
	jsonMode bool
	quiet    bool
}

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate [path]",
		Short: "validate config / skills / agent definitions without booting",
		Long: `Validate a Harbor project's config without booting the Runtime.

Each finding carries a stable category and (where the YAML AST resolves
the field) a file:line. Suitable as a CI pre-flight check.

With no argument, ` + "`harbor validate`" + ` reads ` + "`harbor.yaml`" + ` from the working
directory.

Exit codes:

  0  valid
  1  validation errors found
  2  unexpected / internal error (I/O, unsupported input)

Phase 68 ships config validation. Standalone skill / agent-definition
validation lands when those surfaces gain file-based primitives — see
RFC §8 + the Phase 68 plan in docs/plans/.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runValidate,
	}
	return cmd
}

// runValidate is the cobra RunE. It resolves the path, runs the
// pipeline (read → parse-for-AST → load+validate → derive findings),
// and prints the result. The function returns a CLIError on any
// non-zero exit; cobra surfaces it via the root's error sink.
//
// Exit-code convention:
//   - return nil → cobra exits 0 (valid).
//   - return CLIError{Code: CodeValidationFailed} → cobra exits 1.
//   - return CLIError{Code: CodeValidationInternal} → cobra exits 2.
//
// The two codes map to two distinct CLIError values so the existing
// `emitCLIError` / `PrintCLIError` plumbing surfaces them uniformly
// in both human + --json modes. Phase 63's main() exits with a
// non-zero exit code when Execute returns any error; the exit-2
// distinction lives in the CLIError.Code field that callers read.
func runValidate(cmd *cobra.Command, args []string) error {
	flags := validateFlags{
		jsonMode: resolveJSONMode(cmd),
		quiet:    resolveQuietMode(cmd),
	}
	path := defaultConfigPath
	if len(args) == 1 {
		path = args[0]
	}

	findings, internalErr := validatePath(cmd.Context(), path)
	switch {
	case internalErr != nil:
		// I/O or internal failure. Emit a single finding and exit 2.
		// Wrap as CLIError so the structured-error sink owns the
		// rendering; the body carries the originating category /
		// line via a single-entry `errors[]` array.
		finding := internalFindingFor(path, internalErr)
		return emitValidationResult(cmd, flags, path,
			[]validationFinding{finding},
			CodeValidationInternal,
			finding.Message,
			"check the file path and permissions; the file must be a YAML config",
		)
	case len(findings) > 0:
		summary := fmt.Sprintf("%d validation error", len(findings))
		if len(findings) != 1 {
			summary += "s"
		}
		summary += " in " + path
		return emitValidationResult(cmd, flags, path, findings,
			CodeValidationFailed, summary,
			"see RFC §10 (Configuration) and CLAUDE.md §10",
		)
	}

	// Valid. Emit a one-line confirmation in human mode (suppressed
	// by --quiet); --json mode emits a structured success body so
	// scripts can `jq '.code'` uniformly.
	if flags.jsonMode {
		body := struct {
			OK   bool   `json:"ok"`
			File string `json:"file"`
		}{OK: true, File: path}
		buf, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return emitValidationResult(cmd, flags, path,
				[]validationFinding{{Category: CategoryConfigParse, File: path, Message: fmt.Sprintf("marshal success body: %v", marshalErr)}},
				CodeValidationInternal, "marshal success body failed",
				"this is a Harbor internal bug; please file an issue",
			)
		}
		if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), string(buf)); writeErr != nil {
			return fmt.Errorf("validate: write success body: %w", writeErr)
		}
		return nil
	}
	if !flags.quiet {
		if _, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", path); writeErr != nil {
			return fmt.Errorf("validate: write success line: %w", writeErr)
		}
	}
	return nil
}

// validatePath is the core pipeline. It reads `path`, runs the Phase 02
// loader, derives a (category, line) per finding, and returns the
// list. A non-nil `internalErr` indicates an I/O or read failure that
// short-circuits the pipeline — the caller surfaces this as a single
// internal-class finding with exit code 2.
//
// Findings ordered as the loader surfaces them (Phase 02 is single-
// finding today — its `Validate()` returns the first error it hits;
// a future enhancement that gathers all errors will round-trip through
// this slice).
func validatePath(ctx context.Context, path string) (findings []validationFinding, internalErr error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// I/O failures (including file-not-found) are internal — exit
		// 2, not 1. The caller decides which exit code to surface
		// via the validation_internal_error code path. We don't wrap
		// the path into the error string — `internalFindingFor`
		// already includes it as the finding's File field, and
		// `os.ReadFile`'s default error already includes the path.
		return nil, err
	}

	// Run the Phase 02 loader. We use LoadFromBytes (NOT Load) so we
	// retain the byte stream for AST-based line lookup. The loader's
	// own error formatting carries the field path; we parse it back
	// into a category + line.
	_, loaderErr := config.LoadFromBytes(ctx, data)
	if loaderErr == nil {
		return nil, nil
	}

	finding := classifyLoaderError(loaderErr, path, data)
	return []validationFinding{finding}, nil
}

// classifyLoaderError converts a Phase 02 loader error into a stable
// validation finding. The two categories the loader produces are:
//
//   - CategoryConfigParse — YAML parse / strict-decode failure. The
//     wrapped error string contains ": parse: [<line>:<col>] ...".
//     We surface the inner reason as the finding's Message (stripped
//     of multi-line excerpt) and the parsed line.
//   - CategoryConfigSemantic — a `validateXxx` rule fired. The
//     wrapped error string follows
//     "config: invalid configuration: config.<dotted.path>: <reason> (source: <name>)".
//     We derive the line by parsing the YAML AST and looking up the
//     dotted path; missing-field errors fall back to line=0.
//
// The classification is governed by the presence of the ": parse: "
// segment in the wrapped error chain — a stable marker produced by
// `internal/config/loader.go::loadFromBytesNamed`.
func classifyLoaderError(err error, path string, data []byte) validationFinding {
	msg := err.Error()
	finding := validationFinding{File: path}

	if strings.Contains(msg, ": parse: ") {
		// Parse / strict-decode failure.
		finding.Category = CategoryConfigParse
		finding.Line = parseLineFromGoccyMessage(msg)
		finding.Message = extractParseReason(msg)
		finding.Hint = "fix the YAML at the indicated line"
		return finding
	}

	// Semantic path.
	finding.Category = CategoryConfigSemantic
	finding.Message = stripErrorWrappers(msg)
	fieldPath := extractFieldPath(msg)
	if fieldPath != "" {
		if line := lineForFieldPath(data, fieldPath); line > 0 {
			finding.Line = line
		}
	}
	finding.Hint = hintForSemanticField(fieldPath)
	return finding
}

// stripErrorWrappers turns the loader's wrapped error string into a
// shorter, golden-pinnable message. The loader composes its error as:
//
//	"config: invalid configuration: config.<path>: <reason> (source: <name>)"
//
// We drop the leading "config: invalid configuration: " prefix and
// the trailing "(source: ...)" annotation so the message is just the
// field + reason — what an operator needs.
func stripErrorWrappers(msg string) string {
	const prefix = "config: invalid configuration: "
	cleaned := strings.TrimPrefix(msg, prefix)
	if i := strings.LastIndex(cleaned, " (source:"); i > 0 {
		cleaned = cleaned[:i]
	}
	return strings.TrimSpace(cleaned)
}

// extractParseReason pulls the goccy parse-error reason out of a
// loader-wrapped message. goccy's full Error() includes a multi-line
// excerpt of the offending YAML; for an operator-facing CLI we want
// only the first line ("[L:C] reason").
//
// Input shape (single string with embedded newlines):
//
//	"config: invalid configuration: <source>: parse: [2:3] reason\n   1 | ...\n>  2 | ...\n         ^\n"
//
// Output: "[2:3] reason".
func extractParseReason(msg string) string {
	const marker = ": parse: "
	i := strings.Index(msg, marker)
	if i < 0 {
		return stripErrorWrappers(msg)
	}
	tail := msg[i+len(marker):]
	// Take only the first line — drop the YAML excerpt.
	if nl := strings.IndexByte(tail, '\n'); nl > 0 {
		tail = tail[:nl]
	}
	return strings.TrimSpace(tail)
}

// extractFieldPath pulls the dotted field path from a Phase 02
// semantic error. The error format is `config.<path>: <reason>...`.
// Returns "" when the path can't be extracted (defensive — should not
// happen on well-formed loader errors).
func extractFieldPath(msg string) string {
	cleaned := stripErrorWrappers(msg)
	// `cleaned` now starts with "config.<path>: <reason>".
	if !strings.HasPrefix(cleaned, "config.") {
		return ""
	}
	rest := strings.TrimPrefix(cleaned, "config.")
	if i := strings.Index(rest, ":"); i > 0 {
		return rest[:i]
	}
	return ""
}

// hintForSemanticField returns a category-specific hint for a
// semantic finding. Stable across releases. Generic when the field is
// unrecognised.
func hintForSemanticField(field string) string {
	if field == "" {
		return "consult RFC §10 and CLAUDE.md §10"
	}
	switch {
	case strings.HasPrefix(field, "llm."):
		return "see examples/harbor.yaml under `llm:`"
	case strings.HasPrefix(field, "identity."):
		return "see examples/harbor.yaml under `identity:`"
	case strings.HasPrefix(field, "state."):
		return "see examples/harbor.yaml under `state:`"
	case strings.HasPrefix(field, "events."):
		return "see examples/harbor.yaml under `events:`"
	case strings.HasPrefix(field, "server."):
		return "see examples/harbor.yaml under `server:`"
	case strings.HasPrefix(field, "telemetry."):
		return "see examples/harbor.yaml under `telemetry:`"
	default:
		return "see examples/harbor.yaml for the canonical shape"
	}
}

// parseLineFromGoccyMessage scans a goccy/go-yaml error message for
// the canonical `[<line>:<col>]` location marker and returns the
// line. Returns 0 when no marker is present.
//
// goccy's error format example:
//
//	"[5:7] unknown field \"x\""
//
// We accept any prefix-context, scan for the first '[', and parse.
func parseLineFromGoccyMessage(msg string) int {
	// Find the first '[N:M]' triple. goccy emits this verbatim.
	for i := 0; i < len(msg); i++ {
		if msg[i] != '[' {
			continue
		}
		// Look for the matching colon + bracket.
		end := strings.IndexByte(msg[i:], ']')
		if end < 0 {
			continue
		}
		span := msg[i+1 : i+end]
		colon := strings.IndexByte(span, ':')
		if colon < 1 {
			continue
		}
		line := 0
		for _, c := range span[:colon] {
			if c < '0' || c > '9' {
				line = 0
				break
			}
			line = line*10 + int(c-'0')
		}
		if line > 0 {
			return line
		}
	}
	return 0
}

// lineForFieldPath parses `data` as YAML and walks the AST to find
// the dotted field path. Returns the 1-indexed line of the matched
// mapping-value node, or 0 when the path does not resolve (missing
// key — the operator's error IS "must not be empty").
//
// Path segments are split on "." and matched against mapping keys.
// Indexed segments like `model_profiles[\"x\"]` or
// `model_profiles["x"]` are normalised to the bracketed key value
// before lookup. Unbracketed indices (`tools.mcp_servers[0]`) walk
// into sequence nodes by index.
func lineForFieldPath(data []byte, dottedPath string) int {
	file, err := parser.ParseBytes(data, 0)
	if err != nil {
		return 0
	}
	for _, doc := range file.Docs {
		if doc == nil || doc.Body == nil {
			continue
		}
		root, ok := mappingFromForCmd(doc.Body)
		if !ok {
			continue
		}
		if line := walkPath(root, dottedPath); line > 0 {
			return line
		}
	}
	return 0
}

// walkPath follows `dottedPath` through `node` and returns the
// 1-indexed token line of the matched leaf, or 0 on miss. The line
// reported is the offending KEY's line — the operator looks at where
// they typed the bad field name, not the value column.
func walkPath(node ast.Node, dottedPath string) int {
	segments := splitPathSegments(dottedPath)
	cur := node
	var lastKeyLine int
	for segIdx, segment := range segments {
		// Index segments like `mcp_servers[0]` split into (key,
		// index). Bracketed string segments (`model_profiles["x"]`)
		// become (key, "x") and we drop into the named sub-mapping.
		key, idxStr, hasIdx := splitIndex(segment)
		mapping, ok := mappingFromForCmd(cur)
		if !ok {
			return 0
		}
		var matched ast.Node
		var matchedKeyLine int
		for _, kv := range mapping.Values {
			if mappingKeyNameForCmd(kv.Key) == key {
				matched = kv.Value
				if tok := kv.Key.GetToken(); tok != nil {
					matchedKeyLine = tok.Position.Line
				}
				break
			}
		}
		if matched == nil {
			return 0
		}
		lastKeyLine = matchedKeyLine
		// If this is the final segment AND there's no index, the
		// matched key IS our target — report its line.
		isLast := segIdx == len(segments)-1
		if isLast && !hasIdx {
			return matchedKeyLine
		}
		// Either we have an index OR we have more segments to walk.
		if hasIdx {
			// Descend into a sequence or map[key].
			switch v := matched.(type) {
			case *ast.SequenceNode:
				i, isNum := parseDecimal(idxStr)
				if !isNum || i < 0 || i >= len(v.Values) {
					return 0
				}
				cur = v.Values[i]
				if isLast && cur != nil && cur.GetToken() != nil {
					return cur.GetToken().Position.Line
				}
			case *ast.MappingNode:
				// Stringly-indexed map lookup.
				found := false
				for _, kv := range v.Values {
					if mappingKeyNameForCmd(kv.Key) == idxStr {
						cur = kv.Value
						if tok := kv.Key.GetToken(); tok != nil {
							lastKeyLine = tok.Position.Line
						}
						if isLast {
							return lastKeyLine
						}
						found = true
						break
					}
				}
				if !found {
					return 0
				}
			default:
				return 0
			}
		} else {
			cur = matched
		}
	}
	return lastKeyLine
}

// splitPathSegments splits "foo.bar.baz" → ["foo", "bar", "baz"]. A
// bracketed segment like `foo.mcp_servers[0].name` stays attached:
// the index lives on its key segment so callers see ["foo",
// "mcp_servers[0]", "name"].
func splitPathSegments(p string) []string {
	if p == "" {
		return nil
	}
	out := []string{}
	cur := strings.Builder{}
	depth := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '[':
			depth++
			cur.WriteByte(c)
		case ']':
			depth--
			cur.WriteByte(c)
		case '.':
			if depth == 0 {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitIndex breaks `key[idx]` into (`key`, `idx`, true). Returns
// (`key`, "", false) when no bracket is present. The idx is returned
// as a string (sequence indices and bracketed string keys share the
// same shape; the caller decides on context).
func splitIndex(segment string) (key, idx string, ok bool) {
	lb := strings.IndexByte(segment, '[')
	if lb < 0 {
		return segment, "", false
	}
	rb := strings.LastIndexByte(segment, ']')
	if rb <= lb {
		return segment, "", false
	}
	inner := segment[lb+1 : rb]
	// Strip optional quotes around stringly-typed indices.
	inner = strings.TrimPrefix(inner, `"`)
	inner = strings.TrimSuffix(inner, `"`)
	return segment[:lb], inner, true
}

// parseDecimal mirrors strconv.Atoi without pulling the dependency in.
// Returns (0, false) on malformed input.
func parseDecimal(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// mappingFromForCmd mirrors deprecations.go::mappingFrom — extracted
// here so cmd/harbor does not need to import internal/config's
// unexported helpers. The shape is identical: a `MappingValueNode`
// with one entry wraps into a synthesised single-entry `MappingNode`.
func mappingFromForCmd(n ast.Node) (*ast.MappingNode, bool) {
	switch m := n.(type) {
	case *ast.MappingNode:
		return m, true
	case *ast.MappingValueNode:
		return &ast.MappingNode{
			BaseNode: m.BaseNode,
			Start:    m.GetToken(),
			Values:   []*ast.MappingValueNode{m},
		}, true
	}
	return nil, false
}

// mappingKeyNameForCmd mirrors deprecations.go::mappingKeyName.
func mappingKeyNameForCmd(k ast.MapKeyNode) string {
	if s, ok := k.(*ast.StringNode); ok {
		return s.Value
	}
	return ""
}

// internalFindingFor builds a single-row findings list out of an
// I/O / internal error. The category is derived from the wrapped
// error chain.
func internalFindingFor(path string, err error) validationFinding {
	cat := CategoryIORead
	msg := fmt.Sprintf("read %s: %s", path, err.Error())
	hint := "check file permissions and disk health"
	if errors.Is(err, fs.ErrNotExist) {
		cat = CategoryIONotFound
		msg = fmt.Sprintf("file not found: %s", path)
		hint = "create the config file or pass an explicit path"
	}
	return validationFinding{
		Category: cat,
		File:     path,
		Line:     0,
		Message:  msg,
		Hint:     hint,
	}
}

// emitValidationResult writes the validate result and returns a
// CLIError. In human mode the body is a multi-line text rendering
// (one line per finding); in --json mode the body is a single line
// of validationBody. Returns a CLIError whose Code maps to the
// exit-code distinction (validation_failed → 1; validation_internal_error → 2).
//
// The returned CLIError is what cobra surfaces; the body is written
// to cmd.ErrOrStderr() so it shows up on the error channel exactly
// like every other Phase 63 error.
func emitValidationResult(cmd *cobra.Command, flags validateFlags, path string, findings []validationFinding, code, summary, summaryHint string) error {
	w := cmd.ErrOrStderr()
	if flags.jsonMode {
		body := validationBody{
			Error:  summary,
			Code:   code,
			Hint:   summaryHint,
			Errors: findings,
		}
		buf, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			// Fail loudly per CLAUDE.md §5.
			return fmt.Errorf("validate: marshal --json body: %w", marshalErr)
		}
		buf = append(buf, '\n')
		if _, writeErr := w.Write(buf); writeErr != nil {
			return fmt.Errorf("validate: write --json body: %w", writeErr)
		}
	} else {
		if err := writeHumanFindings(w, summary, summaryHint, findings); err != nil {
			return err
		}
	}
	return CLIError{
		Subcommand: "validate",
		Message:    summary,
		Code:       code,
		Hint:       summaryHint,
	}
}

// writeHumanFindings renders the human-mode body to w. Layout:
//
//	Error: harbor validate: <summary> (<hint>)
//	<file>:<line>: <category>: <message>[ (<hint>)]
//	...
//
// The first line is the CLIError-shaped summary so the human-mode
// rendering matches the Phase 63 convention. Subsequent lines are
// per-finding details so operators can grep / read.
func writeHumanFindings(w io.Writer, summary, summaryHint string, findings []validationFinding) error {
	first := "Error: harbor validate: " + summary
	if summaryHint != "" {
		first += " (" + summaryHint + ")"
	}
	if _, err := fmt.Fprintln(w, first); err != nil {
		return fmt.Errorf("validate: write summary line: %w", err)
	}
	for _, f := range findings {
		line := formatFindingHuman(f)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("validate: write finding line: %w", err)
		}
	}
	return nil
}

// formatFindingHuman renders one finding as a single line. Shape:
//
//	<file>:<line>: <category>: <message>[ (<hint>)]
//
// When line == 0 the rendering drops the ":<line>" so the operator
// sees `harbor.yaml: config.semantic: ...` and knows the line was
// not resolvable.
func formatFindingHuman(f validationFinding) string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	out := fmt.Sprintf("%s: %s: %s", loc, f.Category, f.Message)
	if f.Hint != "" {
		out += " (" + f.Hint + ")"
	}
	return out
}

// defaultConfigPathFor is a future seam — `harbor validate` accepts
// only paths today, but a project-aware resolver will later look at
// the working tree to find the canonical config. The helper is a
// pure function so the resolution is testable.
func defaultConfigPathFor(workdir string) string { //nolint:unused // forward-compatible helper; first consumer is a Phase 64+ project-aware resolver
	return filepath.Join(workdir, defaultConfigPath)
}
