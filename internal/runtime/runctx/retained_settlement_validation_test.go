package runctx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

type settlementRedactor struct {
	audit.Redactor
	extra string
}

func (r settlementRedactor) Redact(ctx context.Context, value any) (any, error) {
	safe, err := r.Redactor.Redact(ctx, value)
	if err != nil {
		return nil, err
	}
	if step, ok := safe.(map[string]any); ok && step["llm_observation"] != nil {
		body, err := json.Marshal(safe)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(append(body[:len(body)-1], []byte(","+r.extra+"}")...)), nil
	}
	return safe, nil
}

func TestRetainedJournal_SettlementKeepsStrictRedactorMetadata(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			for _, extra := range []string{
				`"action":{}`,
				`"Action":{}`,
				`"failure":{"code":"first","code":"second"}`,
				`"failure":{"Code":"aliased"}`,
			} {
				t.Run(extra, func(t *testing.T) {
					store, redactor, _ := retainedStore(t, driver)
					base := retainedBase("source", "redactor")
					r, err := runctx.BeginRetainedRun(t.Context(), store, settlementRedactor{Redactor: redactor, extra: extra}, base.Quadruple, 2, time.Hour, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err = r.Apply(&base); err != nil {
						t.Fatal(err)
					}
					if err = r.Start(t.Context(), base); err != nil {
						t.Fatal(err)
					}
					step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "read-1", Args: json.RawMessage(`{}`)}}
					if err = r.BeforeDispatch(t.Context(), base, step); err != nil {
						t.Fatal(err)
					}
					before, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
					if err != nil {
						t.Fatal(err)
					}
					step.LLMObservation = json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
					if err = r.AfterDispatch(t.Context(), base, step); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
						t.Fatalf("ambiguous redactor output accepted: %v", err)
					}
					after, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
					if err != nil || before.ID != after.ID || !bytes.Equal(before.Bytes, after.Bytes) {
						t.Fatal("rejected settlement mutated the pending journal")
					}
				})
			}
		})
	}
}

// Return corrupt bytes under the same generation to model storage corruption,
// not a concurrent writer (which already changes the generation and is fenced).
type corruptIntentStore struct {
	state.StateStore
	corrupt func([]byte) []byte
}

func (s *corruptIntentStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	if err == nil && s.corrupt != nil && kind == journalHeadKind+"/action/000" {
		record.Bytes = s.corrupt(record.Bytes)
	}
	return record, err
}

func TestRetainedJournal_SettlementRejectsCorruptIntentBounds(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			for _, scenario := range []string{"different expiry", "oversized intent"} {
				t.Run(scenario, func(t *testing.T) {
					store, redactor, _ := retainedStore(t, driver)
					wrapper := &corruptIntentStore{StateStore: store}
					base := retainedBase("source", "corrupt-intent")
					r, err := runctx.BeginRetainedRun(t.Context(), wrapper, redactor, base.Quadruple, 2, time.Hour, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err = r.Apply(&base); err != nil {
						t.Fatal(err)
					}
					if err = r.Start(t.Context(), base); err != nil {
						t.Fatal(err)
					}
					step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "read-1", Args: json.RawMessage(`{}`)}}
					if err = r.BeforeDispatch(t.Context(), base, step); err != nil {
						t.Fatal(err)
					}
					before, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
					if err != nil {
						t.Fatal(err)
					}
					wrapper.corrupt = func(body []byte) []byte {
						if scenario == "oversized intent" {
							return append(bytes.Clone(body), bytes.Repeat([]byte(" "), 512*1024)...)
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(body, &fields); err != nil {
							t.Fatal(err)
						}
						fields["expires_at"] = json.RawMessage(`"2000-01-01T00:00:00Z"`)
						data, err := json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
						return data
					}
					step.LLMObservation = "done"
					if err = r.AfterDispatch(t.Context(), base, step); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
						t.Fatalf("corrupt intent not rejected as unavailable before settlement: %v", err)
					}
					after, err := store.Load(t.Context(), base.Quadruple, journalHeadKind)
					if err != nil || before.ID != after.ID || !bytes.Equal(before.Bytes, after.Bytes) {
						t.Fatal("corrupt intent changed the journal")
					}
				})
			}
		})
	}
}
