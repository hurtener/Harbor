package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
)

type candidateTokenSource struct {
	scope     types.IdentityScope
	token     string
	base      *LifetimeTokenSource
	committed atomic.Bool
}

func (s *candidateTokenSource) Token(ctx context.Context, scope types.IdentityScope) (string, error) {
	if s.committed.Load() {
		return s.base.Token(ctx, scope)
	}
	if scopeKey(scope) != scopeKey(s.scope) {
		return "", ErrTokenUnavailable
	}
	return validateTokenForScope(s.token, scope, time.Now())
}

// ConnectionState is an honest transport posture.
type ConnectionState string

const (
	StateConnecting   ConnectionState = "connecting"
	StateLive         ConnectionState = "live"
	StateDisconnected ConnectionState = "disconnected"
	StateReconnecting ConnectionState = "reconnecting"
	StateReplaying    ConnectionState = "replaying"
	StateAuthExpired  ConnectionState = "authentication expired"
	StateClosed       ConnectionState = "closed"
	StateErased       ConnectionState = "erased"
)

// Update is an immutable controller notification.
type Update struct {
	Identity   types.IdentityScope
	Generation uint64
	State      ConnectionState
	Projection projection.Projection
	Err        error
	Attempt    int
	Batchable  bool
	Overflow   bool
}

// Controller owns exactly one active identity-scoped Protocol stream.
type Controller struct {
	mu           sync.Mutex
	switchMu     sync.Mutex
	baseURL      string
	tokens       *LifetimeTokenSource
	client       protocolclient.Client
	stream       *protocolclient.EventStream
	cancel       context.CancelFunc
	done         chan struct{}
	identity     types.IdentityScope
	projection   projection.Projection
	generation   uint64
	capabilities map[types.Capability]bool
	onUpdate     func(Update)
	closed       bool
}

// NewController constructs a reusable attach controller.
func NewController(baseURL string, tokens *LifetimeTokenSource, identity types.IdentityScope, onUpdate func(Update)) (*Controller, error) {
	if tokens == nil {
		return nil, errors.New("tui: token source required")
	}
	if err := validateScope(identity); err != nil {
		return nil, err
	}
	if onUpdate == nil {
		onUpdate = func(Update) {}
	}
	return &Controller{baseURL: baseURL, tokens: tokens, identity: identity, onUpdate: onUpdate, capabilities: map[types.Capability]bool{}}, nil
}

// Attach negotiates and hydrates the initial session.
func (c *Controller) Attach(ctx context.Context) error { return c.switchTo(ctx, c.identity) }

// Switch prepares the target stream and fenced hydration join, then drains the
// old stream before atomically committing the target generation.
func (c *Controller) Switch(ctx context.Context, target types.IdentityScope) error {
	if err := validateScope(target); err != nil {
		return err
	}
	return c.switchTo(ctx, target)
}

func (c *Controller) switchTo(ctx context.Context, target types.IdentityScope) error {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("tui: controller closed")
	}
	oldCancel, oldStream, oldDone := c.cancel, c.stream, c.done
	oldIdentity, oldProjection, oldGeneration := c.identity, c.projection, c.generation
	c.mu.Unlock()
	c.publish(Update{Identity: oldIdentity, Generation: oldGeneration, State: StateConnecting, Projection: oldProjection})
	client, info, streamCtx, cancel, stream, p, generation, err := c.prepareTarget(ctx, target, c.tokens, oldGeneration+1)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		if cancel != nil {
			cancel()
		}
		if oldCancel != nil {
			oldCancel()
		}
		if oldStream != nil {
			_ = oldStream.Close()
		}
		if oldDone != nil {
			<-oldDone
		}
		c.mu.Lock()
		c.cancel, c.stream, c.done = nil, nil, nil
		c.mu.Unlock()
		return c.failFor(oldIdentity, oldGeneration, oldProjection, err)
	}

	// Commit the candidate only after its authenticated stream and complete
	// hydration join are available. The old stream is then drained and joined.
	if oldCancel != nil {
		oldCancel()
	}
	if oldStream != nil {
		_ = oldStream.Close()
	}
	if oldDone != nil {
		<-oldDone
	}
	caps := make(map[types.Capability]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		caps[capability] = true
	}
	if p.SessionErased {
		cancel()
		_ = stream.Close()
		c.mu.Lock()
		c.client, c.identity, c.projection, c.capabilities, c.generation = client, target, p, caps, generation
		c.cancel, c.stream, c.done = nil, nil, nil
		c.mu.Unlock()
		c.publish(Update{Identity: target, Generation: generation, State: StateErased, Projection: p})
		return nil
	}
	done := make(chan struct{})
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		_ = stream.Close()
		return errors.New("tui: controller closed")
	}
	c.client, c.stream, c.cancel, c.done, c.identity, c.projection, c.capabilities, c.generation = client, stream, cancel, done, target, p, caps, generation
	c.mu.Unlock()
	c.publish(Update{Identity: target, Generation: generation, State: StateLive, Projection: p})
	go c.readLoop(streamCtx, generation, target, client, stream, done)
	return nil
}

func (c *Controller) prepareTarget(ctx context.Context, target types.IdentityScope, source protocolclient.TokenSource, generation uint64) (protocolclient.Client, types.RuntimeInfo, context.Context, context.CancelFunc, *protocolclient.EventStream, projection.Projection, uint64, error) {
	if _, err := source.Token(ctx, target); err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	client, err := protocolclient.New(protocolclient.Connection{BaseURL: c.baseURL, Token: source, Identity: target})
	if err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	info, err := client.RuntimeInfo(ctx)
	if err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	if err = requireConversationCapabilities(info.Capabilities); err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := client.Subscribe(streamCtx, protocolclient.StreamOptions{})
	if err != nil {
		cancel()
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	bundle, err := projection.HydrateClient(ctx, client, generation, 0, 8)
	if err != nil {
		var protocolErr *protocolclient.ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != protoerrors.CodeNotFound {
			cancel()
			_ = stream.Close()
			return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
		}
		bundle = projection.SnapshotBundle{Generation: generation, Identity: target}
	}
	p, err := (&projection.Reducer{}).Hydrate(bundle)
	if err != nil {
		cancel()
		_ = stream.Close()
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	return client, info, streamCtx, cancel, stream, p, generation, nil
}

func (c *Controller) readLoop(ctx context.Context, generation uint64, identity types.IdentityScope, client protocolclient.Client, stream *protocolclient.EventStream, done chan struct{}) {
	defer close(done)
	defer func() { _ = stream.Close() }()
	attempt := 0
	for {
		frame, err := stream.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempt++
			c.publish(Update{Identity: identity, Generation: generation, State: StateReconnecting, Attempt: attempt, Err: err})
			delay := time.Duration(min(attempt, 5)) * 200 * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			c.mu.Lock()
			cursor := c.projection.Cursor
			c.mu.Unlock()
			nextStream, subscribeErr := client.Subscribe(ctx, protocolclient.StreamOptions{LastEventID: cursor})
			if subscribeErr != nil {
				if c.fail(subscribeErr) != nil {
					return
				}
				return
			}
			_ = stream.Close()
			stream = nextStream
			c.mu.Lock()
			if generation != c.generation {
				c.mu.Unlock()
				return
			}
			c.stream = stream
			c.mu.Unlock()
			c.publish(Update{Identity: identity, Generation: generation, State: StateReplaying, Attempt: attempt})
			c.mu.Lock()
			c.generation++
			reconcileGeneration := c.generation
			c.mu.Unlock()
			bundle, hydrateErr := projection.HydrateClient(ctx, client, reconcileGeneration, 0, 8)
			if hydrateErr != nil {
				var protocolErr *protocolclient.ProtocolError
				if !errors.As(hydrateErr, &protocolErr) || protocolErr.Code != protoerrors.CodeNotFound {
					if c.fail(hydrateErr) != nil {
						return
					}
					return
				}
				bundle = projection.SnapshotBundle{Generation: reconcileGeneration, Identity: identity}
			}
			c.mu.Lock()
			reconciled, _, reconcileErr := projection.Reconcile(c.projection, bundle)
			if reconcileErr == nil {
				c.projection = reconciled
			}
			c.mu.Unlock()
			if reconcileErr != nil {
				if c.fail(reconcileErr) != nil {
					return
				}
				return
			}
			generation = reconcileGeneration
			c.publish(Update{Identity: identity, Generation: reconcileGeneration, State: StateLive, Projection: reconciled, Attempt: attempt})
			continue
		}
		if len(frame.Data) == 0 {
			continue
		}
		var event types.StateEvent
		if err = json.Unmarshal(frame.Data, &event); err != nil {
			c.publish(Update{Identity: identity, Generation: generation, State: StateDisconnected, Err: fmt.Errorf("tui: decode event: %w", err)})
			continue
		}
		c.mu.Lock()
		if generation != c.generation || scopeKey(identity) != scopeKey(c.identity) {
			c.mu.Unlock()
			continue
		}
		next, change, applyErr := (&projection.Reducer{}).Apply(c.projection, event)
		if applyErr == nil {
			c.projection = next
		}
		c.mu.Unlock()
		if applyErr != nil {
			c.publish(Update{Identity: identity, Generation: generation, State: StateDisconnected, Err: applyErr})
			continue
		}
		c.publish(Update{Identity: identity, Generation: generation, State: StateLive, Projection: next, Batchable: change.Batchable && !change.Immediate})
	}
}

// Start dispatches canonical start. Closed sessions reopen through this same method;
// erased sessions fail and remain terminal.
func (c *Controller) Start(ctx context.Context, text string, artifactIDs []string, dispositions map[string]string) (types.StartResponse, error) {
	c.mu.Lock()
	client, p := c.client, c.projection
	c.mu.Unlock()
	if client == nil {
		return types.StartResponse{}, errors.New("tui: not attached")
	}
	if p.SessionErased {
		return types.StartResponse{}, &protocolclient.ProtocolError{Status: 409, Code: protoerrors.CodeSessionErased, Message: "session erased; Start Fresh required"}
	}
	response, err := client.Start(ctx, types.StartRequest{Query: text, InputArtifactIDs: append([]string(nil), artifactIDs...), InputArtifactDispositions: cloneStrings(dispositions)})
	if err != nil {
		next, _ := projection.ApplyProtocolError(p, err)
		c.mu.Lock()
		c.projection = next
		c.mu.Unlock()
		if next.SessionErased {
			c.publish(Update{State: StateErased, Projection: next, Err: err})
		}
		return response, err
	}
	return response, nil
}

// Rename updates canonical session metadata.
func (c *Controller) Rename(ctx context.Context, title string) (types.SessionsSetTitleResponse, error) {
	c.mu.Lock()
	client, id := c.client, c.identity
	c.mu.Unlock()
	if client == nil {
		return types.SessionsSetTitleResponse{}, errors.New("tui: not attached")
	}
	return client.SessionsSetTitle(ctx, types.SessionsSetTitleRequest{SessionID: id.Session, Title: title})
}

// Sessions lists authorized session metadata for the picker. Query is applied
// by the canonical sessions surface; an empty query returns the browse page.
func (c *Controller) Sessions(ctx context.Context, query, cursor string) (types.SessionsListResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.SessionsListResponse{}, errors.New("tui: not attached")
	}
	return client.SessionsList(ctx, types.SessionsListRequest{Filter: types.SessionFilter{Query: query}, Cursor: cursor, Limit: types.DefaultSessionListLimit})
}

// Upload stores one user attachment through canonical artifacts.put.
func (c *Controller) Upload(ctx context.Context, name, mime string, body []byte) (types.ArtifactsPutResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.ArtifactsPutResponse{}, errors.New("tui: not attached")
	}
	return client.ArtifactsPut(ctx, types.ArtifactsPutRequest{Bytes: append([]byte(nil), body...), Opts: types.ArtifactsPutOpts{Filename: name, MimeType: mime, Source: types.ArtifactSourceUserUpload}})
}

// ReplaceToken installs an in-memory credential and reattaches the active
// session through the same drain, hydrate, and stream-acquisition transaction.
func (c *Controller) ReplaceToken(ctx context.Context, token string) error {
	id := c.Identity()
	if strings.TrimSpace(token) == "clear" {
		prior, had := c.tokens.Replacement(id)
		c.tokens.Clear(id)
		if err := c.switchTo(ctx, id); err != nil {
			var rollbackErr error
			if had {
				rollbackErr = c.tokens.Replace(id, prior)
			}
			if rollbackErr == nil {
				rollbackErr = c.switchTo(ctx, id)
			}
			if rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("credential rollback: %w", rollbackErr))
			}
			return err
		}
		return nil
	}
	if _, err := validateTokenForScope(token, id, time.Now()); err != nil {
		return c.fail(err)
	}
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("tui: controller closed")
	}
	probeGeneration := c.generation + 1
	oldCancel, oldStream, oldDone := c.cancel, c.stream, c.done
	c.mu.Unlock()
	probeSource := &candidateTokenSource{scope: id, token: strings.TrimSpace(token), base: c.tokens}
	client, info, streamCtx, cancel, stream, p, generation, err := c.prepareTarget(ctx, id, probeSource, probeGeneration)
	if err != nil {
		return c.fail(err)
	}
	if err = c.tokens.Replace(id, token); err != nil {
		cancel()
		_ = stream.Close()
		return c.fail(err)
	}
	probeSource.committed.Store(true)
	if oldCancel != nil {
		oldCancel()
	}
	if oldStream != nil {
		_ = oldStream.Close()
	}
	if oldDone != nil {
		<-oldDone
	}
	caps := make(map[types.Capability]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		caps[capability] = true
	}
	if p.SessionErased {
		cancel()
		_ = stream.Close()
		c.mu.Lock()
		c.client, c.identity, c.projection, c.capabilities, c.generation = client, id, p, caps, generation
		c.cancel, c.stream, c.done = nil, nil, nil
		c.mu.Unlock()
		c.publish(Update{Identity: id, Generation: generation, State: StateErased, Projection: p})
		return nil
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.client, c.stream, c.cancel, c.done, c.identity, c.projection, c.capabilities, c.generation = client, stream, cancel, done, id, p, caps, generation
	c.mu.Unlock()
	c.publish(Update{Identity: id, Generation: generation, State: StateLive, Projection: p})
	go c.readLoop(streamCtx, generation, id, client, stream, done)
	return nil
}

// Delete erases the active canonical session and marks it terminal locally.
func (c *Controller) Delete(ctx context.Context) (types.SessionsDeleteResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.SessionsDeleteResponse{}, errors.New("tui: not attached")
	}
	response, err := client.SessionsDelete(ctx)
	if err == nil {
		c.mu.Lock()
		c.projection.SessionErased = true
		c.projection.SessionStatus = "erased"
		p := c.projection
		c.mu.Unlock()
		c.publish(Update{State: StateErased, Projection: p})
	}
	return response, err
}

// Identity returns the active full triple.
func (c *Controller) Identity() types.IdentityScope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identity
}

// Projection returns an isolated normalized copy.
func (c *Controller) Projection() projection.Projection {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, err := projection.Normalize(c.projection)
	if err != nil {
		return projection.Projection{}
	}
	var out projection.Projection
	if err := json.Unmarshal(body, &out); err != nil {
		return projection.Projection{}
	}
	return out
}

// HasCapability reports the negotiated Runtime capability for command gating.
func (c *Controller) HasCapability(capability types.Capability) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities[capability]
}

// Close cancels and joins the active stream.
func (c *Controller) Close() error {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancel, stream, done := c.cancel, c.stream, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var err error
	if stream != nil {
		err = stream.Close()
	}
	if done != nil {
		<-done
	}
	return err
}

func (c *Controller) fail(err error) error {
	state := StateDisconnected
	var pErr *protocolclient.ProtocolError
	if errors.As(err, &pErr) && pErr.Status == 401 {
		state = StateAuthExpired
	}
	if errors.Is(err, ErrTokenExpired) {
		state = StateAuthExpired
	}
	c.publish(Update{State: state, Err: err})
	return err
}
func (c *Controller) failFor(id types.IdentityScope, generation uint64, p projection.Projection, err error) error {
	state := StateDisconnected
	var protocolErr *protocolclient.ProtocolError
	if (errors.As(err, &protocolErr) && protocolErr.Status == 401) || errors.Is(err, ErrTokenExpired) {
		state = StateAuthExpired
	}
	c.publish(Update{Identity: id, Generation: generation, State: state, Projection: p, Err: err})
	return err
}
func (c *Controller) publish(update Update) {
	c.mu.Lock()
	callback := c.onUpdate
	if update.Identity.Session == "" {
		update.Identity = c.identity
	}
	if update.Generation == 0 {
		update.Generation = c.generation
	}
	if update.Projection.Identity.Session == "" {
		update.Projection = c.projection
	}
	c.mu.Unlock()
	callback(update)
}
func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func requireConversationCapabilities(capabilities []types.Capability) error {
	have := make(map[types.Capability]bool, len(capabilities))
	for _, capability := range capabilities {
		have[capability] = true
	}
	for _, required := range []types.Capability{types.CapEventsSubscribe, types.CapStateSnapshots} {
		if !have[required] {
			return fmt.Errorf("tui: Runtime does not advertise required %s capability", required)
		}
	}
	return nil
}
