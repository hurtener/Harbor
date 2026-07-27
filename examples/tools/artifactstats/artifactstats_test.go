package artifactstats_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/examples/tools/artifactstats"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

const sampleBody = "alpha\nbeta\ngamma\n"

func sampleResolver(id string, body []byte) artifactref.Resolver {
	return artifactref.ResolverFunc(func(_ context.Context, got string) ([]byte, error) {
		if got != id {
			return nil, errors.New("no such artifact")
		}
		return body, nil
	})
}

// TestRegister_ExposesAStringReferenceParameter — the operator-facing
// promise of the example: declare the field type, get a string parameter
// in the schema the model reads.
func TestRegister_ExposesAStringReferenceParameter(t *testing.T) {
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, ok := cat.Resolve(artifactstats.ToolName)
	if !ok {
		t.Fatalf("%s did not resolve", artifactstats.ToolName)
	}
	var doc map[string]any
	if err := json.Unmarshal(desc.Tool.ArgsSchema, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, _ := doc["properties"].(map[string]any)
	field, _ := props["artifact"].(map[string]any)
	if field == nil || field["type"] != "string" {
		t.Fatalf("artifact parameter is not a string: %s", desc.Tool.ArgsSchema)
	}
}

// TestInvoke_ReadsTheResolvedBytesAndReturnsOnlyMeasurements — the
// direction of travel. The bytes entered the process; what comes back
// out is arithmetic over them.
func TestInvoke_ReadsTheResolvedBytesAndReturnsOnlyMeasurements(t *testing.T) {
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, _ := cat.Resolve(artifactstats.ToolName)

	const id = "tool_ab12cd34ef56"
	ctx := artifactref.WithResolver(context.Background(), sampleResolver(id, []byte(sampleBody)))

	res, err := desc.Invoke(ctx, json.RawMessage(`{"artifact":"`+id+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, ok := res.Value.(artifactstats.StatsResult)
	if !ok {
		t.Fatalf("result = %T, want StatsResult", res.Value)
	}
	if out.SizeBytes != len(sampleBody) {
		t.Errorf("SizeBytes = %d, want %d", out.SizeBytes, len(sampleBody))
	}
	if out.LineCount != 3 {
		t.Errorf("LineCount = %d, want 3", out.LineCount)
	}
	if out.RuneCount != len([]rune(sampleBody)) {
		t.Errorf("RuneCount = %d, want %d", out.RuneCount, len([]rune(sampleBody)))
	}
	sum := sha256.Sum256([]byte(sampleBody))
	if out.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %q, want %q", out.SHA256, hex.EncodeToString(sum[:]))
	}
	if out.ArtifactID != id {
		t.Errorf("ArtifactID = %q, want %q", out.ArtifactID, id)
	}

	encoded, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(encoded), "alpha") {
		t.Errorf("the result carries the resolved content: %s", encoded)
	}
}

// TestStats_FailsLoudlyOnAnUnresolvedReference — reading an unresolved
// reference must not read as an empty artifact.
func TestStats_FailsLoudlyOnAnUnresolvedReference(t *testing.T) {
	_, err := artifactstats.Stats(context.Background(), artifactstats.StatsArgs{
		Artifact: artifactref.NewRef("tool_never_resolved"),
	})
	if !errors.Is(err, artifactref.ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

// TestStats_EmptyArtifactIsMeasurable — an artifact that legitimately
// holds no bytes is distinguishable from one that was never resolved.
func TestStats_EmptyArtifactIsMeasurable(t *testing.T) {
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, _ := cat.Resolve(artifactstats.ToolName)

	const id = "tool_empty000000"
	ctx := artifactref.WithResolver(context.Background(), sampleResolver(id, []byte{}))
	res, err := desc.Invoke(ctx, json.RawMessage(`{"artifact":"`+id+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, _ := res.Value.(artifactstats.StatsResult)
	if out.SizeBytes != 0 || out.LineCount != 0 {
		t.Fatalf("empty artifact measured as %+v", out)
	}
	if out.RuneCount != 0 {
		t.Errorf("RuneCount = %d, want 0 (empty bytes are valid UTF-8)", out.RuneCount)
	}
}

// TestStats_NonUTF8ContentReportsAnUnknownRuneCount — the tool reports
// what it can measure and marks what it cannot, rather than reporting a
// number nobody computed.
func TestStats_NonUTF8ContentReportsAnUnknownRuneCount(t *testing.T) {
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, _ := cat.Resolve(artifactstats.ToolName)

	const id = "tool_binary00000"
	ctx := artifactref.WithResolver(context.Background(), sampleResolver(id, []byte{0xff, 0xfe, 0x00}))
	res, err := desc.Invoke(ctx, json.RawMessage(`{"artifact":"`+id+`"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, _ := res.Value.(artifactstats.StatsResult)
	if out.RuneCount != -1 {
		t.Fatalf("RuneCount = %d, want -1 for non-UTF-8 content", out.RuneCount)
	}
	if out.SizeBytes != 3 {
		t.Fatalf("SizeBytes = %d, want 3", out.SizeBytes)
	}
}
