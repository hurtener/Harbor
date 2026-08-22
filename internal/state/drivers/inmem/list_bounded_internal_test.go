package inmem

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/state"
)

func TestSelectBoundedIndexKeys_RetainsOnlyLimitAndOrdersDeterministically(t *testing.T) {
	records := make(map[indexKey]state.StateRecord, 1001)
	for i := range 1000 {
		key := indexKey{
			Tenant:  "tenant",
			User:    "user",
			Session: fmt.Sprintf("session-%04d", i),
			Kind:    fmt.Sprintf("bounded.match.%04d", i),
		}
		records[key] = state.StateRecord{}
	}
	records[indexKey{Tenant: "tenant", User: "user", Session: "session-0000", Kind: "other"}] = state.StateRecord{}

	got, err := selectBoundedIndexKeys(context.Background(), records, "bounded.match.", 3)
	if err != nil {
		t.Fatalf("selectBoundedIndexKeys: %v", err)
	}
	if got.maxRetained != 3 {
		t.Fatalf("max retained keys = %d, want exactly limit 3", got.maxRetained)
	}
	want := []indexKey{
		{Tenant: "tenant", User: "user", Session: "session-0000", Kind: "bounded.match.0000"},
		{Tenant: "tenant", User: "user", Session: "session-0001", Kind: "bounded.match.0001"},
		{Tenant: "tenant", User: "user", Session: "session-0002", Kind: "bounded.match.0002"},
	}
	if !reflect.DeepEqual(got.keys, want) {
		t.Fatalf("selected keys = %+v, want %+v", got.keys, want)
	}
	repeated, err := selectBoundedIndexKeys(context.Background(), records, "bounded.match.", 3)
	if err != nil {
		t.Fatalf("repeat selectBoundedIndexKeys: %v", err)
	}
	if !reflect.DeepEqual(repeated, got) {
		t.Fatalf("selection is not deterministic:\nfirst=%+v\nrepeat=%+v", got, repeated)
	}
}
