package allocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm/pricing"
	"github.com/hurtener/Harbor/internal/state"
)

// BindPricingCatalog pins each immutable ID/revision to its first content hash
// in reserved runtime coordination state. A restart or concurrent runtime cannot
// silently reuse a revision for another tariff. Removed revisions stay pinned.
// No caller identity, task content, credential or production price is stored.
func BindPricingCatalog(ctx context.Context, st state.StateStore, catalog *pricing.Catalog) error {
	for _, ref := range catalog.References() {
		if st == nil {
			return pricing.ErrUnavailable
		}
		identityBytes, err := json.Marshal(struct {
			ID       string
			Revision uint64
		}{ref.ID, ref.Revision})
		if err != nil {
			return fmt.Errorf("pricing identity: %w", err)
		}
		digest := sha256.Sum256(identityBytes)
		kind := "harbor.internal/inference.pricing/" + hex.EncodeToString(digest[:])
		q := identity.InternalCoordinationQuadruple()
		for {
			rec, loadErr := st.Load(ctx, q, kind)
			if loadErr == nil {
				if string(rec.Bytes) != ref.SHA256 {
					return fmt.Errorf("%w: pricing revision changed", pricing.ErrUnavailable)
				}
				break
			}
			if !errors.Is(loadErr, state.ErrNotFound) {
				return fmt.Errorf("load pricing version: %w", loadErr)
			}
			rec = state.NewInternalRecord(state.NewEventID(), q, kind, []byte(ref.SHA256))
			err = st.SaveBatchIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, "")}, []state.StateRecord{rec})
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			if err != nil {
				return fmt.Errorf("pin pricing version: %w", err)
			}
			break
		}
	}
	return nil
}
