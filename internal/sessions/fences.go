package sessions

import (
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/identity"
)

// ErasurePendingSlot returns the durable pending-erasure slot for id. The
// record lives in the protected observability scope so it survives deletion
// of the erased session's ordinary StateStore scope.
func ErasurePendingSlot(id identity.Quadruple) (identity.Quadruple, string, error) {
	return sessionfence.PendingSlot(id)
}

// ErasureTombstoneSlot returns the durable terminal-erasure slot for id.
func ErasureTombstoneSlot(id identity.Quadruple) (identity.Quadruple, string, error) {
	return sessionfence.TombstoneSlot(id)
}
