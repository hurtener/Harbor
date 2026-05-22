package tasks

import (
	"errors"
	"sync"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestListFilterFromWire_NilWire_ReturnsZeroFilter asserts a nil wire
// filter translates to the zero-valued registry filter (every task in
// the session).
func TestListFilterFromWire_NilWire_ReturnsZeroFilter(t *testing.T) {
	rf, err := ListFilterFromWire(nil)
	if err != nil {
		t.Fatalf("ListFilterFromWire(nil) error = %v, want nil", err)
	}
	if rf.Status != nil || rf.Kind != nil || rf.ParentID != nil {
		t.Errorf("nil wire ⇒ non-zero registry filter: %+v", rf)
	}
}

// TestListFilterFromWire_EmptyWire_ReturnsZeroFilter asserts an empty
// (zero-valued) wire filter likewise yields the wildcard registry
// filter.
func TestListFilterFromWire_EmptyWire_ReturnsZeroFilter(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{})
	if err != nil {
		t.Fatalf("ListFilterFromWire(empty) error = %v, want nil", err)
	}
	if rf.Status != nil || rf.Kind != nil || rf.ParentID != nil {
		t.Errorf("empty wire ⇒ non-zero registry filter: %+v", rf)
	}
}

// TestListFilterFromWire_BackgroundKind_NarrowsKind asserts the
// Background Jobs page's canonical `Kinds: ["background"]` queue-mode
// binding narrows the registry Kind pointer to KindBackground.
func TestListFilterFromWire_BackgroundKind_NarrowsKind(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		Kinds: []prototypes.TaskKind{prototypes.TaskKindBackground},
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(background) error = %v", err)
	}
	if rf.Kind == nil {
		t.Fatal("Kinds=[background] ⇒ registry Kind pointer is nil, want KindBackground")
	}
	if *rf.Kind != KindBackground {
		t.Errorf("registry Kind = %q, want %q", *rf.Kind, KindBackground)
	}
}

// TestListFilterFromWire_ForegroundKind_NarrowsKind covers the
// foreground single-kind narrowing.
func TestListFilterFromWire_ForegroundKind_NarrowsKind(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		Kinds: []prototypes.TaskKind{prototypes.TaskKindForeground},
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(foreground) error = %v", err)
	}
	if rf.Kind == nil || *rf.Kind != KindForeground {
		t.Errorf("registry Kind = %v, want %q", rf.Kind, KindForeground)
	}
}

// TestListFilterFromWire_MultiKind_LeavesKindNil asserts a two-kind
// wire set is NOT down-converted to a single registry pointer — it is
// left to the Service-layer filterMatches multi-kind pass.
func TestListFilterFromWire_MultiKind_LeavesKindNil(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		Kinds: []prototypes.TaskKind{
			prototypes.TaskKindForeground,
			prototypes.TaskKindBackground,
		},
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(multi-kind) error = %v", err)
	}
	if rf.Kind != nil {
		t.Errorf("multi-kind wire set ⇒ registry Kind pointer = %v, want nil", rf.Kind)
	}
}

// TestListFilterFromWire_SingleStatus_NarrowsStatus asserts a one-element
// status set narrows the registry Status pointer.
func TestListFilterFromWire_SingleStatus_NarrowsStatus(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		Statuses: []prototypes.TaskStatus{prototypes.TaskStatusRunning},
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(running) error = %v", err)
	}
	if rf.Status == nil || *rf.Status != StatusRunning {
		t.Errorf("registry Status = %v, want %q", rf.Status, StatusRunning)
	}
}

// TestListFilterFromWire_MultiStatus_LeavesStatusNil asserts a
// multi-status set is left to the Service pass.
func TestListFilterFromWire_MultiStatus_LeavesStatusNil(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		Statuses: []prototypes.TaskStatus{
			prototypes.TaskStatusRunning,
			prototypes.TaskStatusPaused,
		},
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(multi-status) error = %v", err)
	}
	if rf.Status != nil {
		t.Errorf("multi-status wire set ⇒ registry Status = %v, want nil", rf.Status)
	}
}

// TestListFilterFromWire_ParentTaskID_NarrowsParentID asserts the
// parent-task pointer is translated verbatim.
func TestListFilterFromWire_ParentTaskID_NarrowsParentID(t *testing.T) {
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		ParentTaskID: "task-parent-7",
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(parent) error = %v", err)
	}
	if rf.ParentID == nil || *rf.ParentID != TaskID("task-parent-7") {
		t.Errorf("registry ParentID = %v, want task-parent-7", rf.ParentID)
	}
}

// TestListFilterFromWire_GroupAndApprovalFacets_NotTranslated asserts
// the GroupID + HasPendingApproval facets — which have no registry
// counterpart — leave the registry filter untouched (the Service-layer
// filterMatches applies them).
func TestListFilterFromWire_GroupAndApprovalFacets_NotTranslated(t *testing.T) {
	hasApproval := true
	rf, err := ListFilterFromWire(&prototypes.TaskFilter{
		GroupID:            "group-9",
		HasPendingApproval: &hasApproval,
	})
	if err != nil {
		t.Fatalf("ListFilterFromWire(group+approval) error = %v", err)
	}
	if rf.Status != nil || rf.Kind != nil || rf.ParentID != nil {
		t.Errorf("group/approval facets leaked into registry filter: %+v", rf)
	}
}

// TestListFilterFromWire_UnknownStatus_FailsLoud asserts an unknown
// wire status enum fails loud with ErrInvalidListFilter — never a
// silent filter-that-matches-nothing (CLAUDE.md §13).
func TestListFilterFromWire_UnknownStatus_FailsLoud(t *testing.T) {
	_, err := ListFilterFromWire(&prototypes.TaskFilter{
		Statuses: []prototypes.TaskStatus{prototypes.TaskStatus("bogus")},
	})
	if !errors.Is(err, ErrInvalidListFilter) {
		t.Fatalf("unknown status error = %v, want ErrInvalidListFilter", err)
	}
}

// TestListFilterFromWire_UnknownStatusInMultiSet_FailsLoud asserts a
// malformed enum buried in a multi-element set still fails loud.
func TestListFilterFromWire_UnknownStatusInMultiSet_FailsLoud(t *testing.T) {
	_, err := ListFilterFromWire(&prototypes.TaskFilter{
		Statuses: []prototypes.TaskStatus{
			prototypes.TaskStatusRunning,
			prototypes.TaskStatus("bogus"),
		},
	})
	if !errors.Is(err, ErrInvalidListFilter) {
		t.Fatalf("unknown status in multi-set error = %v, want ErrInvalidListFilter", err)
	}
}

// TestListFilterFromWire_UnknownKind_FailsLoud asserts an unknown wire
// kind enum fails loud.
func TestListFilterFromWire_UnknownKind_FailsLoud(t *testing.T) {
	_, err := ListFilterFromWire(&prototypes.TaskFilter{
		Kinds: []prototypes.TaskKind{prototypes.TaskKind("bogus")},
	})
	if !errors.Is(err, ErrInvalidListFilter) {
		t.Fatalf("unknown kind error = %v, want ErrInvalidListFilter", err)
	}
}

// TestListFilterFromWire_PureFunction_ConcurrentReuse asserts the
// translator is a pure function safe for N≥100 concurrent invocations
// (D-025 / CLAUDE.md §11). The translator holds no artifact state; this
// test runs N=200 concurrent calls under -race and asserts every result
// is independent and correct.
func TestListFilterFromWire_PureFunction_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	const n = 200
	wire := &prototypes.TaskFilter{
		Kinds:        []prototypes.TaskKind{prototypes.TaskKindBackground},
		Statuses:     []prototypes.TaskStatus{prototypes.TaskStatusRunning},
		ParentTaskID: "task-parent-1",
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]TaskFilter, n)
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			rf, err := ListFilterFromWire(wire)
			results[idx] = rf
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent call %d error = %v", i, errs[i])
		}
		rf := results[i]
		if rf.Kind == nil || *rf.Kind != KindBackground {
			t.Fatalf("concurrent call %d Kind = %v, want background", i, rf.Kind)
		}
		if rf.Status == nil || *rf.Status != StatusRunning {
			t.Fatalf("concurrent call %d Status = %v, want running", i, rf.Status)
		}
		if rf.ParentID == nil || *rf.ParentID != TaskID("task-parent-1") {
			t.Fatalf("concurrent call %d ParentID = %v", i, rf.ParentID)
		}
	}
	// The input wire filter is never mutated by the translator.
	if len(wire.Kinds) != 1 || wire.Kinds[0] != prototypes.TaskKindBackground {
		t.Errorf("translator mutated its input wire filter: %+v", wire)
	}
}
