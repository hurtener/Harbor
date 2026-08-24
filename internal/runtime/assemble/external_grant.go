package assemble

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/grant"
	"github.com/hurtener/Harbor/internal/llm/leases"
	llmreceipts "github.com/hurtener/Harbor/internal/llm/receipts"
	"github.com/hurtener/Harbor/internal/state"
)

// wireExternalGrant composes the production runtime grant boundary. The
// public-key verifier and optional authorized-organization allowlist come from
// boot configuration; signed grants remain the authority for the organization
// on each call. The secret-bearing resolver and coordinator delivery remain
// host-owned injection seams. This keeps Harbor from accepting caller-selected
// provider credentials while still giving the configured runtime a real
// consumer.
func wireExternalGrant(
	ctx context.Context,
	settings config.LLMExternalGrantConfig,
	provided llm.ExternalGrantConfig,
	store state.StateStore,
	delivery llmreceipts.Delivery,
	pending llmreceipts.PendingReceiptSource,
	maxBatch int,
	reconcileInterval time.Duration,
) (llm.ExternalGrantConfig, func(), error) {
	ext := provided
	configuredMode := llm.ExternalGrantMode(strings.TrimSpace(settings.Mode))
	configuredRouteMode := llm.ExternalGrantRouteMode(strings.TrimSpace(settings.RouteMode))
	if configuredMode == "disabled" {
		configuredMode = llm.ExternalGrantDisabled
	}
	if configuredMode != "" && configuredMode != llm.ExternalGrantDisabled && configuredMode != llm.ExternalGrantOptional && configuredMode != llm.ExternalGrantRequired {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: unsupported mode %q", settings.Mode)
	}
	if configuredRouteMode != "" && configuredRouteMode != llm.ExternalGrantRouteRuntimeDefault && configuredRouteMode != llm.ExternalGrantRouteCoordinatorBound {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: unsupported route mode %q", settings.RouteMode)
	}
	if ext.Mode == "" {
		ext.Mode = configuredMode
	} else if configuredMode != "" && configuredMode != llm.ExternalGrantDisabled && ext.Mode != configuredMode {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: injected mode %q conflicts with configured mode %q", ext.Mode, configuredMode)
	}
	if ext.RouteMode == "" {
		ext.RouteMode = configuredRouteMode
	} else if configuredRouteMode != "" && ext.RouteMode != configuredRouteMode {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: injected route mode %q conflicts with configured route mode %q", ext.RouteMode, configuredRouteMode)
	}
	if ext.Mode == "" || ext.Mode == llm.ExternalGrantDisabled {
		return ext, func() {}, nil
	}

	if ext.Verifier == nil {
		keys, err := decodeGrantKeys(settings.PublicKeys)
		if err != nil {
			return llm.ExternalGrantConfig{}, func() {}, err
		}
		verifier, err := grant.NewVerifier(grant.VerifierConfig{
			Audience:                settings.Audience,
			RuntimeID:               settings.RuntimeID,
			AuthorizedOrganizations: settings.AuthorizedOrganizations,
			Keys:                    keys,
			RouteMode:               ext.RouteMode,
		})
		if err != nil {
			return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant verifier: %w", err)
		}
		ext.Verifier = verifier
	}

	// Every enabled runtime gets a durable reservation manager by default.
	// Hosts may replace it with a coordinator-backed implementation, but the
	// default is still a real StateStore implementation rather than a process
	// local counter.
	var reservationStore *leases.Store
	if ext.Reservations == nil {
		var err error
		reservationStore, err = leases.New(store, nil)
		if err != nil {
			return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant reservations: %w", err)
		}
		ext.Reservations = reservationStore
		if pending == nil {
			pending = reservationStore
		}
	}
	// An empty route restriction accepts either explicit signed shape. Do not
	// collapse it to the legacy coordinator-bound grant shape at boot: a
	// runtime-default grant intentionally needs no coordinator credential
	// resolver. If an unrestricted runtime later receives a coordinator-bound
	// grant without a resolver, the per-call wrapper still rejects it before
	// the provider is invoked.
	if ext.Mode == llm.ExternalGrantRequired && ext.RouteMode == llm.ExternalGrantRouteCoordinatorBound && ext.Credentials == nil {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: required mode needs an injected credential resolver")
	}
	if ext.Mode == llm.ExternalGrantRequired && ext.ReceiptSink == nil && delivery == nil {
		return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: required mode needs an injected receipt delivery or receipt sink")
	}

	var outbox *llmreceipts.Outbox
	var runCancel context.CancelFunc
	var runDone chan struct{}
	if ext.ReceiptSink == nil && delivery != nil {
		if maxBatch <= 0 {
			maxBatch = 64
		}
		outboxCfg := llmreceipts.Config{
			Store:             store,
			Delivery:          delivery,
			PendingSource:     pending,
			MaxBatch:          maxBatch,
			ReconcileInterval: reconcileInterval,
		}
		var err error
		outbox, err = llmreceipts.New(outboxCfg)
		if err != nil {
			return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant receipt outbox: %w", err)
		}
		ext.ReceiptSink = outbox
		runCtx, cancel := context.WithCancel(context.Background())
		runCancel = cancel
		runDone = make(chan struct{})
		go func() {
			defer close(runDone)
			if err := outbox.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Default().Error("external grant receipt outbox stopped", slog.String("error", err.Error()))
			}
		}()
	}

	if ext.Mode == llm.ExternalGrantRequired {
		if ext.ReceiptSink == nil {
			return llm.ExternalGrantConfig{}, func() {}, fmt.Errorf("external grant: required mode needs an injected receipt delivery or receipt sink")
		}
		ext.ReceiptRequired = true
	}

	closeFn := func() {
		if outbox != nil {
			_ = outbox.Close()
		}
		if runCancel != nil {
			runCancel()
		}
		if runDone != nil {
			<-runDone
		}
	}
	_ = ctx // kept in the signature so future host-owned outboxes can use boot cancellation
	return ext, closeFn, nil
}

func decodeGrantKeys(encoded map[string]string) (map[string]ed25519.PublicKey, error) {
	if len(encoded) == 0 {
		return nil, fmt.Errorf("external grant verifier: public_keys must contain at least one key")
	}
	keys := make(map[string]ed25519.PublicKey, len(encoded))
	for id, value := range encoded {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("external grant verifier: empty key id")
		}
		var decoded []byte
		var err error
		for _, encoding := range []*base64.Encoding{
			base64.RawURLEncoding,
			base64.URLEncoding,
			base64.RawStdEncoding,
			base64.StdEncoding,
		} {
			decoded, err = encoding.DecodeString(value)
			if err == nil {
				break
			}
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("external grant verifier: public key %q is not a %d-byte Ed25519 key", id, ed25519.PublicKeySize)
		}
		keys[id] = append(ed25519.PublicKey(nil), decoded...)
	}
	return keys, nil
}
