package statestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestRetirementStrictDecoders_RejectNestedDuplicatesAndUnknownClass(t *testing.T) {
	duplicateTarget := base64.RawURLEncoding.EncodeToString([]byte(`{"tenant_id":"tenant","tenant_id":"other","user_id":"user","session_id":"session","kind":"kind","agent_id":"agent"}`))
	if _, err := decodeRetirementSessionTarget(duplicateTarget); !errors.Is(err, agentcfg.ErrRetirementConflict) {
		t.Fatalf("duplicate nested target=%v", err)
	}

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background())
	r := &registry{state: store}
	q := syntheticQuad("tenant", "agent")
	const operation = "strict-operation"
	retirement := &retirementRecord{OperationID: operation, ManifestCount: 1, ManifestFrozen: true, ManifestDigest: emptyRetirementManifestDigest(), CleanupDigest: emptyRetirementManifestDigest()}
	kind := retirementManifestKind(agentcfg.RetirementOperationHash(operation), 0)
	for _, body := range []string{
		`{"schema":1,"operation_hash":"` + agentcfg.RetirementOperationHash(operation) + `","ordinal":0,"class":"session_personal","class":"oauth_provider","resource":"opaque","prior_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
		`{"schema":1,"operation_hash":"` + agentcfg.RetirementOperationHash(operation) + `","ordinal":0,"class":"future_cleanup","resource":"opaque","prior_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
		`{"schema":1,"operation_hash":"` + agentcfg.RetirementOperationHash(operation) + `","ordinal":0,"class":"session_personal","resource":{"target":"opaque","target":"other"},"prior_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
	} {
		if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: []byte(body)}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.loadRetirementManifestItem(context.Background(), q, retirement, 0); err == nil {
			t.Fatalf("invalid manifest accepted: %s", body)
		}
	}
}

func TestRetirementManifestTamper_RecomputedSelfDigestFailsBeforeProjectionAndProgress(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(map[int]string{1: "final", 2: "current"}[count], func(t *testing.T) {
			ctx := context.Background()
			store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close(ctx)
			bus, err := eventsinmem.New(config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 4, SubscriberBufferSize: 16, IdleTimeout: time.Second, DropWindow: time.Second}, auditpatterns.New())
			if err != nil {
				t.Fatal(err)
			}
			defer bus.Close(ctx)
			newRegistry := func() *registry {
				return &registry{state: store, bus: bus, clock: time.Now, logger: slog.Default()}
			}
			r := newRegistry()
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-tamper", UserID: "admin", SessionID: "control"}}
			servers := make([]agentcfg.MCPConnectionDescriptor, 0, count)
			for i := range count {
				servers = append(servers, agentcfg.MCPConnectionDescriptor{Name: "owned-" + string(rune('a'+i))})
			}
			revision, err := r.SetRevision(ctx, id, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: servers}}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			operation := map[int]string{1: "tamper-final", 2: "tamper-current"}[count]
			status, err := r.Retire(ctx, id, "agent", agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
			if err != nil || len(status.Cleanup) != 1 {
				t.Fatalf("retire=(%+v,%v)", status, err)
			}
			q := syntheticQuad(id.TenantID, "agent")
			kind := retirementManifestKind(agentcfg.RetirementOperationHash(operation), 0)
			record, err := store.Load(ctx, q, kind)
			if err != nil {
				t.Fatal(err)
			}
			var item retirementManifestItem
			if err := json.Unmarshal(record.Bytes, &item); err != nil {
				t.Fatal(err)
			}
			item.Class = "oauth_provider"
			item.Resource = "well-shaped-altered-resource"
			item.Digest, err = advanceRetirementManifestDigest(item.PriorDigest, item)
			if err != nil {
				t.Fatal(err)
			}
			item.Successor.ManifestDigest = item.Digest
			altered, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: altered}); err != nil {
				t.Fatal(err)
			}
			restarted := newRegistry()
			if _, _, err := restarted.RetirementStatus(ctx, id, "agent"); !errors.Is(err, agentcfg.ErrRetirementConflict) {
				t.Fatalf("tampered restarted status=%v, want conflict", err)
			}
			if _, err := restarted.CompleteRetirementStep(ctx, id, "agent", operation, item.Class, item.Resource); !errors.Is(err, agentcfg.ErrRetirementConflict) {
				t.Fatalf("tampered completion=%v, want conflict", err)
			}
			lifecycle, _, _, err := restarted.loadActiveRecord(ctx, q)
			if err != nil || lifecycle.Retirement.CleanupCompleted != 0 || lifecycle.Retirement.ScrubCompleted != 0 {
				t.Fatalf("tamper advanced lifecycle=(%+v,%v)", lifecycle.Retirement, err)
			}
		})
	}
}
