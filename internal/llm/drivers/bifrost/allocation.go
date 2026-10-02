package bifrost

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
)

// ProviderAttemptBound covers configured network retries plus the pinned
// transport's single encrypted-reasoning fail-soft retry. No fallback list is
// emitted by Harbor's request translator. Unreported intermediate usage remains
// held even when the final attempt returns measured usage.
func (d *Driver) ProviderAttemptBound(ctx context.Context, req llm.CompleteRequest) (int, error) {
	if _, routed := llm.TrustedProviderRouteFrom(ctx); routed {
		return 2, nil
	}
	if d.account == nil {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	cfg, err := d.account.GetConfigForProvider(d.provider)
	if err != nil {
		return 0, fmt.Errorf("allocation provider bounds: %w", err)
	}
	if cfg.NetworkConfig.MaxRetries < 0 || cfg.NetworkConfig.MaxRetries > 998 {
		return 0, llm.ErrAllocationBoundUnavailable
	}
	return cfg.NetworkConfig.MaxRetries + 2, nil
}
