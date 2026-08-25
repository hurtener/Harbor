// Package receipts is the public SDK facade for Harbor's transport-neutral,
// durable content-free usage receipt delivery seam.
package receipts

import (
	internal "github.com/hurtener/Harbor/internal/llm/receipts"
)

// Delivery sends a canonical usage receipt to a coordinator or other host.
type Delivery = internal.Delivery

// DeliveryAck identifies one exact canonical receipt accepted by a batch
// transport.
type DeliveryAck = internal.DeliveryAck

// BatchDelivery is the optional bounded-batch extension detected by Outbox.
type BatchDelivery = internal.BatchDelivery

// PendingReceiptSource supplies durable receipts to the outbox replay loop.
type PendingReceiptSource = internal.PendingReceiptSource

// Config configures the durable outbox.
type Config = internal.Config

// Outbox is Harbor's durable, bounded receipt delivery implementation.
type Outbox = internal.Outbox

// New constructs a durable receipt outbox.
var New = internal.New

// ErrInvalidReceipt identifies a malformed content-free receipt.
var ErrInvalidReceipt = internal.ErrInvalidReceipt

// ErrDeliveryFailed identifies a receipt transport failure.
var ErrDeliveryFailed = internal.ErrDeliveryFailed

// ErrCircuitOpen indicates that bounded delivery retry opened its circuit.
var ErrCircuitOpen = internal.ErrCircuitOpen

// ErrClosed indicates use after the receipt outbox was closed.
var ErrClosed = internal.ErrClosed
