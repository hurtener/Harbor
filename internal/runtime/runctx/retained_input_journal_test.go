package runctx_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

func TestRetainedJournal_InputReferencesAtomicAndRecoverable(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("original", "input-journal")
			base.InputArtifacts = []planner.InputArtifactView{{ID: "source-ref", Bytes: []byte("PRIVATE-IMAGE-BYTES"), MIME: "image/png"}}
			r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			head, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
			if err != nil || !strings.Contains(string(head.Bytes), `"count":1`) || strings.Contains(string(head.Bytes), `"pending":true`) {
				t.Fatalf("input not settled atomically: %v", err)
			}
			frame, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000")
			if err != nil || !strings.Contains(string(frame.Bytes), "source-ref") || strings.Contains(string(frame.Bytes), "PRIVATE-IMAGE-BYTES") {
				t.Fatalf("input bytes or identity wrong: %v", err)
			}
			if driver == "sqlite" {
				if err = store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			if err = runctx.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); err != nil {
				t.Fatal(err)
			}
			next := retainedBase("next", "input-journal")
			r, err = runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&next); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(encodeRetained(t, next), "source-ref") {
				t.Fatal("recovered query lost supplied attachment")
			}
		})
	}
}

type inputAtomicStore struct {
	state.StateStore
	writes []state.StateRecord
}

func (s *inputAtomicStore) SaveBatchIf(_ context.Context, _ []state.SlotExpectation, writes []state.StateRecord) error {
	s.writes = append([]state.StateRecord(nil), writes...)
	return errors.New("injected atomic write failure")
}
func TestRetainedJournal_InputAdmissionFailureCannotLeaveQueryOnlyHead(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	fail := &inputAtomicStore{StateStore: store}
	base := retainedBase("original", "input-atomic")
	base.InputArtifacts = []planner.InputArtifactView{{ID: "source"}}
	r, err := runctx.BeginRetainedRun(t.Context(), fail, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = r.Start(t.Context(), base); err == nil {
		t.Fatal("failed atomic admission accepted")
	}
	if len(fail.writes) != 2 {
		t.Fatalf("query and initial input not written together: %d", len(fail.writes))
	}
	for _, write := range fail.writes {
		if _, err := store.Load(t.Context(), write.Identity, write.Kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatal("failed admission leaked partial head/frame")
		}
	}
}
