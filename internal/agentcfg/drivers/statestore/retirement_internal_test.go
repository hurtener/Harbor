package statestore

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
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
