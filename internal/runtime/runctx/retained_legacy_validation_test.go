package runctx_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

func TestRetainedContext_LegacyEvidenceValidation(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, version := range []int{1, 2} {
			for _, body := range []string{
				`{"action":{},"reasoning_trace":"PRIVATE"}`,
				`{"action":{},"observation":"PRIVATE"}`,
				`{"action":{},"streams":{}}`,
				`{"action":{},"Action":{"Tool":"write"}}`,
				`{"action":{},"action":{"Tool":"write"}}`,
				`null`,
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", driver, version, body), func(t *testing.T) {
					store, redactor, _ := retainedStore(t, driver)
					base := retainedBase("new", "legacy-unsafe")
					q := identity.Quadruple{Identity: base.Quadruple.Identity}
					window, err := json.Marshal(map[string]any{
						"version": version,
						"turns": []any{map[string]any{
							"admission":  map[string]any{"id": state.NewEventID(), "run_id": "old"},
							"expires_at": time.Now().Add(time.Hour), "status": "complete", "query": "old request",
							"steps": []json.RawMessage{json.RawMessage(body)},
						}},
					})
					if err != nil {
						t.Fatal(err)
					}
					record := state.NewInternalRecord(state.NewEventID(), q, retainedKind, window)
					if err = store.Save(t.Context(), record); err != nil {
						t.Fatal(err)
					}
					run, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
					if !errors.Is(err, runctx.ErrRetainedContextUnavailable) || run != nil {
						t.Fatalf("unsafe legacy evidence admitted: %v", err)
					}
					if strings.Contains(err.Error(), "PRIVATE") {
						t.Fatal("error leaked historical data")
					}
					after, err := store.Load(t.Context(), q, retainedKind)
					if err != nil || after.ID != record.ID {
						t.Fatal("failed legacy validation changed admission state")
					}
				})
			}
		}
	}
}
