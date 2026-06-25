package projection

import (
	"slices"
	"testing"
)

// TestUnionSorted_OrderIndependentAndDeduped pins the property the three-set
// tool-exposure union relies on: the result is byte-identical regardless of the
// order of the two arguments OR the order of elements within them, and
// duplicates collapse. The projection folds (admin ∪ user ∪ session) in a fixed
// order today, but the grow-only union must not depend on that — a future
// reorder of the tiers must not change the run's tool view.
func TestUnionSorted_OrderIndependentAndDeduped(t *testing.T) {
	want := []string{"a", "b", "c", "d"}
	perms := [][2][]string{
		{{"a", "b"}, {"c", "d"}},
		{{"d", "c"}, {"b", "a"}},
		{{"b", "d"}, {"a", "c"}},
		{{"c", "a", "c"}, {"d", "b", "b"}}, // duplicates within and across args
		{{"a", "b", "c", "d"}, nil},
		{nil, {"d", "c", "b", "a"}},
		{{"a", "a", "b"}, {"b", "c", "d", "d"}},
	}
	for i, p := range perms {
		got := unionSorted(p[0], p[1])
		if !slices.Equal(got, want) {
			t.Fatalf("perm %d: unionSorted(%v, %v) = %v, want byte-identical %v", i, p[0], p[1], got, want)
		}
	}

	// Commutativity, stated explicitly.
	ab := unionSorted([]string{"x", "y"}, []string{"y", "z"})
	ba := unionSorted([]string{"z", "y"}, []string{"y", "x"})
	if !slices.Equal(ab, ba) {
		t.Fatalf("not commutative: %v vs %v", ab, ba)
	}

	// The empty union is nil (the grow-from-nothing base case).
	if got := unionSorted(nil, nil); got != nil {
		t.Fatalf("empty union = %v, want nil", got)
	}
}
