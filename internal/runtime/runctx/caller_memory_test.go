// caller_memory_test.go — the composition matrix, the no-aliasing
// property and the D-025 concurrent-reuse run for caller-supplied
// memory (Phase 219 / D-364).
package runctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner"
)

// recalledTurnsFixture is a stand-in for what the runtime's own recall
// producer writes into the External tier.
func recalledTurnsFixture() []map[string]any {
	return []map[string]any{{"user": "how do refunds work?", "assistant": "within 30 days", "score": 0.91}}
}

// TestComposeCallerMemory_Matrix walks every shape of the (mb, raw)
// input space and asserts what survives.
func TestComposeCallerMemory_Matrix(t *testing.T) {
	t.Run("EmptyRawReturnsTheSamePointerUntouched", func(t *testing.T) {
		mb := &planner.MemoryBlocks{Conversation: map[string]any{"strategy": "recent_turns"}}
		got, err := ComposeCallerMemory(mb, nil)
		if err != nil {
			t.Fatalf("ComposeCallerMemory(nil raw): %v", err)
		}
		if got != mb {
			t.Fatalf("an empty raw allocated a new MemoryBlocks (%p != %p) — the no-caller-memory path must be byte-identical", got, mb)
		}
		got, err = ComposeCallerMemory(mb, json.RawMessage{})
		if err != nil {
			t.Fatalf("ComposeCallerMemory(empty raw): %v", err)
		}
		if got != mb {
			t.Fatal("a zero-length raw allocated a new MemoryBlocks")
		}
	})

	t.Run("NilMemoryBlocksAllocates", func(t *testing.T) {
		// The two nil-sources this must survive: a session with no stored
		// memory (ProjectMemoryBlocks returns nil) and a runtime with no
		// memory subsystem configured at all (memBlocks is never set).
		got, err := ComposeCallerMemory(nil, json.RawMessage(`{"note":"alpha"}`))
		if err != nil {
			t.Fatalf("ComposeCallerMemory(nil mb): %v", err)
		}
		if got == nil {
			t.Fatal("ComposeCallerMemory(nil mb) returned nil — a session with no memory must still admit caller memory")
		}
		if got.Conversation != nil {
			t.Fatalf("Conversation = %v, want nil — composition never writes the conversation tier", got.Conversation)
		}
		ext, ok := got.External.(map[string]any)
		if !ok {
			t.Fatalf("External is %T, want map[string]any", got.External)
		}
		if len(ext) != 1 {
			t.Fatalf("External carries %d keys, want exactly the caller's one: %v", len(ext), ext)
		}
	})

	t.Run("ComposesAlongsideTheRuntimeRecallKey", func(t *testing.T) {
		turns := recalledTurnsFixture()
		mb := &planner.MemoryBlocks{
			External:     map[string]any{"recalled_turns": turns},
			Conversation: map[string]any{"strategy": "recent_turns"},
		}
		got, err := ComposeCallerMemory(mb, json.RawMessage(`{"note":"alpha"}`))
		if err != nil {
			t.Fatalf("ComposeCallerMemory: %v", err)
		}
		ext := got.External.(map[string]any)
		if _, ok := ext["recalled_turns"]; !ok {
			t.Fatal("the runtime's recalled_turns key was displaced — composition must add a key, never replace the map")
		}
		if _, ok := ext[CallerSuppliedKey]; !ok {
			t.Fatalf("the caller's %q key is absent", CallerSuppliedKey)
		}
		// Neither producer's value is altered by the other.
		if !reflect.DeepEqual(ext["recalled_turns"], turns) {
			t.Fatalf("recalled_turns changed: got %v, want %v", ext["recalled_turns"], turns)
		}
		if !reflect.DeepEqual(ext[CallerSuppliedKey], map[string]any{"note": "alpha"}) {
			t.Fatalf("caller value changed: got %v", ext[CallerSuppliedKey])
		}
		// Conversation crosses untouched.
		if !reflect.DeepEqual(got.Conversation, mb.Conversation) {
			t.Fatalf("Conversation = %v, want %v", got.Conversation, mb.Conversation)
		}
	})

	t.Run("ConversationOnlyMemoryBlocks", func(t *testing.T) {
		mb := &planner.MemoryBlocks{Conversation: map[string]any{"summary": "billing"}}
		got, err := ComposeCallerMemory(mb, json.RawMessage(`["a","b"]`))
		if err != nil {
			t.Fatalf("ComposeCallerMemory: %v", err)
		}
		if !reflect.DeepEqual(got.Conversation, mb.Conversation) {
			t.Fatalf("Conversation = %v, want it untouched", got.Conversation)
		}
		ext := got.External.(map[string]any)
		if !reflect.DeepEqual(ext[CallerSuppliedKey], []any{"a", "b"}) {
			t.Fatalf("caller value = %v, want [a b]", ext[CallerSuppliedKey])
		}
	})

	t.Run("ExplicitNullIsRefused", func(t *testing.T) {
		_, err := ComposeCallerMemory(nil, json.RawMessage(`null`))
		if !errors.Is(err, ErrCallerMemoryInvalid) {
			t.Fatalf("err = %v, want ErrCallerMemoryInvalid — a null must never be silently treated as absent", err)
		}
	})

	t.Run("InvalidJSONIsRefused", func(t *testing.T) {
		_, err := ComposeCallerMemory(nil, json.RawMessage(`{"unterminated":`))
		if !errors.Is(err, ErrCallerMemoryInvalid) {
			t.Fatalf("err = %v, want ErrCallerMemoryInvalid", err)
		}
	})

	t.Run("NonMapExternalTierFailsLoud", func(t *testing.T) {
		mb := &planner.MemoryBlocks{External: []string{"not a map"}}
		_, err := ComposeCallerMemory(mb, json.RawMessage(`{"note":"alpha"}`))
		if !errors.Is(err, ErrCallerMemoryTierShape) {
			t.Fatalf("err = %v, want ErrCallerMemoryTierShape — silently overwriting another producer's tier is the two-producers-on-one-slot bug", err)
		}
	})
}

// TestComposeCallerMemory_NoAliasing asserts BOTH halves of the scoped
// no-aliasing contract, because an earlier cut asserted only the first
// while the godoc claimed "the returned struct shares no map with the
// input" for the whole struct:
//
//   - `External` — the tier this function WRITES — is a fresh map. A
//     write to either side is invisible to the other.
//   - `Conversation` is carried across BY REFERENCE, and that is pinned
//     AS PRESENT rather than left unstated. It is a residual, not an
//     oversight: copying an arbitrary `any` needs a reflective or
//     round-trip deep copy, and a top-level-only copy would leave the
//     same claim half-true one level down. Nothing downstream writes the
//     tier, so the sharing is safe today. A future deep copy turns this
//     assertion RED, which is the point — the godoc must be re-derived
//     with it rather than silently outliving the mechanism.
func TestComposeCallerMemory_NoAliasing(t *testing.T) {
	mb := &planner.MemoryBlocks{
		External:     map[string]any{"recalled_turns": recalledTurnsFixture()},
		Conversation: map[string]any{"strategy": "recent_turns"},
	}
	before := len(mb.External.(map[string]any))

	got, err := ComposeCallerMemory(mb, json.RawMessage(`{"note":"alpha"}`))
	if err != nil {
		t.Fatalf("ComposeCallerMemory: %v", err)
	}
	if got == mb {
		t.Fatal("ComposeCallerMemory returned its argument — the caller's blocks were mutated in place")
	}
	if after := len(mb.External.(map[string]any)); after != before {
		t.Fatalf("the input's External map grew from %d to %d keys — the argument was mutated", before, after)
	}
	if _, leaked := mb.External.(map[string]any)[CallerSuppliedKey]; leaked {
		t.Fatal("the caller's key was written into the INPUT map")
	}

	// The two External maps are genuinely distinct: a write to the result
	// is not observable through the input.
	got.External.(map[string]any)["written_after"] = true
	if _, shared := mb.External.(map[string]any)["written_after"]; shared {
		t.Fatal("the result aliases the input's External map")
	}

	// THE RESIDUAL, PINNED AS PRESENT. `Conversation` IS shared. Assert
	// it in the direction the godoc states, so the doc and the mechanism
	// cannot drift apart in either direction.
	got.Conversation.(map[string]any)["written_after"] = true
	if _, shared := mb.Conversation.(map[string]any)["written_after"]; !shared {
		t.Fatal("Conversation is no longer shared with the input — the tier is now copied, which is an IMPROVEMENT, but ComposeCallerMemory's godoc still scopes its no-aliasing guarantee to External only. Widen the godoc and delete this assertion.")
	}
}

// TestComposeCallerMemory_ExternalValuesAreSharedNotDeepCopied pins the
// SECOND residual — the one the assertion above cannot see and an
// earlier godoc denied outright by saying a write to either side's
// External "cannot be observed through the other".
//
// The copy at the top of the External branch is `external[k] = v` over
// the input's entries: it makes a fresh MAP, not a fresh value graph. So
// a nested map or slice is shared, and mutating THROUGH a copied entry
// is visible on both sides. This is not a corner case — the runtime's
// own recall producer writes exactly this shape
// (`{"recalled_turns": []map[string]any{…}}`), which is why the fixture
// here is that shape rather than a flat scalar map.
//
// It is asserted AS PRESENT rather than fixed, and the reason is stated
// in the godoc: composition's input has a single holder (the run loop
// replaces its local with the result and drops the original), so no
// second observer exists for the nested write to reach. Deep-copying an
// arbitrary `any` costs a reflective or round-trip copy — the same
// objection that keeps `Conversation` shared.
//
// The failure message tells the next reader what to do in EITHER
// direction, because both are real changes rather than noise: a deep
// copy landing means the godoc must widen, and it going red for any
// other reason means the aliasing shape moved under the doc.
func TestComposeCallerMemory_ExternalValuesAreSharedNotDeepCopied(t *testing.T) {
	turns := recalledTurnsFixture()
	mb := &planner.MemoryBlocks{
		External:     map[string]any{"recalled_turns": turns},
		Conversation: map[string]any{"strategy": "recent_turns"},
	}

	got, err := ComposeCallerMemory(mb, json.RawMessage(`{"note":"alpha"}`))
	if err != nil {
		t.Fatalf("ComposeCallerMemory: %v", err)
	}

	// The copied entry must be the SAME underlying value, not a clone.
	gotTurns, ok := got.External.(map[string]any)["recalled_turns"].([]map[string]any)
	if !ok {
		t.Fatalf("recalled_turns did not survive composition with its type intact: %T",
			got.External.(map[string]any)["recalled_turns"])
	}
	if len(gotTurns) != len(turns) {
		t.Fatalf("recalled_turns length changed across composition: %d -> %d", len(turns), len(gotTurns))
	}

	// Mutate THROUGH the result's copied entry and look for it on the
	// input. A deep copy would hide it; the shallow copy does not.
	gotTurns[0]["written_after"] = true
	if _, shared := turns[0]["written_after"]; !shared {
		t.Fatal("External's nested values are no longer shared with the input — the tier is now DEEP-copied, which is an IMPROVEMENT, but ComposeCallerMemory's godoc explicitly scopes its no-aliasing guarantee to the External MAP and documents the nested sharing as a residual. Widen the godoc and delete this assertion.")
	}

	// And the converse direction, so the sharing is pinned as genuinely
	// mutual rather than one-way.
	turns[0]["written_from_input"] = true
	if _, shared := gotTurns[0]["written_from_input"]; !shared {
		t.Fatal("a write through the INPUT's nested value was not visible on the result — the aliasing is now one-way, which the godoc does not describe; re-derive it")
	}

	// The top-level guarantee the godoc DOES make still holds alongside
	// the residual: the two maps are distinct objects. Asserting both in
	// one test is what stops a future reader from concluding that
	// "shallow" means "no isolation at all".
	got.External.(map[string]any)["top_level_key"] = true
	if _, leaked := mb.External.(map[string]any)["top_level_key"]; leaked {
		t.Fatal("the result aliases the input's External MAP itself — the fresh-map half of the guarantee is gone")
	}
}

// TestComposeCallerMemory_ConcurrentReuse_NoCrossTalk is the D-025 gate:
// N=128 goroutines against ONE shared input under -race, each with a
// distinct payload, asserting no data race, no content bleed (each
// goroutine sees its own marker and NO sibling's), and that a cancelled
// sibling cannot disturb the rest.
//
// ComposeCallerMemory is a pure function over its arguments — it starts
// no goroutines and holds no state — so the guarantee under test is that
// the SHARED argument is never written and never aliased into a result.
func TestComposeCallerMemory_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const n = 128

	baseline := runtime.NumGoroutine()

	// ONE shared, immutable input every goroutine composes against.
	shared := &planner.MemoryBlocks{
		External:     map[string]any{"recalled_turns": recalledTurnsFixture()},
		Conversation: map[string]any{"strategy": "recent_turns"},
	}

	// A cancellable sibling: one goroutine abandons its work early. Its
	// cancellation must not disturb the others' results.
	cancelled := make(chan struct{})

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i == 0 {
				close(cancelled)
				return
			}
			marker := fmt.Sprintf("tenant-%03d-marker", i)
			raw := json.RawMessage(fmt.Sprintf(`{"marker":%q}`, marker))
			got, err := ComposeCallerMemory(shared, raw)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			ext, ok := got.External.(map[string]any)
			if !ok {
				errCh <- fmt.Errorf("goroutine %d: External is %T", i, got.External)
				return
			}
			mine, ok := ext[CallerSuppliedKey].(map[string]any)
			if !ok {
				errCh <- fmt.Errorf("goroutine %d: caller value is %T", i, ext[CallerSuppliedKey])
				return
			}
			// Own marker present.
			if mine["marker"] != marker {
				errCh <- fmt.Errorf("goroutine %d: marker = %v, want %q", i, mine["marker"], marker)
				return
			}
			// No sibling's marker anywhere in the composed blocks.
			blob, mErr := json.Marshal(got)
			if mErr != nil {
				errCh <- fmt.Errorf("goroutine %d: marshal: %w", i, mErr)
				return
			}
			for j := 1; j < n; j++ {
				if j == i {
					continue
				}
				if other := fmt.Sprintf("tenant-%03d-marker", j); strings.Contains(string(blob), other) {
					errCh <- fmt.Errorf("goroutine %d: sibling %d's marker %q bled into its blocks", i, j, other)
					return
				}
			}
			// The shared input is never written.
			if _, leaked := shared.External.(map[string]any)[CallerSuppliedKey]; leaked {
				errCh <- fmt.Errorf("goroutine %d: the shared input gained the caller key", i)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	select {
	case <-cancelled:
	default:
		t.Fatal("the cancelling goroutine never ran — the cancellation arm did not execute")
	}
	if got := len(shared.External.(map[string]any)); got != 1 {
		t.Fatalf("the shared input's External map has %d keys after %d concurrent compositions, want 1", got, n)
	}

	// No goroutine leak: the helper starts none, so the count settles
	// back once the test's own goroutines have joined.
	waitSettle(t, baseline)
}

// waitSettle polls until the goroutine count returns to baseline, with a
// bounded real-time budget. Never a bare sleep-as-synchronisation.
func waitSettle(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines did not settle: %d, baseline %d", runtime.NumGoroutine(), baseline)
}
