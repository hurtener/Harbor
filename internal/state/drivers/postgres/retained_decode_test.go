package postgres_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

func TestPostgres_RetainedContext_RejectsAmbiguousHostEncoding(t *testing.T) {
	for _, replacement := range []struct{ from, to string }{
		{`"pending":false`, `"pending":true,"pending":false`},
		{`"pending":false`, `"Pending":false`},
		{`"run_id":"source"`, `"run_id":"foreign","run_id":"source"`},
		{`"run_id":"source"`, `"RUN_ID":"source"`},
	} {
		t.Run(replacement.to, func(t *testing.T) {
			writer, reader, redactor := retainedPostgresStores(t)
			base := retainedBase("source", "ambiguous-host")
			r, err := runctx.BeginRetainedRun(t.Context(), writer, redactor, base.Quadruple, 4, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err := r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := journalAction(t, r, base)
			if err := r.AfterDispatch(t.Context(), base, step); err != nil {
				t.Fatal(err)
			}
			head, err := reader.Load(t.Context(), base.Quadruple, journalHeadKind)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := bytes.Replace(head.Bytes, []byte(replacement.from), []byte(replacement.to), 1)
			if bytes.Equal(corrupt, head.Bytes) {
				t.Fatal("fixture did not modify journal metadata")
			}
			if err := reader.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(base.Quadruple, journalHeadKind, head.ID)}, state.NewInternalRecord(state.NewEventID(), base.Quadruple, journalHeadKind, corrupt)); err != nil {
				t.Fatal(err)
			}
			session := identity.Quadruple{Identity: base.Quadruple.Identity}
			kind := state.InternalKindPrefix + "session-execution-context"
			before, err := writer.Load(t.Context(), session, kind)
			if err != nil {
				t.Fatal(err)
			}
			if err := runctx.ReconcileRetainedRun(t.Context(), reader, redactor, base.Quadruple, 4, nil); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
				t.Fatalf("ambiguous journal accepted: %v", err)
			}
			after, err := writer.Load(t.Context(), session, kind)
			if err != nil || after.ID != before.ID || !bytes.Equal(after.Bytes, before.Bytes) {
				t.Fatalf("rejection mutated the retained window: %v", err)
			}
		})
	}
}
