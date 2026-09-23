package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/planner"
)

func TestRetainedInputs_ValidationAndBoundedProjection(t *testing.T) {
	t.Parallel()
	for _, inputs := range [][]planner.InputArtifactView{
		{{ID: ""}}, {{ID: " bad "}}, {{ID: strings.Repeat("x", 257)}}, {{ID: string([]byte{255})}}, make([]planner.InputArtifactView, 65),
	} {
		if _, err := retainedInputContext(inputs); err == nil {
			t.Fatal("invalid input context accepted")
		}
	}
	if step, err := retainedInputContext(nil); step != nil || err != nil {
		t.Fatal("empty inputs changed context")
	}
	for _, tc := range []struct {
		requested []string
		resolved  []planner.InputArtifactView
	}{
		{[]string{"missing"}, nil}, {nil, []planner.InputArtifactView{{ID: "extra"}}}, {[]string{"x"}, []planner.InputArtifactView{{ID: "other"}}},
		{[]string{" "}, nil}, {make([]string, 65), nil}, {nil, make([]planner.InputArtifactView, 65)},
	} {
		if err := ValidateRetainedInputs(tc.requested, tc.resolved); err == nil {
			t.Fatal("incomplete input admission accepted")
		}
	}
	if err := ValidateRetainedInputs([]string{"a", "a"}, []planner.InputArtifactView{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	step, err := retainedInputContext([]planner.InputArtifactView{{ID: "b", Bytes: []byte("PRIVATE-BYTES")}, {ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	rc := referenceContext()
	rc.Trajectory.Steps = []planner.Step{*step}
	body, err := rc.Trajectory.Serialize()
	if err != nil || strings.Contains(string(body), "PRIVATE-BYTES") {
		t.Fatal("input bytes retained")
	}
	refs, err := retainedResultReferences(t.Context(), rc, referenceStore())
	if err != nil || len(refs) != 2 || refs[0].Ref != "a" || refs[1].Ref != "b" || refs[0].Provenance != "retained input attachment" {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	for _, raw := range []any{nil, "x", []any{}, []any{12}, []any{""}, []any{"a ", "b"}, make([]any, 65)} {
		if _, err := retainedResultReferences(t.Context(), referenceContext(map[string]any{retainedInputRefsKey: raw}), referenceStore()); err == nil {
			t.Fatal("malformed attachment entry accepted")
		}
	}
	// Tool output and nested injection cannot impersonate host attachment input.
	foreign := referenceContext(map[string]any{retainedInputRefsKey: []string{"forged"}}, map[string]any{"steering_update": map[string]any{"context": map[string]any{retainedInputRefsKey: []string{"nested"}}}})
	foreign.Trajectory.Steps[0].Action = planner.CallTool{Tool: "read"}
	if refs, err := retainedResultReferences(t.Context(), foreign, nil); err != nil || len(refs) != 0 {
		t.Fatal("untrusted nested data promoted to admitted input")
	}
}

func TestRetainedInputs_ConcurrentScopeAndDeletedReferences(t *testing.T) {
	t.Parallel()
	store := retainedReferenceStore{lookup: func(_ context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
		if id != scope.SessionID {
			t.Error("cross-session input reference")
		}
		return &artifacts.ArtifactRef{ID: id, Scope: scope, MimeType: "image/png", SizeBytes: 1024}, true, nil
	}}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("input-%03d", i)
			rc := referenceContext(map[string]any{retainedInputRefsKey: []string{id}})
			rc.Quadruple.SessionID = id
			refs, err := retainedResultReferences(t.Context(), rc, store)
			if err != nil || len(refs) != 1 || refs[0].Ref != id {
				t.Errorf("scope %d: %+v %v", i, refs, err)
			}
		}()
	}
	wg.Wait()
	for _, wrongScope := range []bool{false, true} {
		bad := retainedReferenceStore{lookup: func(_ context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
			if !wrongScope {
				return nil, false, nil
			}
			scope.UserID = "other"
			return &artifacts.ArtifactRef{ID: id, Scope: scope}, true, nil
		}}
		if _, err := retainedResultReferences(t.Context(), referenceContext(map[string]any{retainedInputRefsKey: []string{"a"}}), bad); err == nil {
			t.Fatal("unavailable or cross-scope attachment accepted")
		}
	}
}
