package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// callSession replays a locally rejected request once. An HTTP session 404
// recovers the connection, but only read-only operations may replay: a tool
// call may have committed before the server forgot the session receipt.
func callSession[T any](ctx context.Context, p *Provider, replayMissing bool, call func(*mcpsdk.ClientSession) (T, error)) (T, error) {
	var zero T
	p.mu.RLock()
	session, generation, closed := p.session, p.reconnectGeneration, p.closed
	p.mu.RUnlock()
	if closed || session == nil {
		return zero, ErrNotConnected
	}
	result, err := call(session)
	closedConnection := errors.Is(err, mcpsdk.ErrConnectionClosed)
	missingSession := errors.Is(err, mcpsdk.ErrSessionMissing)
	if err == nil || (!closedConnection && !missingSession) {
		return result, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	replacement, reconnectErr := p.reconnectSession(ctx, session, generation)
	if reconnectErr != nil {
		if missingSession && !replayMissing {
			return zero, err
		}
		return zero, fmt.Errorf("mcp: reconnect session: %w", reconnectErr)
	}
	if missingSession && !replayMissing {
		return zero, err
	}
	return call(replacement)
}

const sessionConnectTimeout = 15 * time.Second

// preparationBase keeps only durable attach authority needed to authenticate
// another initialize. It deliberately drops request-local credentials, App
// operation selectors, deadlines, and cancellation from the initiating call.
// The provider computes it once, at the first connect; every reconnect reuses
// that binding and never seats the identity of the request that observed the
// failure. A verified identity is carried, never minted.
func preparationBase(ctx context.Context) context.Context {
	base := context.Background()
	if _, ok := identity.FromVerified(ctx); ok {
		base = identity.CarryVerified(base, ctx)
	} else if id, ok := identity.From(ctx); ok {
		base, _ = identity.With(base, id)
	}
	if agent, ok := tools.InvokingAgentFrom(ctx); ok {
		base = tools.WithInvokingAgent(base, agent)
	}
	if agent, ok := tools.EffectiveAgentConfigFrom(ctx); ok {
		base = tools.WithEffectiveAgentConfig(base, agent)
	}
	if auth.IsSignedCapabilityPreparation(ctx) {
		base = auth.WithSignedCapabilityPreparation(base)
	}
	return base
}

func newSessionFlight(base context.Context, previousCancel context.CancelFunc) (*sessionFlight, context.Context) {
	connectCtx, cancel := context.WithCancel(base)
	flight := &sessionFlight{done: make(chan struct{}), cancel: cancel, previousCancel: previousCancel}
	return flight, connectCtx
}

// sessionFlight shares one initialize attempt without keeping the state mutex
// across credentials or network I/O. Results are immutable once done closes.
type sessionFlight struct {
	done           chan struct{}
	cancel         context.CancelFunc
	previousCancel context.CancelFunc
	session        *mcpsdk.ClientSession
	err            error
}

type subscriptionKey struct {
	uri      string
	identity identity.Identity
}

// restoreSubscriptions runs with subscriptionMu held through publication, so
// a subscription completed during replacement is either restored or detects
// that it landed on the old session and retries on the new one.
func (p *Provider) restoreSubscriptions(ctx context.Context, session *mcpsdk.ClientSession) error {
	for key, base := range p.subscriptions {
		attemptBase, cancel := context.WithCancel(base)
		stop := context.AfterFunc(ctx, cancel)
		meta, err := p.buildIdentityMeta(attemptBase)
		if err != nil {
			stop()
			cancel()
			return fmt.Errorf("mcp: restore resource subscription: %w", err)
		}
		attempt, err := p.resolveBearerCtx(attemptBase, key.uri)
		if err != nil {
			stop()
			cancel()
			return fmt.Errorf("mcp: restore resource subscription: %w", err)
		}
		params := &mcpsdk.SubscribeParams{URI: key.uri}
		params.Meta = meta
		err = session.Subscribe(attempt, params)
		stop()
		cancel()
		if err != nil {
			return fmt.Errorf("mcp: restore resource subscription: %w", safeDownstreamCause(err))
		}
	}
	return nil
}

func awaitSession(ctx context.Context, flight *sessionFlight) (*mcpsdk.ClientSession, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.session, flight.err
	}
}

// reconnectSession shares successful and failed replacement receipts with the
// callers that observed the same session epoch. Each waiter retains its own
// cancellation; a later call may retry a failed epoch.
func (p *Provider) reconnectSession(ctx context.Context, failed *mcpsdk.ClientSession, generation uint64) (*mcpsdk.ClientSession, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrNotConnected
	}
	if p.session != failed || p.reconnectGeneration != generation {
		session, err := p.session, p.reconnectErr
		p.mu.Unlock()
		return session, err
	}
	if flight := p.connectionFlight; flight != nil {
		p.mu.Unlock()
		return awaitSession(ctx, flight)
	}
	flight, connectCtx := newSessionFlight(p.sessionBase, p.sessionCancel)
	p.connectionFlight = flight
	p.mu.Unlock()
	go p.completeConnection(connectCtx, flight, failed)
	return awaitSession(ctx, flight)
}

func (p *Provider) completeConnection(ctx context.Context, flight *sessionFlight, failed *mcpsdk.ClientSession) {
	// Bound the initialize attempt without putting a deadline on the live
	// session's transport context after initialization succeeds.
	timer := time.AfterFunc(sessionConnectTimeout, flight.cancel)
	defer timer.Stop()
	if failed != nil {
		// Close joins the old SDK transport even when it reports the terminal
		// failure that caused recovery. No provider state mutex is held here.
		if err := failed.Close(); err != nil {
			p.logger.Debug("mcp: closing failed session", "source", p.source)
		}
	}
	if flight.previousCancel != nil {
		flight.previousCancel()
	}
	session, mode, err := p.establishSession(ctx)
	p.subscriptionMu.Lock()
	if err == nil && failed != nil {
		if restoreErr := p.restoreSubscriptions(ctx, session); restoreErr != nil {
			_ = session.Close()
			session, err = nil, restoreErr
		}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		// Teardown may win while initialize succeeds. Never publish that late
		// handle, and finish its cleanup before releasing flight waiters.
		if session != nil {
			if closeErr := session.Close(); closeErr != nil {
				p.logger.Debug("mcp: closing unpublished session", "source", p.source)
			}
		}
		p.mu.Lock()
		session, err = nil, ErrNotConnected
	} else if err == nil {
		p.session, p.selectedMode, p.sessionCancel = session, mode, flight.cancel
	}
	if failed != nil {
		p.reconnectGeneration++
		p.reconnectErr = err
	}
	if err != nil {
		flight.cancel()
	}
	flight.session, flight.err = session, err
	p.connectionFlight = nil
	close(flight.done)
	p.mu.Unlock()
	p.subscriptionMu.Unlock()
}
