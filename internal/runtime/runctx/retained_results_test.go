package runctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// The negative driver is intentionally minimal: reference projection must
// never load blob bytes or enumerate unrelated session artifacts.
type retainedReferenceStore struct {
	artifacts.ArtifactStore
	lookup func(context.Context, artifacts.ArtifactScope, string) (*artifacts.ArtifactRef, bool, error)
}

func (s retainedReferenceStore) GetRef(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	return s.lookup(ctx, scope, id)
}

func referenceContext(values ...any) planner.RunContext {
	rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "next"}, Trajectory: &planner.Trajectory{}}
	for _, value := range values {
		rc.Trajectory.Steps = append(rc.Trajectory.Steps, planner.Step{LLMObservation: value})
	}
	return rc
}

func offloadedReference(ref string) any {
	return map[string]any{"tool": "read", "size_bytes": 120000, "truncated": true, "preview": "bounded", "artifact_ref": ref}
}

func referenceStore() retainedReferenceStore {
	return retainedReferenceStore{lookup: func(_ context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
		return &artifacts.ArtifactRef{ID: id, Scope: scope, MimeType: "application/json", SizeBytes: 120000}, true, nil
	}}
}

func TestRetainedResults_StableScopedProjection(t *testing.T) {
	t.Parallel()
	store := referenceStore()
	step, err := planner.RetainStep(planner.Step{Action: planner.CallTool{Tool: "read", CallID: "old"}, LLMObservation: offloadedReference("b")}, "previous", 0)
	if err != nil {
		t.Fatal(err)
	}
	rc := referenceContext(map[string]any{"branches": []any{offloadedReference("b"), offloadedReference("a")}})
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, step)
	before, _ := rc.Trajectory.Serialize()
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs, err := retainedResultReferences(t.Context(), rc, store)
			if err != nil || len(refs) != 2 || refs[0].Ref != "a" || refs[1].Ref != "b" || refs[0].SizeBytes != 120000 {
				t.Errorf("unstable projection: %+v %v", refs, err)
			}
		}()
	}
	wg.Wait()
	after, _ := rc.Trajectory.Serialize()
	if string(before) != string(after) {
		t.Fatal("projection mutated retained evidence")
	}
}

func TestRetainedResults_OnlyEnvelopesNotSummaryOrArguments(t *testing.T) {
	t.Parallel()
	rc := referenceContext(`{"artifact_ref":"prose"}`, map[string]any{"artifact_ref": "untyped"}, nil)
	rc.Trajectory.Summary = &planner.Summary{Facts: []string{"artifact_ref: invented"}}
	rc.Trajectory.Steps[0].Action = planner.CallTool{Tool: "read", Args: json.RawMessage(`{"artifact_ref":"argument"}`)}
	refs, err := retainedResultReferences(t.Context(), rc, nil)
	if err != nil || refs != nil {
		t.Fatalf("invented retrieval reference: %+v %v", refs, err)
	}
}

func TestRetainedResults_RejectsUnavailableOrForeignReference(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing", "nil", "error", "tenant", "user", "session", "id", "size"} {
		t.Run(mode, func(t *testing.T) {
			store := retainedReferenceStore{lookup: func(_ context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
				if scope.TenantID != "t" || scope.UserID != "u" || scope.SessionID != "s" {
					t.Fatal("lookup escaped current identity")
				}
				ref := &artifacts.ArtifactRef{ID: id, Scope: scope, SizeBytes: 120000}
				switch mode {
				case "missing":
					return nil, false, nil
				case "nil":
					return nil, true, nil
				case "error":
					return nil, false, errors.New("PRIVATE DRIVER DETAIL")
				case "tenant":
					ref.Scope.TenantID = "other"
				case "user":
					ref.Scope.UserID = "other"
				case "session":
					ref.Scope.SessionID = "other"
				case "id":
					ref.ID = "different"
				case "size":
					ref.SizeBytes = -1
				}
				return ref, true, nil
			}}
			refs, err := retainedResultReferences(t.Context(), referenceContext(offloadedReference("ref")), store)
			if !errors.Is(err, ErrRetainedContextUnavailable) || refs != nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("invalid scoped evidence accepted: %+v %v", refs, err)
			}
		})
	}
}

func TestRetainedResults_BoundsAndCancellation(t *testing.T) {
	t.Parallel()
	for _, n := range []int{64, 65} {
		values := make([]any, n)
		for i := range values {
			values[i] = offloadedReference(fmt.Sprint(i))
		}
		refs, err := retainedResultReferences(t.Context(), referenceContext(values...), referenceStore())
		if n == 64 && (err != nil || len(refs) != 64) {
			t.Fatalf("bounded references lost: %d %v", len(refs), err)
		}
		if n == 65 && !errors.Is(err, ErrRetainedContextCapacity) {
			t.Fatal("reference ceiling silently truncated")
		}
	}
	for _, ref := range []string{"", " ref", strings.Repeat("x", 257)} {
		if _, err := retainedResultReferences(t.Context(), referenceContext(offloadedReference(ref)), referenceStore()); !errors.Is(err, ErrRetainedContextUnavailable) {
			t.Fatal("invalid reference accepted")
		}
	}
	deep := offloadedReference("a")
	for range 66 {
		deep = []any{deep}
	}
	for _, value := range []any{deep, strings.Repeat("x", 2*maxRetainedContextBytes+1)} {
		if _, err := retainedResultReferences(t.Context(), referenceContext(value), referenceStore()); !errors.Is(err, ErrRetainedContextCapacity) {
			t.Fatal("unbounded retained scan")
		}
	}
	store := referenceStore()
	old := store.lookup
	store.lookup = func(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
		ref, found, err := old(ctx, scope, id)
		ref.Filename = strings.Repeat("x", maxRetainedResultMetadataBytes)
		return ref, found, err
	}
	if _, err := retainedResultReferences(t.Context(), referenceContext(offloadedReference("a")), store); !errors.Is(err, ErrRetainedContextCapacity) {
		t.Fatal("unbounded metadata")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := retainedResultReferences(ctx, referenceContext(offloadedReference("a")), referenceStore()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled lookup")
	}
	if _, err := retainedResultReferences(t.Context(), referenceContext(offloadedReference("a")), nil); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("missing store accepted")
	}
	if _, err := retainedResultReferences(t.Context(), referenceContext(make(chan int)), referenceStore()); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("invalid observation accepted")
	}
	if refs, err := retainedResultReferences(t.Context(), referenceContext(nil), nil); err != nil || !reflect.DeepEqual(refs, []planner.ArtifactManifestEntry(nil)) {
		t.Fatal("empty evidence acquired dependency")
	}
}
