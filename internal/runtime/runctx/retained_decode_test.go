package runctx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

// A last-key-wins decoder must not choose whether a persisted action settled.
// Corrupted host envelopes are rejected before admission or reconciliation can
// mutate state. Result payloads remain opaque evidence, tested separately.
func TestRetainedRecovery_AmbiguousHostEncoding(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, scenario := range []string{"duplicate pending", "aliased pending", "duplicate admission", "aliased admission", "duplicate window version", "aliased window version", "duplicate frame settlement"} {
			t.Run(driver+"/"+scenario, func(t *testing.T) {
				store, redactor, _ := retainedStore(t, driver)
				base := retainedBase("source", "ambiguous")
				r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := r.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				journalAction(t, r, base, true)
				session := identity.Quadruple{Identity: base.Quadruple.Identity}
				head := loadHostRecord(t, store, base.Quadruple, journalHeadKind)
				switch scenario {
				case "duplicate pending":
					head.Bytes = bytes.Replace(head.Bytes, []byte(`"pending":false`), []byte(`"pending":true,"pending":false`), 1)
				case "aliased pending":
					head.Bytes = bytes.Replace(head.Bytes, []byte(`"pending":`), []byte(`"Pending":`), 1)
				case "duplicate admission":
					head.Bytes = bytes.Replace(head.Bytes, []byte(`"run_id":"source"`), []byte(`"run_id":"foreign","run_id":"source"`), 1)
				case "aliased admission":
					head.Bytes = bytes.Replace(head.Bytes, []byte(`"run_id":`), []byte(`"RUN_ID":`), 1)
				case "duplicate window version", "aliased window version":
					window := loadHostRecord(t, store, session, retainedKind)
					if scenario == "duplicate window version" {
						window.Bytes = bytes.Replace(window.Bytes, []byte(`"version":3`), []byte(`"version":999,"version":3`), 1)
					} else {
						window.Bytes = bytes.Replace(window.Bytes, []byte(`"version":`), []byte(`"VERSION":`), 1)
					}
					replaceHostRecord(t, store, window)
				case "duplicate frame settlement":
					frame := loadHostRecord(t, store, base.Quadruple, journalHeadKind+"/action/000")
					before := len(frame.Bytes)
					frame.Bytes = bytes.Replace(frame.Bytes, []byte(`"settled":true`), []byte(`"settled":false,"settled":true`), 1)
					// Keep the byte accounting consistent so the encoding itself,
					// not an unrelated size mismatch, must trigger rejection.
					var counts map[string]json.RawMessage
					if err := json.Unmarshal(head.Bytes, &counts); err != nil {
						t.Fatal(err)
					}
					var count int
					if err := json.Unmarshal(counts["bytes"], &count); err != nil {
						t.Fatal(err)
					}
					counts["bytes"], err = json.Marshal(count + len(frame.Bytes) - before)
					if err != nil {
						t.Fatal(err)
					}
					head.Bytes, err = json.Marshal(counts)
					if err != nil {
						t.Fatal(err)
					}
					replaceHostRecord(t, store, frame)
				}
				replaceHostRecord(t, store, head)
				before := loadHostRecord(t, store, session, retainedKind)
				if err := runctx.ReconcileRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, nil); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
					t.Errorf("ambiguous durable authority accepted: %v", err)
				}
				after := loadHostRecord(t, store, session, retainedKind)
				if after.ID != before.ID || !bytes.Equal(after.Bytes, before.Bytes) {
					t.Error("rejection mutated the retained window")
				}
			})
		}
	}
}

func loadHostRecord(t *testing.T, store state.StateStore, q identity.Quadruple, kind string) state.StateRecord {
	t.Helper()
	record, err := store.Load(t.Context(), q, kind)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func replaceHostRecord(t *testing.T, store state.StateStore, record state.StateRecord) {
	t.Helper()
	if err := store.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(record.Identity, record.Kind, record.ID)}, state.NewInternalRecord(state.NewEventID(), record.Identity, record.Kind, record.Bytes)); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedContext_OpaqueResultKeysRemainData(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("source", "opaque")
	r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: map[string]any{"Pending": "external field", "VERSION": json.Number("9007199254740993"), "pending": false}})
	if err := r.Finish(context.Background(), base.Trajectory, base.Query, "done", "complete"); err != nil {
		t.Fatal(err)
	}
	next := retainedBase("next", "opaque")
	r, err = runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&next); err != nil {
		t.Fatal(err)
	}
	body := encodeRetained(t, next)
	for _, expected := range []string{`"Pending":"external field"`, `"VERSION":9007199254740993`, `"pending":false`} {
		if !bytes.Contains([]byte(body), []byte(expected)) {
			t.Fatalf("opaque evidence changed: %s", expected)
		}
	}
}
