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
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	tuiartifacts "github.com/hurtener/Harbor/internal/tui/artifacts"
	tuievents "github.com/hurtener/Harbor/internal/tui/events"
	"github.com/hurtener/Harbor/internal/tui/interventions"
	"github.com/hurtener/Harbor/internal/tui/posture"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/surface"
	tuitasks "github.com/hurtener/Harbor/internal/tui/tasks"
	tuitools "github.com/hurtener/Harbor/internal/tui/tools"
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
	mu            sync.Mutex
	switchMu      sync.Mutex
	baseURL       string
	tokens        *LifetimeTokenSource
	client        protocolclient.RuntimeClient
	stream        *protocolclient.EventStream
	cancel        context.CancelFunc
	done          chan struct{}
	identity      types.IdentityScope
	projection    projection.Projection
	generation    uint64
	inspectEpoch  uint64
	capabilities  map[types.Capability]bool
	interventions interventions.Inbox
	onUpdate      func(Update)
	closed        bool
}

// RuntimeData is the bounded canonical inspection state for the active session.
// Errors is keyed by surface so partial availability never becomes a zero value.
type RuntimeData struct {
	Identity      types.IdentityScope
	Generation    uint64
	RequestEpoch  uint64
	Stale         bool
	Tasks         tuitasks.State
	Tools         tuitools.State
	Artifacts     tuiartifacts.State
	Events        tuievents.State
	Posture       posture.State
	Interventions interventions.Inbox
	Errors        map[string]string
}

// InspectionRequest fences one bounded screen refresh and carries exact
// selections. The controller never silently substitutes index zero.
type InspectionRequest struct {
	Identity                    types.IdentityScope
	Generation, RequestEpoch    uint64
	TaskID, ToolID, ArtifactID  string
	TaskFilter, ToolFilter      string
	ArtifactFilter, EventFilter string
	TaskPage, ToolPage          int
	ArtifactPage, EventPage     int
	TaskCursor                  types.TaskListCursor
}

// Mutation is one exact Protocol mutation request. App-level ActionIntent is
// converted to this value only after stale-context validation.
type Mutation struct {
	Identity           types.IdentityScope
	Method             methods.Method
	RunID, PauseToken  string
	ToolID, ArtifactID string
	Payload            map[string]any
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
	return &Controller{baseURL: baseURL, tokens: tokens, identity: identity, onUpdate: onUpdate, capabilities: map[types.Capability]bool{}, interventions: interventions.New()}, nil
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
		// The current session was never touched — the candidate failed
		// BEFORE any teardown, so a failed switch must not kill the live
		// stream. Republish the previous posture (with the error for the
		// surface toast) and stay exactly where we were; the same commit
		// ordering that guards the success path guards the failure path.
		if oldStream != nil {
			c.publish(Update{Identity: oldIdentity, Generation: oldGeneration, State: StateLive, Projection: oldProjection, Err: err})
			return err
		}
		if oldProjection.SessionErased {
			c.publish(Update{Identity: oldIdentity, Generation: oldGeneration, State: StateErased, Projection: oldProjection, Err: err})
			return err
		}
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
	if scopeKey(oldIdentity) != scopeKey(target) {
		c.interventions = interventions.New()
	}
	c.mu.Unlock()
	c.publish(Update{Identity: target, Generation: generation, State: StateLive, Projection: p})
	go c.readLoop(streamCtx, generation, target, client, stream, done)
	return nil
}

func (c *Controller) prepareTarget(ctx context.Context, target types.IdentityScope, source protocolclient.TokenSource, generation uint64) (protocolclient.RuntimeClient, types.RuntimeInfo, context.Context, context.CancelFunc, *protocolclient.EventStream, projection.Projection, uint64, error) {
	if _, err := source.Token(ctx, target); err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	baseClient, err := protocolclient.New(protocolclient.Connection{BaseURL: c.baseURL, Token: source, Identity: target})
	if err != nil {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, err
	}
	client, ok := baseClient.(protocolclient.RuntimeClient)
	if !ok {
		return nil, types.RuntimeInfo{}, nil, nil, nil, projection.Projection{}, generation, errors.New("tui: Protocol client lacks Runtime inspection surface")
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

// Inspect loads every Runtime-control screen through the authenticated client.
// Individual unavailable surfaces are retained as explicit errors.
func (c *Controller) Inspect(ctx context.Context, request InspectionRequest) RuntimeData {
	c.mu.Lock()
	client, capabilities, generation, identity, inbox := c.client, mapsClone(c.capabilities), c.generation, c.identity, c.interventions
	if request.RequestEpoch < c.inspectEpoch {
		c.mu.Unlock()
		return RuntimeData{Identity: identity, Generation: generation, RequestEpoch: request.RequestEpoch, Stale: true, Errors: map[string]string{}}
	}
	c.inspectEpoch = request.RequestEpoch
	c.mu.Unlock()
	data := RuntimeData{Identity: identity, Generation: generation, RequestEpoch: request.RequestEpoch, Errors: map[string]string{}, Interventions: inbox}
	data.Tasks.Surface = surface.State{Status: surface.Loading}
	data.Tools.Surface = surface.State{Status: surface.Loading}
	data.Artifacts.Surface = surface.State{Status: surface.Loading}
	data.Events.Surface = surface.State{Status: surface.Loading}
	data.Posture.Surfaces = map[string]surface.State{}
	if request.Generation != 0 && (request.Generation != generation || scopeKey(request.Identity) != scopeKey(identity)) {
		data.Stale = true
		return data
	}
	if client == nil {
		data.Errors["runtime"] = "not attached"
		data.Tasks.Surface = surface.State{Status: surface.Unavailable, Reason: "not attached"}
		data.Tools.Surface = data.Tasks.Surface
		data.Artifacts.Surface = data.Tasks.Surface
		data.Events.Surface = data.Tasks.Surface
		return data
	}
	info, err := client.RuntimeInfo(ctx)
	if err != nil {
		data.Errors[string(methods.MethodRuntimeInfo)] = err.Error()
		data.Posture.Surfaces[string(methods.MethodRuntimeInfo)] = errorSurface(err)
	} else {
		data.Posture.Info = info
		data.Posture.Surfaces[string(methods.MethodRuntimeInfo)] = surface.State{Status: surface.Loaded}
	}
	health, err := client.RuntimeHealth(ctx)
	if err != nil {
		data.Errors[string(methods.MethodRuntimeHealth)] = err.Error()
	} else {
		data.Posture.Health = health
	}
	data.Posture.Surfaces[string(methods.MethodRuntimeHealth)] = stateFor(err)
	counters, err := client.RuntimeCounters(ctx)
	if err != nil {
		data.Errors[string(methods.MethodRuntimeCounters)] = err.Error()
	} else {
		data.Posture.Counters = counters
	}
	data.Posture.Surfaces[string(methods.MethodRuntimeCounters)] = stateFor(err)
	drivers, err := client.RuntimeDrivers(ctx)
	if err != nil {
		data.Errors[string(methods.MethodRuntimeDrivers)] = err.Error()
	} else {
		data.Posture.Drivers = drivers
	}
	data.Posture.Surfaces[string(methods.MethodRuntimeDrivers)] = stateFor(err)
	metrics, err := client.MetricsSnapshot(ctx)
	if err != nil {
		data.Errors[string(methods.MethodMetricsSnapshot)] = err.Error()
	} else {
		data.Posture.Metrics = metrics
	}
	data.Posture.Surfaces[string(methods.MethodMetricsSnapshot)] = stateFor(err)
	governance, err := client.GovernancePosture(ctx)
	if err != nil {
		data.Errors[string(methods.MethodGovernancePosture)] = err.Error()
	} else {
		data.Posture.Governance = governance
	}
	data.Posture.Surfaces[string(methods.MethodGovernancePosture)] = stateFor(err)
	llm, err := client.LLMPosture(ctx)
	if err != nil {
		data.Errors[string(methods.MethodLLMPosture)] = err.Error()
	} else {
		data.Posture.LLM = llm
	}
	data.Posture.Surfaces[string(methods.MethodLLMPosture)] = stateFor(err)
	taskPage := max(1, request.TaskPage)
	taskRows, err := client.TasksList(ctx, types.TaskListRequest{PageSize: types.DefaultTaskListPageSize, Cursor: request.TaskCursor, Filter: types.TaskFilter{Search: request.TaskFilter}, IncludeStatusCounterStrip: true})
	if err != nil {
		data.Errors[string(methods.MethodTasksList)] = err.Error()
		data.Tasks.Surface = errorSurface(err)
	} else {
		data.Tasks = tuitasks.Derive(taskRows, nil)
		data.Tasks.Filter, data.Tasks.Page = request.TaskFilter, taskPage
		if request.TaskID != "" {
			detail, detailErr := client.TasksGet(ctx, types.TaskGetRequest{ID: request.TaskID})
			if detailErr != nil {
				data.Errors[string(methods.MethodTasksGet)] = detailErr.Error()
				data.Tasks.Surface = surface.State{Status: surface.Partial, Reason: "selected task detail: " + detailErr.Error()}
			} else {
				data.Tasks = tuitasks.Derive(taskRows, &detail)
				data.Tasks.Filter, data.Tasks.Page = request.TaskFilter, taskPage
			}
		}
	}
	toolPage := max(1, request.ToolPage)
	toolRows, err := client.ToolsList(ctx, types.ToolListRequest{Page: toolPage, PageSize: types.DefaultToolListPageSize, Filter: types.ToolFilter{Search: request.ToolFilter}})
	if err != nil {
		data.Errors[string(methods.MethodToolsList)] = err.Error()
		data.Tools.Surface = errorSurface(err)
	} else {
		data.Tools = tuitools.Derive(toolRows, capabilities[types.CapToolAnnotations])
		data.Tools.Filter, data.Tools.Page = request.ToolFilter, toolPage
		if request.ToolID != "" {
			var selected types.Tool
			for _, tool := range data.Tools.Rows {
				if tool.ID == request.ToolID {
					selected = tool
					break
				}
			}
			detail := tuitools.Detail{Tool: selected, BestEffort: true, Annotations: capabilities[types.CapToolAnnotations]}
			if detail.Tool.ID == "" {
				got, getErr := client.ToolsGet(ctx, types.ToolGetRequest{ID: request.ToolID})
				if getErr != nil {
					data.Errors[string(methods.MethodToolsGet)] = getErr.Error()
					data.Tools.Surface = surface.State{Status: surface.Partial, Reason: getErr.Error()}
				} else {
					detail.Tool = got
				}
			}
			if !detail.Annotations {
				detail.Unavailable = "Runtime does not advertise tool_annotations; policy authority remains server-enforced and unknown"
			} else {
				manifest, manifestErr := client.ToolsDescribe(ctx, types.ToolDescribeRequest{ID: detail.Tool.ID})
				metrics, metricsErr := client.ToolsMetrics(ctx, types.ToolMetricsRequest{ID: detail.Tool.ID, Window: types.ToolWindow1h})
				content, contentErr := client.ToolsContentStats(ctx, types.ToolContentStatsRequest{ID: detail.Tool.ID})
				for surface, detailErr := range map[methods.Method]error{methods.MethodToolsDescribe: manifestErr, methods.MethodToolsMetrics: metricsErr, methods.MethodToolsContentStats: contentErr} {
					if detailErr != nil {
						data.Errors[string(surface)] = detailErr.Error()
					}
				}
				if manifestErr == nil {
					detail.Manifest = &manifest
				}
				if metricsErr == nil {
					detail.Metrics = &metrics
				}
				if contentErr == nil {
					detail.Content = &content
				}
			}
			data.Tools.Selected = &detail
			data.Tools.SelectedID = request.ToolID
		}
	}
	artifactRows, err := client.ArtifactsList(ctx, types.ArtifactsListRequest{Limit: types.DefaultArtifactsLimit})
	if err != nil {
		data.Errors[string(methods.MethodArtifactsList)] = err.Error()
		data.Artifacts.Surface = errorSurface(err)
	} else {
		data.Artifacts = tuiartifacts.Derive(artifactRows)
		data.Artifacts.Filter, data.Artifacts.Page = request.ArtifactFilter, max(1, request.ArtifactPage)
		if request.ArtifactID != "" {
			var row types.ArtifactRow
			for _, candidate := range data.Artifacts.Rows {
				if candidate.Ref.ID == request.ArtifactID {
					row = candidate
					break
				}
			}
			preview := tuiartifacts.Preview{Ref: row.Ref}
			if row.Ref.ID == "" {
				preview.Ref.ID = request.ArtifactID
				preview.Unavailable = "selected artifact is outside this retained page"
			}
			if !tuiartifacts.Previewable(row.Ref) {
				preview.Unavailable = "terminal preview is unavailable for " + row.Ref.MimeType
			} else {
				ref, refErr := client.ArtifactsGetRef(ctx, types.ArtifactsGetRefRequest{ID: row.Ref.ID})
				if refErr != nil {
					preview.Unavailable = refErr.Error()
					data.Errors[string(methods.MethodArtifactsGetRef)] = refErr.Error()
				} else {
					preview.URL = ref.PresignedURL
				}
			}
			data.Artifacts.Selected = &preview
			data.Artifacts.SelectedID = request.ArtifactID
		}
	}
	eventRows, listErr := client.EventsList(ctx, types.EventsListRequest{Limit: types.DefaultEventsListLimit})
	aggregateRows, aggregateErr := client.EventsAggregate(ctx, types.EventAggregateRequest{Window: time.Hour, Bucket: 5 * time.Minute})
	if listErr != nil {
		data.Errors[string(methods.MethodEventsList)] = listErr.Error()
	}
	if aggregateErr != nil {
		data.Errors[string(methods.MethodEventsAggregate)] = aggregateErr.Error()
	}
	data.Events = tuievents.Derive(eventRows, aggregateRows)
	data.Events.Filter, data.Events.Page = request.EventFilter, max(1, request.EventPage)
	if listErr != nil && aggregateErr != nil {
		data.Events.Surface = errorSurface(listErr)
	} else if listErr != nil || aggregateErr != nil {
		data.Events.Surface = surface.State{Status: surface.Partial, Reason: "one event projection failed"}
	}
	pauses, err := client.PauseList(ctx, types.PauseListRequest{Page: 1, PageSize: types.DefaultPauseListPageSize})
	if err != nil {
		data.Errors[string(methods.MethodPauseList)] = err.Error()
		data.Interventions.Surface = errorSurface(err)
	} else {
		data.Interventions = data.Interventions.ReconcilePage(pauses, generation, time.Now())
		c.mu.Lock()
		if generation == c.generation && request.RequestEpoch == c.inspectEpoch && scopeKey(identity) == scopeKey(c.identity) {
			c.interventions = data.Interventions
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	data.Stale = generation != c.generation || request.RequestEpoch != c.inspectEpoch || scopeKey(identity) != scopeKey(c.identity)
	c.mu.Unlock()
	return data
}

func stateFor(err error) surface.State {
	if err != nil {
		return errorSurface(err)
	}
	return surface.State{Status: surface.Loaded}
}

func errorSurface(err error) surface.State {
	var protocolErr *protocolclient.ProtocolError
	if errors.As(err, &protocolErr) && (protocolErr.Status == 404 || protocolErr.Status == 501) {
		return surface.State{Status: surface.Unavailable, Reason: err.Error()}
	}
	return surface.State{Status: surface.Failed, Reason: err.Error()}
}

// TaskDetail reads an authoritative task result and trajectory projection.
func (c *Controller) TaskDetail(ctx context.Context, taskID string) (types.TaskDetail, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.TaskDetail{}, errors.New("tui: not attached")
	}
	return client.TasksGet(ctx, types.TaskGetRequest{ID: taskID})
}

// ToolDetail reads schema, policy, OAuth, metrics, and content posture.
func (c *Controller) ToolDetail(ctx context.Context, toolID string) (tuitools.Detail, error) {
	c.mu.Lock()
	client := c.client
	annotations := c.capabilities[types.CapToolAnnotations]
	c.mu.Unlock()
	if client == nil {
		return tuitools.Detail{}, errors.New("tui: not attached")
	}
	tool, err := client.ToolsGet(ctx, types.ToolGetRequest{ID: toolID})
	if err != nil {
		return tuitools.Detail{}, err
	}
	detail := tuitools.Detail{Tool: tool, BestEffort: true, Annotations: annotations}
	if !annotations {
		detail.Unavailable = "Runtime does not advertise tool_annotations"
		return detail, nil
	}
	manifest, err := client.ToolsDescribe(ctx, types.ToolDescribeRequest{ID: toolID})
	if err != nil {
		return detail, err
	}
	metrics, err := client.ToolsMetrics(ctx, types.ToolMetricsRequest{ID: toolID, Window: types.ToolWindow1h})
	if err != nil {
		return detail, err
	}
	content, err := client.ToolsContentStats(ctx, types.ToolContentStatsRequest{ID: toolID})
	if err != nil {
		return detail, err
	}
	detail.Manifest, detail.Metrics, detail.Content = &manifest, &metrics, &content
	return detail, nil
}

// Control submits one canonical steering action. The response only verifies
// acceptance; the caller reconciles the effect from events and snapshots.
func (c *Controller) Control(ctx context.Context, method methods.Method, runID, scope string, payload map[string]any) (types.ControlResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.ControlResponse{}, errors.New("tui: not attached")
	}
	return client.Control(ctx, method, types.ControlRequest{Identity: types.IdentityScope{Run: runID, Scope: scope}, Payload: cloneAnyMap(payload)})
}

// Execute is the single mutation boundary used by the TUI action matrix.
// Authority is never inferred client-side; the authenticated server enforces it.
func (c *Controller) Execute(ctx context.Context, mutation Mutation) error {
	c.mu.Lock()
	identity := c.identity
	c.mu.Unlock()
	if scopeKey(identity) != scopeKey(mutation.Identity) {
		return errors.New("tui: stale mutation identity")
	}
	payload := cloneAnyMap(mutation.Payload)
	if mutation.PauseToken != "" {
		if payload == nil {
			payload = map[string]any{}
		}
		payload["token"] = mutation.PauseToken
	}
	switch mutation.Method {
	case methods.MethodArtifactsDelete:
		_, err := c.DeleteArtifact(ctx, mutation.ArtifactID)
		return err
	case methods.MethodToolsSetApprovalPolicy:
		policy, ok := payload["policy"].(string)
		if !ok {
			return errors.New("tui: tool approval policy required")
		}
		_, err := c.SetToolApproval(ctx, mutation.ToolID, types.ToolApprovalPolicy(policy))
		return err
	case methods.MethodToolsRevokeOAuth:
		_, err := c.RevokeToolOAuth(ctx, mutation.ToolID)
		return err
	case methods.MethodSessionsDelete:
		_, err := c.Delete(ctx)
		return err
	default:
		_, err := c.Control(ctx, mutation.Method, mutation.RunID, canonicalAuthority(mutation.Method), payload)
		return err
	}
}

func canonicalAuthority(method methods.Method) string {
	switch method {
	case methods.MethodInjectContext, methods.MethodUserMessage:
		return "session_user"
	case methods.MethodPrioritize:
		return "admin"
	default:
		return "owner_user"
	}
}

// DeleteArtifact requests canonical audited eviction.
func (c *Controller) DeleteArtifact(ctx context.Context, id string) (types.ArtifactsDeleteResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.ArtifactsDeleteResponse{}, errors.New("tui: not attached")
	}
	return client.ArtifactsDelete(ctx, types.ArtifactsDeleteRequest{ID: id})
}

// SetToolApproval updates one canonical tool policy.
func (c *Controller) SetToolApproval(ctx context.Context, id string, policy types.ToolApprovalPolicy) (types.ToolSetApprovalPolicyResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.ToolSetApprovalPolicyResponse{}, errors.New("tui: not attached")
	}
	return client.ToolsSetApprovalPolicy(ctx, types.ToolSetApprovalPolicyRequest{ID: id, Policy: policy})
}

// RevokeToolOAuth revokes canonical tool bindings.
func (c *Controller) RevokeToolOAuth(ctx context.Context, id string) (types.ToolRevokeOAuthResponse, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return types.ToolRevokeOAuthResponse{}, errors.New("tui: not attached")
	}
	return client.ToolsRevokeOAuth(ctx, types.ToolRevokeOAuthRequest{ID: id})
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

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mapsClone(in map[types.Capability]bool) map[types.Capability]bool {
	out := make(map[types.Capability]bool, len(in))
	for key, value := range in {
		out[key] = value
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
