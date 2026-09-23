package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// A custom redactor can return raw JSON, not just the detached tree it receives.
// Reusing a prepared action must not remove strict validation of nested host
// metadata or allow redaction to change the identity of a settled operation.
type settlementShapeRedactor struct {
	shape  string
	intent bool
}

func (r settlementShapeRedactor) Redact(_ context.Context, value any) (any, error) {
	object, ok := value.(map[string]any)
	if !ok || (object["llm_observation"] == nil && !r.intent) {
		return value, nil
	}
	if r.shape == "changed action" {
		object["action"] = map[string]any{"Tool": "different", "CallID": "changed", "Args": map[string]any{}}
	}
	if r.shape == "changed args" {
		action, ok := object["action"].(map[string]any)
		if ok {
			action["Args"] = map[string]any{"redacted": true}
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	suffix := ""
	switch r.shape {
	case "duplicate nested host":
		suffix = `,"failure":{"code":"first","code":"second","message":"PRIVATE-FAILURE","attempts":1}`
	case "aliased nested host":
		suffix = `,"failure":{"Code":"second","message":"PRIVATE-FAILURE","attempts":1}`
	case "trailing":
		return json.RawMessage(string(encoded) + `{}`), nil
	}
	return json.RawMessage(string(encoded[:len(encoded)-1]) + suffix + "}"), nil
}

func TestRetainedJournal_PreparedSettlementPreservesValidation(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, shape := range []string{"valid", "duplicate nested host", "aliased nested host", "changed action", "trailing"} {
			t.Run(driver+"/"+shape, func(t *testing.T) {
				store, _, _ := retainedStore(t, driver)
				base := retainedBase("source", "prepared-settlement")
				run, err := sessionmemory.BeginRetainedRun(t.Context(), store, settlementShapeRedactor{shape: shape}, base.Quadruple, 2, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := run.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "read-exact", Args: json.RawMessage(`{"version":9007199254740993}`)}}
				if err := run.BeforeDispatch(t.Context(), base, step); err != nil {
					t.Fatal(err)
				}
				head := loadHostRecord(t, store, base.Quadruple, journalHeadKind)
				frame := loadHostRecord(t, store, base.Quadruple, journalHeadKind+"/action/000")
				// These similarly named result fields are opaque, not host authority.
				receipt := `{"source":"` + strings.Repeat("x", 14585) + `","version":9007199254740993,"more":false,"Code":"external","code":"data"}`
				step.LLMObservation = json.RawMessage(receipt)
				err = run.AfterDispatch(t.Context(), base, step)
				newHead := loadHostRecord(t, store, base.Quadruple, journalHeadKind)
				newFrame := loadHostRecord(t, store, base.Quadruple, journalHeadKind+"/action/000")
				if shape == "valid" {
					if err != nil || newHead.ID == head.ID || newFrame.ID == frame.ID {
						t.Fatalf("valid prepared settlement did not commit: %v", err)
					}
					for _, exact := range []string{strings.Repeat("x", 14585), `"version":9007199254740993`, `"more":false`, `"Code":"external"`, `"code":"data"`} {
						if !bytes.Contains(newFrame.Bytes, []byte(exact)) {
							t.Fatal("prepared settlement altered exact result evidence")
						}
					}
					return
				}
				if !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) || strings.Contains(err.Error(), "PRIVATE-FAILURE") {
					t.Fatalf("invalid settlement did not fail with a content-free error: %v", err)
				}
				if newHead.ID != head.ID || newFrame.ID != frame.ID || !bytes.Equal(newHead.Bytes, head.Bytes) || !bytes.Equal(newFrame.Bytes, frame.Bytes) {
					t.Fatal("rejected settlement mutated the committed intent")
				}
			})
		}
	}
}

// Time the real settlement path independently of admission and cleanup. The
// production in-memory driver, default redactor and exact receipt are unchanged.
func BenchmarkRetainedJournal_Settlement(b *testing.B) {
	store, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	redactor := auditpatterns.New()
	result := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		base := retainedBase(fmt.Sprint(i), "settlement-benchmark")
		run, err := sessionmemory.BeginRetainedRun(b.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err = run.Apply(&base); err != nil {
			b.Fatal(err)
		}
		if err = run.Start(b.Context(), base); err != nil {
			b.Fatal(err)
		}
		step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "r", Args: json.RawMessage(`{}`)}}
		if err = run.BeforeDispatch(b.Context(), base, step); err != nil {
			b.Fatal(err)
		}
		step.LLMObservation = result
		b.StartTimer()
		err = run.AfterDispatch(b.Context(), base, step)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		if _, err = store.DeleteScope(b.Context(), base.Quadruple.Identity); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func TestRetainedJournal_RejectsAmbiguousIntentBeforeDispatch(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, shape := range []string{"duplicate nested host", "changed action"} {
			t.Run(driver+"/"+shape, func(t *testing.T) {
				store, _, _ := retainedStore(t, driver)
				base := retainedBase("source", "ambiguous-intent")
				run, err := sessionmemory.BeginRetainedRun(t.Context(), store, settlementShapeRedactor{shape: shape, intent: true}, base.Quadruple, 2, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := run.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				head := loadHostRecord(t, store, base.Quadruple, journalHeadKind)
				step := planner.Step{Action: planner.CallTool{Tool: "write", CallID: "write", Args: json.RawMessage(`{}`)}}
				if err := run.BeforeDispatch(t.Context(), base, step); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
					t.Fatalf("ambiguous intent admitted an external action: %v", err)
				}
				after := loadHostRecord(t, store, base.Quadruple, journalHeadKind)
				if after.ID != head.ID || !bytes.Equal(after.Bytes, head.Bytes) {
					t.Fatal("rejection changed journal admission")
				}
				if _, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000"); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("rejection wrote an intent frame: %v", err)
				}
			})
		}
	}
}

func TestRetainedJournal_AllowsIntentContentRedaction(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, _, _ := retainedStore(t, driver)
			base := retainedBase("source", "redacted-intent")
			run, err := sessionmemory.BeginRetainedRun(t.Context(), store, settlementShapeRedactor{shape: "changed args", intent: true}, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = run.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = run.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := planner.Step{Action: planner.CallTool{Tool: "write", CallID: "write", Args: json.RawMessage(`{"secret":"value"}`)}}
			if err = run.BeforeDispatch(t.Context(), base, step); err != nil {
				t.Fatalf("argument redaction rejected before dispatch: %v", err)
			}
			step.LLMObservation = "done"
			if err = run.AfterDispatch(t.Context(), base, step); err != nil {
				t.Fatalf("argument redaction rejected at settlement: %v", err)
			}
			frame := loadHostRecord(t, store, base.Quadruple, journalHeadKind+"/action/000")
			if bytes.Contains(frame.Bytes, []byte("value")) || !bytes.Contains(frame.Bytes, []byte(`"redacted":true`)) {
				t.Fatal("journal did not preserve the redacted arguments")
			}
		})
	}
}
