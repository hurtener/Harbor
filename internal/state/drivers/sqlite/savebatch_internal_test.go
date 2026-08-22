package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

func TestSaveBatchIf_WriteStageFailureRollsBackEveryPosition(t *testing.T) {
	for failPosition := range 3 {
		t.Run(string(rune('1'+failPosition)), func(t *testing.T) {
			opened, err := New(config.StateConfig{Driver: "sqlite", DSN: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			d := opened.(*driver)
			defer func() { _ = d.Close(context.Background()) }()
			kind := "batch.fail"
			if _, err := d.db.Exec(`CREATE TRIGGER fail_batch BEFORE INSERT ON state_records WHEN NEW.kind = '` + kind + `' BEGIN SELECT RAISE(ABORT, 'injected batch write failure'); END`); err != nil {
				t.Fatal(err)
			}
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			writes := make([]state.StateRecord, 3)
			expects := make([]state.SlotExpectation, 3)
			for i := range writes {
				writeKind := "batch.ok." + string(rune('1'+i))
				if i == failPosition {
					writeKind = kind
				}
				writes[i] = state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: writeKind, Bytes: []byte{byte(i)}}
				expects[i] = state.SlotExpectation{Identity: id, Kind: writeKind}
			}
			if err := d.SaveBatchIf(context.Background(), expects, writes); err == nil {
				t.Fatal("SaveBatchIf succeeded across injected write failure")
			}
			for _, write := range writes {
				if _, err := d.Load(context.Background(), id, write.Kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("partial write visible for %q: %v", write.Kind, err)
				}
			}
		})
	}
}
