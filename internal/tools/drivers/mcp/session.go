package mcp

import (
	"context"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// callSession retries once only when the SDK proves this request did not run:
// ErrConnectionClosed rejects dispatch locally; ErrSessionMissing is the MCP
// server's HTTP 404 rejection of an expired session. An arbitrary failed write
// or malformed response may follow a remote effect and must not be replayed.
func callSession[T any](ctx context.Context, p *Provider, call func(*mcpsdk.ClientSession) (T, error)) (T, error) {
	var zero T
	p.mu.RLock()
	session, generation, closed := p.session, p.reconnectGeneration, p.closed
	p.mu.RUnlock()
	if closed || session == nil {
		return zero, ErrNotConnected
	}
	result, err := call(session)
	if err == nil || (!errors.Is(err, mcpsdk.ErrConnectionClosed) && !errors.Is(err, mcpsdk.ErrSessionMissing)) {
		return result, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	replacement, reconnectErr := p.reconnectSession(ctx, session, generation)
	if reconnectErr != nil {
		return zero, fmt.Errorf("mcp: reconnect session: %w", reconnectErr)
	}
	return call(replacement)
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
	connectCtx, cancel := context.WithCancel(ctx)
	flight := &sessionFlight{done: make(chan struct{}), cancel: cancel, previousCancel: p.sessionCancel}
	p.connectionFlight = flight
	p.mu.Unlock()
	return p.completeConnection(connectCtx, flight, failed)
}

func (p *Provider) completeConnection(ctx context.Context, flight *sessionFlight, failed *mcpsdk.ClientSession) (*mcpsdk.ClientSession, error) {
	if failed != nil {
		// Close joins the old SDK transport even when it reports the terminal
		// failure that caused recovery. No provider state mutex is held here.
		if err := failed.Close(); err != nil {
			p.logger.Debug("mcp: closing failed session", "source", p.source, "error", err)
		}
	}
	if flight.previousCancel != nil {
		flight.previousCancel()
	}
	session, mode, err := p.establishSession(ctx)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		// Teardown may win while initialize succeeds. Never publish that late
		// handle, and finish its cleanup before releasing flight waiters.
		if session != nil {
			if closeErr := session.Close(); closeErr != nil {
				p.logger.Debug("mcp: closing unpublished session", "source", p.source, "error", closeErr)
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
	return session, err
}
