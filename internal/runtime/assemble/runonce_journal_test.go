package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

const journalKind = state.InternalKindPrefix + "session-execution-journal"

type journalClient struct {
	t     *testing.T
	store state.StateStore
	calls int
}

func (c *journalClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.calls++
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	head, err := c.store.Load(ctx, q, journalKind)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	var metadata struct {
		Query   string `json:"query"`
		Pending bool   `json:"pending"`
		Count   int    `json:"count"`
	}
	if err := json.Unmarshal(head.Bytes, &metadata); err != nil {
		return llm.CompleteResponse{}, err
	}
	if metadata.Query != "persist every boundary" || metadata.Pending {
		c.t.Fatal("decision preceded required admission or settlement")
	}
	if c.calls == 1 {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "write-1", Name: "journal_write", Args: json.RawMessage(`{}`)}}}, nil
	}
	record, err := c.store.Load(ctx, q, journalKind+"/action/000")
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	if metadata.Count != 1 || !strings.Contains(string(record.Bytes), `"settled":true`) ||
		!strings.Contains(string(record.Bytes), `"version":9007199254740993`) ||
		!strings.Contains(string(record.Bytes), `"more":false`) {
		c.t.Fatal("next inference preceded exact durable result")
	}
	return llm.CompleteResponse{Content: "done"}, nil
}
func (*journalClient) Close(context.Context) error { return nil }

type failingJournalStore struct {
	state.StateStore
	fail string
}

func (s *failingJournalStore) SaveBatchIf(ctx context.Context, conditions []state.SlotExpectation, writes []state.StateRecord) error {
	for _, record := range writes {
		if record.Kind == journalKind+"/action/000" {
			isSettlement := strings.Contains(string(record.Bytes), `"settled":true`)
			if (s.fail == "intent" && !isSettlement) || (s.fail == "settlement" && isSettlement) {
				return errors.New("injected dispatch checkpoint failure")
			}
		}
	}
	return s.StateStore.SaveBatchIf(ctx, conditions, writes)
}

func TestRunOnce_RetainedJournalDispatchBoundaries(t *testing.T) {
	for _, fail := range []string{"", "intent", "settlement"} {
		t.Run("fail="+fail, func(t *testing.T) {
			s := runnableStack(t)
			s.Cfg.Memory.Strategy, s.Cfg.Memory.RecentTurns = "rolling_summary", 2
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			store := &failingJournalStore{StateStore: s.State, fail: fail}
			s.State = store
			client := &journalClient{t: t, store: store}
			s.Planner = react.New(client)
			var toolsCalled atomic.Int64
			err := s.Catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{
				Name: "journal_write", Description: "Synthetic write", ArgsSchema: json.RawMessage(`{"type":"object"}`),
				Transport: tools.TransportInProcess, Source: "journal-test", Loading: tools.LoadingAlways,
			}, Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				q, ok := identity.QuadrupleFrom(ctx)
				if !ok {
					return tools.ToolResult{}, llm.ErrIdentityMissing
				}
				head, err := store.Load(ctx, q, journalKind)
				if err != nil {
					return tools.ToolResult{}, err
				}
				frame, err := store.Load(ctx, q, journalKind+"/action/000")
				if err != nil {
					return tools.ToolResult{}, err
				}
				if !strings.Contains(string(head.Bytes), `"pending":true`) || !strings.Contains(string(frame.Bytes), `"settled":false`) {
					t.Error("external action ran before committed intent")
				}
				toolsCalled.Add(1)
				return tools.ToolResult{Value: json.RawMessage(`{"saved":true,"version":9007199254740993,"more":false}`)}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "journal"}
			q := identity.Quadruple{Identity: id, RunID: "r"}
			_, err = s.RunOnce(t.Context(), "persist every boundary", id, assemble.WithRunID(q.RunID))
			if fail != "" {
				if err == nil || client.calls != 1 {
					t.Fatalf("persistence failure reached next decision: calls=%d err=%v", client.calls, err)
				}
				want := int64(0)
				if fail == "settlement" {
					want = 1
				}
				if toolsCalled.Load() != want {
					t.Fatal("failed persistence repeated or admitted an action")
				}
				head, e := store.Load(t.Context(), q, journalKind)
				if e != nil {
					t.Fatal(e)
				}
				if strings.Contains(string(head.Bytes), `"terminal":`) {
					t.Fatal("failed settlement was sealed as a terminal success")
				}
				window, e := store.Load(t.Context(), identity.Quadruple{Identity: id}, state.InternalKindPrefix+"session-execution-context")
				if e != nil || strings.Contains(string(window.Bytes), `"turns":`) {
					t.Fatal("terminal fallback bypassed failed journal write")
				}
				return
			}
			if err != nil || client.calls != 2 || toolsCalled.Load() != 1 {
				t.Fatalf("valid dispatch: %v", err)
			}
			for _, kind := range []string{journalKind, journalKind + "/action/000"} {
				if _, err := store.Load(t.Context(), q, kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("transient journal not cleaned: %v", err)
				}
			}
			window, err := store.Load(t.Context(), identity.Quadruple{Identity: id}, state.InternalKindPrefix+"session-execution-context")
			if err != nil || !strings.Contains(string(window.Bytes), `"version":9007199254740993`) {
				t.Fatal("terminal evidence not retained")
			}
		})
	}
}
