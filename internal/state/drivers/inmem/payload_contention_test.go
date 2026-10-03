package inmem_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// Readers must own detached payloads even when another call replaces or deletes
// the generation they observed. Exercise the same shared store from 128 scopes
// while callers mutate their returned copies and the next generation publishes.
func TestInMem_ConcurrentPayloadSnapshots(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	const peers = 128
	start := make(chan struct{})
	var workers sync.WaitGroup
	for peer := range peers {
		workers.Go(func() {
			<-start
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprint(peer)}}
			var previous state.EventID
			var previousSnapshot []byte
			for generation := range 8 {
				payload := bytes.Repeat([]byte{byte(generation + 1)}, 32<<10)
				record := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: "payload", Bytes: payload}
				if err := store.SaveIf(t.Context(), []state.SlotExpectation{{Identity: q, Kind: record.Kind, ExpectedEventID: previous}}, record); err != nil {
					t.Error(err)
					return
				}
				previous = record.ID
				if previousSnapshot != nil && (previousSnapshot[0] != byte(generation) || previousSnapshot[len(previousSnapshot)-1] != byte(generation)) {
					t.Error("replacement mutated a returned prior snapshot")
					return
				}
				payload[0] = 0 // The caller retains ownership after SaveIf returns.
				loaded, err := store.Load(t.Context(), q, record.Kind)
				if err != nil || loaded.ID != record.ID || len(loaded.Bytes) != 32<<10 || loaded.Bytes[0] != byte(generation+1) {
					t.Errorf("load mixed or aliased generation: %v", err)
					return
				}
				loaded.Bytes[0] = 0
				byID, err := store.LoadByEventID(t.Context(), record.ID)
				if err != nil || byID.ID != record.ID || len(byID.Bytes) != 32<<10 || byID.Bytes[0] != byte(generation+1) {
					t.Errorf("event lookup mixed or aliased generation: %v", err)
					return
				}
				if byID.Bytes[len(byID.Bytes)-1] != byte(generation+1) {
					t.Error("snapshot contained bytes from another generation")
					return
				}
				previousSnapshot = byID.Bytes
			}
			if deleted, err := store.DeleteIf(t.Context(), state.SlotExpectation{Identity: q, Kind: "payload", ExpectedEventID: previous}); err != nil || !deleted {
				t.Errorf("conditional cleanup failed: deleted=%t error=%v", deleted, err)
			}
			if previousSnapshot[0] != 8 || previousSnapshot[len(previousSnapshot)-1] != 8 {
				t.Error("deletion mutated a returned snapshot")
			}
			if _, err := store.LoadByEventID(t.Context(), previous); !errors.Is(err, state.ErrNotFound) {
				t.Errorf("deleted generation remained indexed: %v", err)
			}
		})
	}
	close(start)
	workers.Wait()
}

func TestInMem_BatchPayloadOwnership(t *testing.T) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "batch-ownership"}}
	payload := bytes.Repeat([]byte("x"), 32<<10)
	writes := []state.StateRecord{
		{ID: state.NewEventID(), Identity: q, Kind: "first", Bytes: payload},
		{ID: state.NewEventID(), Identity: q, Kind: "second", Bytes: payload},
	}
	if err := store.SaveBatchIf(t.Context(), []state.SlotExpectation{{Identity: q, Kind: "first"}, {Identity: q, Kind: "second"}}, writes); err != nil {
		t.Fatal(err)
	}
	for _, write := range writes {
		if &write.Bytes[0] != &payload[0] {
			t.Fatal("store replaced the caller's batch slice headers")
		}
	}
	payload[0] = 0
	for _, write := range writes {
		loaded, err := store.Load(t.Context(), q, write.Kind)
		if err != nil || loaded.ID != write.ID || len(loaded.Bytes) != len(payload) || loaded.Bytes[0] != 'x' {
			t.Fatalf("caller input mutation reached stored batch: %v", err)
		}
		loaded.Bytes[0] = 0
	}
	for _, write := range writes {
		loaded, err := store.LoadByEventID(t.Context(), write.ID)
		if err != nil || len(loaded.Bytes) != len(payload) || loaded.Bytes[0] != 'x' {
			t.Fatalf("returned snapshot mutation reached stored batch: %v", err)
		}
	}
}

// The journal-shaped sequence exposes time spent handing the global map lock
// between conditional publication, payload reads and exact-generation cleanup.
// Run with GOMAXPROCS=2 for 128 workers, matching retained runtime stress.
func BenchmarkInMem_ConditionalPayloadContention(b *testing.B) {
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	payload := bytes.Repeat([]byte("x"), 14660)
	var serial atomic.Uint64
	b.SetParallelism(64)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprint(serial.Add(1))}}
		for pb.Next() {
			record := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: "payload", Bytes: payload}
			head := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: "head", Bytes: []byte(`{"sealed":true}`)}
			if err := store.SaveBatchIf(b.Context(), []state.SlotExpectation{{Identity: q, Kind: record.Kind}, {Identity: q, Kind: head.Kind}}, []state.StateRecord{record, head}); err != nil {
				b.Error(err)
				return
			}
			if _, err := store.Load(b.Context(), q, record.Kind); err != nil {
				b.Error(err)
				return
			}
			for _, value := range []state.StateRecord{record, head} {
				if deleted, err := store.DeleteIf(b.Context(), state.SlotExpectation{Identity: q, Kind: value.Kind, ExpectedEventID: value.ID}); err != nil || !deleted {
					b.Errorf("conditional cleanup failed: deleted=%t error=%v", deleted, err)
					return
				}
			}
		}
	})
}
