// Package eventbus is the public SDK facade over Harbor's
// internal/telemetry/eventbus package — the production BusEmitter
// adapter that pairs Logger.Error calls with runtime.error events on
// the canonical bus (RFC §3.6, §6.14). Alias-based
// re-exports only: no behavior lives here. Ships
// alongside sdk/telemetry (the manual-composition recipe path needs
// the adapter to wire telemetry.WithBusEmitter).
package eventbus

import (
	internal "github.com/hurtener/Harbor/internal/telemetry/eventbus"
)

// Adapter is the production BusEmitter over an events.EventBus.
type Adapter = internal.Adapter

// New wraps an events.EventBus as the Logger's BusEmitter. A nil bus
// returns nil (the Logger treats a nil emitter as no-op).
var New = internal.New
