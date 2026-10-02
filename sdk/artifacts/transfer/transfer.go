// Package transfer exposes the generic, recipient-bound artifact authority
// signer. Protocol clients transport grants and receipts, never artifact bytes.
package transfer

import (
	internal "github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// Grant authorizes one exact source-to-recipient artifact copy.
type Grant = types.ArtifactTransferGrant

// Endpoint is an exact runtime audience and tenant/user/session owner.
type Endpoint = types.ArtifactTransferEndpoint

// Receipt is the durable content-free transfer result.
type Receipt = types.ArtifactTransferReceipt

// Sign signs complete immutable authority with the caller's Ed25519 key.
var Sign = internal.Sign
