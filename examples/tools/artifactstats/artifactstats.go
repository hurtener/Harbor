// Package artifactstats is a worked, runnable example of an in-process
// tool that takes content BY REFERENCE.
//
// It shows the canonical pass-by-reference shape: declare a field of
// type artifactref.Ref in the typed input struct, and the derived JSON
// Schema renders it to the model as a plain artifact-id string. The
// model supplies the id; the runtime resolves it at dispatch and hands
// the function the stored bytes. Bytes flow store → tool. The model
// authors an id and never sees content, and the resolved value is
// dispatch-local — it does not reach the argument JSON, the trajectory,
// an event payload, an audit payload, or a log.
//
// The statistics this tool computes are deliberately mundane; the value
// of the example is the ROUTING shape, not the arithmetic. A real tool
// would run an extraction, a conversion, or an upload against content
// far too large to pass through a model's context.
//
// See docs/recipes/define-a-tool.md for the companion how-to and the
// internal/tools/artifactref package doc for the substitution invariant.
package artifactstats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// StatsArgs is the tool's typed input.
//
// `Artifact` is the reference PARAMETER. It derives to
// `{"type": "string"}` in the schema the model reads, so a call looks
// like `{"artifact": "tool_ab12cd34ef56"}` — an id, never content.
type StatsArgs struct {
	// Artifact is the artifact reference to measure.
	Artifact artifactref.Ref `json:"artifact"`
}

// StatsResult is the tool's typed output. It carries measurements of
// the content and never the content itself, which is what makes the
// tool a fair demonstration: the bytes entered the process and did not
// leave it.
type StatsResult struct {
	// ArtifactID echoes the reference the caller supplied.
	ArtifactID string `json:"artifact_id"`
	// SizeBytes is the resolved content's length in bytes.
	SizeBytes int `json:"size_bytes"`
	// LineCount is the number of newline-terminated or trailing lines.
	LineCount int `json:"line_count"`
	// RuneCount is the number of UTF-8 runes, or -1 when the content is
	// not valid UTF-8.
	RuneCount int `json:"rune_count"`
	// SHA256 is the hex-encoded SHA-256 digest of the resolved content.
	SHA256 string `json:"sha256"`
}

// ToolName is the catalog name the example tool registers under.
const ToolName = "artifact.stats"

// Register adds the artifact.stats tool to cat. It returns the error
// from inproc.RegisterFunc unchanged so callers fail loudly on a
// duplicate name or a schema-derivation bug.
func Register(cat tools.ToolCatalog) error {
	if err := inproc.RegisterFunc[StatsArgs, StatsResult](
		cat,
		ToolName,
		Stats,
		tools.WithDescription(
			"Measure a stored artifact without reading it into the conversation. "+
				"Supply the artifact reference id; the runtime resolves the content and "+
				"the tool returns its size, line count, rune count and SHA-256 digest."),
		tools.WithTags("example", "artifact"),
		tools.WithSideEffect(tools.SideEffectRead),
	); err != nil {
		return fmt.Errorf("register %s: %w", ToolName, err)
	}
	return nil
}

// Stats measures the resolved artifact content.
//
// It reads the bytes through [artifactref.Ref.Bytes], which fails loudly
// when the reference was never resolved rather than returning an empty
// slice the tool would mistake for an empty artifact.
func Stats(_ context.Context, in StatsArgs) (StatsResult, error) {
	body, err := in.Artifact.Bytes()
	if err != nil {
		return StatsResult{}, fmt.Errorf("%s: %w", ToolName, err)
	}
	sum := sha256.Sum256(body)
	out := StatsResult{
		ArtifactID: in.Artifact.ID(),
		SizeBytes:  len(body),
		LineCount:  lineCount(body),
		RuneCount:  -1,
		SHA256:     hex.EncodeToString(sum[:]),
	}
	if utf8.Valid(body) {
		out.RuneCount = utf8.RuneCount(body)
	}
	return out, nil
}

// lineCount counts newline-terminated lines plus a trailing unterminated
// one. Empty content has zero lines.
func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
