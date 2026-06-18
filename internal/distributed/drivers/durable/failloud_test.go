package durable_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
)

// TestDurable_NilStore_FailsLoud asserts the durable bus refuses to
// start without a StateStore (CLAUDE.md §13 "no silent stub default").
func TestDurable_NilStore_FailsLoud(t *testing.T) {
	eb := newEventBus(t)
	defer func() { _ = eb.Close(context.Background()) }()
	_, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: eb,
		State:    nil,
		Cfg:      config.DistributedConfig{BusDriver: "durable"},
	})
	if err == nil {
		t.Fatal("durable bus with nil StateStore: want fail-loud error, got nil")
	}
	if !strings.Contains(err.Error(), "StateStore") {
		t.Errorf("error should name the missing StateStore, got: %v", err)
	}
}

// TestDurable_NilEventBus_FailsLoud asserts the durable bus refuses to
// start without an EventBus to project onto.
func TestDurable_NilEventBus_FailsLoud(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	_, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: nil,
		State:    store,
		Cfg:      config.DistributedConfig{BusDriver: "durable"},
	})
	if err == nil {
		t.Fatal("durable bus with nil EventBus: want fail-loud error, got nil")
	}
	if !strings.Contains(err.Error(), "EventBus") {
		t.Errorf("error should name the missing EventBus, got: %v", err)
	}
}

// TestDurable_Unserializable_FailsLoud asserts a non-serializable
// envelope payload surfaces a loud error rather than being dropped.
func TestDurable_Unserializable_FailsLoud(t *testing.T) {
	store := newStore(t)
	eb := newEventBus(t)
	b := openOver(t, store, eb)
	defer func() {
		ctx := context.Background()
		_ = b.Close(ctx)
		_ = eb.Close(ctx)
		_ = store.Close(ctx)
	}()

	id := triple()
	bad := envelope("x", id, "evt-bad")
	bad.Payload = []byte("{not valid json") // invalid json.RawMessage → marshal fails
	bad.Timestamp = time.Now().UTC()

	if err := b.Publish(ctxFor(t, id.Identity), bad); err == nil {
		t.Fatal("publish of unserializable envelope: want error, got nil")
	}
}
