package llm

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestCompactionAttempt_DeterministicDistinctAndReadOnly(t *testing.T) {
	t.Parallel()
	grant := ExternalGrant{LogicalCallID: "root", AttemptNonce: "signed-nonce"}
	ctx, parent, err := EnsureGrantAttemptScope(WithAttemptStep(t.Context(), 7), grant)
	if err != nil {
		t.Fatal(err)
	}
	original := *parent
	seen := map[string]bool{}
	for i := range DefaultCompactionCalls + 4 {
		ordinal := i + 1
		child, err := CompactionAttemptContext(ctx, ordinal)
		if err != nil {
			t.Fatal(err)
		}
		child = WithAttemptCoordinates(child, 3, 2, 1, 0)
		_, scope, err := EnsureGrantAttemptScope(child, grant)
		if err != nil {
			t.Fatal(err)
		}
		wantID := fmt.Sprintf("root/step/7/compaction/%d", ordinal)
		if scope.CallID != wantID || scope.LogicalCallID != wantID || seen[scope.AttemptNonce] || scope.PlannerStep != 7 || scope.Attempt != 3 || scope.Retry != 2 || scope.Downgrade != 1 {
			t.Fatalf("bad child: %+v", scope)
		}
		seen[scope.AttemptNonce] = true
		again, err := CompactionAttemptContext(ctx, ordinal)
		if err != nil {
			t.Fatal(err)
		}
		_, replay, err := EnsureGrantAttemptScope(again, grant)
		if err != nil || replay.AttemptNonce != scope.AttemptNonce || replay.LogicalCallID != scope.LogicalCallID {
			t.Fatalf("unstable replay: %v", err)
		}
		if !validCompactionAttempt(scope.LogicalCallID, scope.AttemptNonce, parent.LogicalCallID, parent.AttemptNonce) {
			t.Fatal("valid child rejected")
		}
	}
	if *parent != original {
		t.Fatal("mutated parent scope")
	}
	for _, ordinal := range []int{-1, 0} {
		if _, err := CompactionAttemptContext(ctx, ordinal); err == nil {
			t.Fatal("invalid ordinal accepted")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CompactionAttemptContext(cancelled, 1); err == nil {
		t.Fatal("canceled maintenance admitted")
	}
}

func TestCompactionAttempt_RejectsForgedDerivation(t *testing.T) {
	t.Parallel()
	id, nonce := compactionAttemptIdentity("root/step/7", "nonce", 1)
	for _, fake := range []struct{ id, nonce, parent, parentNonce string }{
		{id, "wrong", "root/step/7", "nonce"},
		{id, nonce, "root/step/8", "nonce"},
		{id, nonce, "root/step/7", "different"},
		{"root/step/7/compaction/01", nonce, "root/step/7", "nonce"},
		{"root/step/7/compaction/+1", nonce, "root/step/7", "nonce"},
		{"root/step/7/compaction/0", nonce, "root/step/7", "nonce"},
		{"root/step/7/compaction/17", nonce, "root/step/7", "nonce"}, // wrong nonce for this ordinal
		{"root/step/7/compaction/9999999999999999999999999999", nonce, "root/step/7", "nonce"},
		{id + "/compaction/1", nonce, "root/step/7", "nonce"},
		{"root/step/7/arbitrary/1", nonce, "root/step/7", "nonce"},
	} {
		if validCompactionAttempt(fake.id, fake.nonce, fake.parent, fake.parentNonce) {
			t.Fatal("forged child accepted")
		}
	}
}

func TestCompactionAttempt_ConcurrentScopes(t *testing.T) {
	t.Parallel()
	grant := ExternalGrant{LogicalCallID: "root", AttemptNonce: "nonce"}
	parent := WithAttemptStep(t.Context(), 4)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, err := CompactionAttemptContext(parent, 1+i%(DefaultCompactionCalls+4))
			if err != nil {
				t.Error(err)
				return
			}
			_, scope, err := EnsureGrantAttemptScope(ctx, grant)
			if err != nil || scope.LogicalCallID != fmt.Sprintf("root/step/4/compaction/%d", 1+i%(DefaultCompactionCalls+4)) {
				t.Errorf("scope bleed: %v", err)
			}
		}()
	}
	wg.Wait()
}
