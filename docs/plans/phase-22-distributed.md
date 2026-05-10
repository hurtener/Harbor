# Phase 22 — MessageBus + RemoteTransport contracts (V1: contracts only, A2A v1 spec)

## Summary

Land `internal/distributed/`: the V1 distributed contracts. Two surfaces, one in-process driver each. `MessageBus.Publish` is the at-least-once cross-worker fan-out edge; the V1 driver is in-process loopback (no durable backend ships at V1; that's post-V1 phase 86). `RemoteTransport` is the cross-process / cross-host call surface designed end-to-end against the **full A2A v1 spec** (vendored from `a2aproject/A2A` at commit `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`, 2026-04-23) so Phase 29's southbound A2A driver can wire to it without churn. The V1 driver is `loopback` — request/reply + streaming for in-process integration tests; production A2A wire goes through Phase 29.

## RFC anchor

- RFC §6.4
- RFC §6.12
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 + §5 ("Distributed contracts ship without backends").** "Harbor mirrors that for V1 (deliberate — the user accepted in-process at V1) but versions and freezes the contracts so a distributed driver can ship later without runtime churn." Phase 22 ships the contracts; the in-process loopback `MessageBus`; an A2A-shaped `RemoteTransport` interface; one V1 driver (`loopback`) that satisfies it for integration tests. Durable distributed bus drivers (NATS, Redis Streams, Postgres-as-queue) are post-V1 phase 86; an actual A2A wire driver is Phase 29 (southbound).
- **brief 05 §2 (data shapes — BusEnvelope, RemoteCallRequest/Result, RemoteEventStream).** Phase 22 implements all three.
- **brief 05 §4 (delivery semantics).** "MessageBus.Publish is at-least-once — handlers must be idempotent on (TaskID, Edge, EventID). RemoteTransport.Send is request/reply; Stream yields ordered events with a final done=true." Implemented at the boundary; idempotency-key-based dedup is the consumer's responsibility (the contract makes the requirement explicit).
- **D-007 — A2A: full spec compliance from V1.** Harbor's RemoteTransport surface MUST cover the entire A2A v1 spec — every RPC, every message type, every state transition. Phase 22 maps the proto to Go shapes that Phase 29's southbound driver consumes verbatim. The proto is vendored to `docs/specifications/a2a.proto` so the source-of-truth is in-repo (per the master plan's "in-repo docs" hygiene principle).

## Findings I'm departing from (if any)

- **Brief 05 §2 sketches `RemoteTransport` with five terse methods (Send / Stream / GetTask / Subscribe / Cancel).** Harbor extends the surface to the full A2A v1 RPC set so the seam doesn't churn when Phase 29 lands. New methods: `ListTasks`, `CreateTaskPushNotificationConfig`, `GetTaskPushNotificationConfig`, `ListTaskPushNotificationConfigs`, `DeleteTaskPushNotificationConfig`, `GetExtendedAgentCard`. Each maps 1:1 to an A2A RPC. Documented + the brief sketch is preserved in §6.12 of the RFC (it captured intent; this is a faithful expansion, not a redesign).

## Goals

- Ship `internal/distributed/` as a new top-level subdirectory under `internal/` (AGENTS.md §3 update bundled — see Acceptance criteria).
- One `MessageBus` interface (Publish only; Subscribe lands when a durable driver does at post-V1 86) + one V1 driver (`loopback`).
- One `RemoteTransport` interface covering the **full A2A v1 RPC set** + one V1 driver (`loopback`) that exercises it without leaving the process. The Loopback driver registers an in-process `RemoteAgent` (functional implementation of the seven A2A RPCs) and routes RemoteTransport calls to it.
- Vendor `a2a.proto` at `docs/specifications/a2a.proto` (committed by this PR; commit-SHA-pinned in `docs/specifications/README.md`).
- Define A2A core Go shapes in `internal/distributed/a2a/types.go`: `Task`, `TaskStatus`, `TaskState` (8-state enum), `Message`, `Part` (oneof: text/raw/url/data + filename + media_type), `Role`, `Artifact`, `TaskStatusUpdateEvent`, `TaskArtifactUpdateEvent`, `AgentCard`, `AgentInterface`, `AgentProvider`, `AgentCapabilities`, `AgentExtension`, `AgentSkill`, `AgentCardSignature`, `AuthenticationInfo`, `TaskPushNotificationConfig`, `SecurityScheme` (oneof: APIKey/HTTPAuth/OAuth2/OIDC/MutualTLS), `OAuthFlows` + the four flow concretes, `SecurityRequirement`, `StringList`. These are pure Go shapes (transcribed from the proto); the actual gRPC stubs live in Phase 29.
- Identity-mandatory at every boundary. `MessageBus.Publish` rejects empty triple. `RemoteTransport` methods receive `identity.Quadruple` via `ctx`; missing identity returns `ErrIdentityRequired`.
- Audit redaction: `BusEnvelope.Payload` and `RemoteCallRequest`/`RemoteCallResult` payloads run through `audit.Redactor` before crossing a transport boundary (caller-side per D-020).
- Cross-package `conformancetest.Run` for both `MessageBus` and `RemoteTransport`. Loopback driver passes both. Future drivers (post-V1 86 + Phase 29) inherit verbatim.

## Non-goals

- No real network transport. `loopback` is in-process. The actual A2A gRPC / JSON-RPC / HTTP+JSON wire bindings ship in Phase 29 as the southbound A2A driver. Phase 22 does NOT depend on `google.golang.org/grpc`, `google.golang.org/protobuf`, or any HTTP framework.
- No durable distributed bus. Post-V1 phase 86 (NATS / Redis Streams / Postgres-as-queue).
- No A2A *northbound* server (Harbor accepting A2A calls from peers). RFC §11 Q-2 leans V1.1; not in V1 cut.
- No protobuf-generated code at this phase. Phase 22 transcribes the A2A types into hand-written Go (`internal/distributed/a2a/types.go`) so the package has no generated-code dependency. Phase 29's southbound driver MAY use generated code from `protoc` for the gRPC binding; Phase 22 doesn't.
- No push notifications dispatch. `RemoteTransport.CreateTaskPushNotificationConfig` etc. only manage CRUD on the config; the actual outbound push notification machinery lives in Phase 29.
- No A2A AgentCard publication. The runtime's own AgentCard (when Harbor exposes its A2A *northbound* surface) is V1.1; Phase 22's `GetExtendedAgentCard` is consumer-only.
- No retry / backoff at the transport layer. `ToolPolicy` (D-024) wraps Tool invocations; `NodePolicy` wraps engine nodes; transport callers compose these.

## Acceptance criteria

- [ ] `internal/distributed/distributed.go` (new) defines:
  - `BusEnvelope` struct: `Edge, Source, Target string; Identity identity.Quadruple; TaskID tasks.TaskID; Payload json.RawMessage; Headers map[string]string; Meta map[string]any; EventID events.EventID`.
  - `MessageBus` interface: `Publish(ctx, env BusEnvelope) error`. Documented contract: at-least-once delivery; handlers idempotent on `(TaskID, Edge, EventID)`.
  - Sentinel errors: `ErrBusClosed`, `ErrIdentityRequired`, `ErrUnknownDriver` (mirrors `state` / `events` patterns).
- [ ] `internal/distributed/remote.go` (new) defines:
  - `RemoteCallRequest` / `RemoteCallResult` / `RemoteEventStream` shapes.
  - `RemoteTaskSnapshot` (returned by `GetTask`).
  - `RemoteTaskEventStream` (returned by `Subscribe`).
  - `RemoteTransport` interface — the FULL A2A RPC set, mapped 1:1 from `docs/specifications/a2a.proto`'s `A2AService` RPCs (full Go shape shown below).

```go
type RemoteTransport interface {
    // Maps to A2A SendMessage.
    Send(ctx context.Context, req RemoteCallRequest) (RemoteCallResult, error)
    // Maps to A2A SendStreamingMessage / SubscribeToTask depending on
    // request shape; the driver routes by req.Kind.
    Stream(ctx context.Context, req RemoteCallRequest) (RemoteEventStream, error)
    // Maps to A2A GetTask.
    GetTask(ctx context.Context, taskID, contextID string) (*RemoteTaskSnapshot, error)
    // Maps to A2A ListTasks.
    ListTasks(ctx context.Context, filter RemoteTaskFilter) ([]RemoteTaskSnapshot, error)
    // Maps to A2A CancelTask.
    Cancel(ctx context.Context, taskID, contextID string) error
    // Maps to A2A SubscribeToTask (when the caller wants live updates
    // for a task it didn't initiate).
    Subscribe(ctx context.Context, taskID, contextID string) (RemoteTaskEventStream, error)
    // Push-notification config CRUD, mapping to the A2A
    // TaskPushNotificationConfig RPCs.
    CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
    GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
    ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
    DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error
    // Maps to A2A GetExtendedAgentCard.
    GetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error)
    // Lifecycle.
    Close(ctx context.Context) error
}
```

- [ ] `internal/distributed/a2a/types.go` (new) transcribes the A2A v1 message types from `docs/specifications/a2a.proto`. Key types implemented:
  - `Task`, `TaskStatus`, `TaskState` (8-state enum: `TaskStateUnspecified`, `TaskStateSubmitted`, `TaskStateWorking`, `TaskStateCompleted`, `TaskStateFailed`, `TaskStateCanceled`, `TaskStateInputRequired`, `TaskStateRejected`, `TaskStateAuthRequired`).
  - `Message` (with `Role` enum: `RoleUnspecified` / `RoleUser` / `RoleAgent`), `Part` (Go-flavored discriminated union: `TextPart`, `RawPart`, `URLPart`, `DataPart` — each with `Filename`, `MediaType`, `Metadata`).
  - `Artifact`, `TaskStatusUpdateEvent`, `TaskArtifactUpdateEvent`.
  - `AgentCard`, `AgentInterface` (with `ProtocolBinding string` accepting `"JSONRPC"`, `"GRPC"`, `"HTTP+JSON"`), `AgentProvider`, `AgentCapabilities` (Streaming / PushNotifications / Extensions / ExtendedAgentCard), `AgentExtension`, `AgentSkill`, `AgentCardSignature`.
  - `AuthenticationInfo` (push-notification credentials).
  - `TaskPushNotificationConfig`.
  - `SecurityScheme` discriminated union: `APIKeySecurityScheme`, `HTTPAuthSecurityScheme`, `OAuth2SecurityScheme`, `OpenIdConnectSecurityScheme`, `MutualTlsSecurityScheme`.
  - `OAuthFlows` discriminated union: `AuthorizationCodeOAuthFlow`, `ClientCredentialsOAuthFlow`, `ImplicitOAuthFlow` (deprecated; included for spec parity), `PasswordOAuthFlow` (deprecated; included), `DeviceCodeOAuthFlow`.
  - `SecurityRequirement`, `StringList`.
  Each type has a godoc comment quoting its source proto comment.
- [ ] `internal/distributed/a2a/types_test.go` covers JSON round-trip (every message type marshals + unmarshals byte-equal), enum validity (`TaskState` recognises all 8 values + a 9th sentinel for `unspecified`), and oneof discrimination (`Part` correctly identifies which sub-type a value carries).
- [ ] `internal/distributed/registry.go` provides `Register(name, factory)` / `Open(ctx, cfg)` / `OpenDriver(name, cfg)` / `RegisteredDrivers()` for BOTH `MessageBus` and `RemoteTransport` (two parallel registries; same shape mirrored from `internal/state/registry.go`).
- [ ] `internal/distributed/drivers/loopback/loopback.go` (new) implements:
  - `MessageBus` (in-process publish; subscribers are wired via the `events.EventBus` for V1 — the brief's clarification that "the bus is a fan-out projection of the typed event bus" is honoured by routing `BusEnvelope.Payload` through a typed `events.Event`).
  - `RemoteTransport` (in-process A2A: maintains an in-memory `agentRegistry` mapping agent URLs to a hosting `Agent` interface; calls dispatch synchronously). The driver implements every RemoteTransport method against an `Agent` callback that simulates a remote A2A endpoint.
- [ ] `internal/distributed/drivers/loopback/agent.go` (new) defines the `Agent` interface that the loopback driver invokes for each call (Go shape below).

```go
type Agent interface {
    SendMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error)
    SendStreamingMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error)
    GetTask(ctx context.Context, taskID, contextID string) (a2a.Task, error)
    ListTasks(ctx context.Context, filter ListTasksFilter) ([]a2a.Task, error)
    CancelTask(ctx context.Context, taskID, contextID string) (a2a.Task, error)
    SubscribeToTask(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error)
    CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
    GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
    ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
    DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error
    GetExtendedAgentCard(ctx context.Context) (a2a.AgentCard, error)
}
```

- [ ] `internal/distributed/conformancetest/conformancetest.go` exports `RunBus(t, factory)` and `RunRemoteTransport(t, factory)`. Subtests cover:
  - Bus: `Publish_AtLeastOnce_DeliversToSubscribers`, `Publish_Identity_Mandatory`, `Publish_AfterClose_Errors`, `Concurrent_Publish_NoRace`, `GoroutineLeak_AfterClose`.
  - RemoteTransport: `Send_RoundTrip`, `Send_Identity_Mandatory`, `Stream_OrderedEventsWithDoneTrue`, `Stream_RespectsClose`, `GetTask_RoundTrip`, `GetTask_NotFound`, `ListTasks_FilterApplied`, `Cancel_TerminalState`, `Subscribe_DeliversArtifactAndStatusUpdates`, `Subscribe_RespectsClose`, `PushNotificationConfig_Crud_RoundTrip`, `GetExtendedAgentCard_HappyPath`, `Concurrent_Send_NoRace`, `GoroutineLeak_AfterClose`.
- [ ] `internal/distributed/conformancetest/conformancetest_test.go` self-applies both Run* against the loopback driver.
- [ ] `cmd/harbor/main.go` adds `_ "github.com/hurtener/Harbor/internal/distributed/drivers/loopback"`.
- [ ] **Config additions:** `internal/config/config.go` gains `DistributedConfig{ BusDriver, RemoteDriver string }` (defaults `"loopback"`/`"loopback"`); `validate.go` rejects empty driver names.
- [ ] **AGENTS.md / CLAUDE.md update**: §3 "Repository layout" gains the `internal/distributed/` directory entry. Mirror invariant maintained.
- [ ] **`docs/specifications/a2a.proto`** committed verbatim (the proto file pulled at commit `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`).
- [ ] **`docs/specifications/README.md`** committed (this PR establishes the convention).
- [ ] **`docs/decisions.md` D-031**: distributed contracts surface = full A2A v1 mapping + loopback V1 driver; vendored proto pinned by commit SHA.
- [ ] Coverage on `internal/distributed` ≥ 85%; `internal/distributed/a2a` ≥ 85%; `internal/distributed/drivers/loopback` ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-22.sh` present and executable.
- [ ] `docs/glossary.md` adds A2A vocabulary: `MessageBus`, `RemoteTransport`, `BusEnvelope`, `RemoteCallRequest`, `Agent` (A2A peer), `AgentCard`, `AgentInterface`, `AgentSkill`, `Task` (A2A), `Message` (A2A), `Part` (A2A), `Artifact` (A2A), `TaskStatusUpdateEvent`, `TaskArtifactUpdateEvent`, `TaskPushNotificationConfig`, `SecurityScheme`. Note: A2A `Task` and Harbor `Task` (Phase 20) are different types with overlapping semantics; the glossary disambiguates.
- [ ] `docs/plans/README.md` Phase 22 row Status flips to Shipped.

## Files added or changed

- `internal/distributed/distributed.go` (new)
- `internal/distributed/remote.go` (new)
- `internal/distributed/registry.go` (new)
- `internal/distributed/distributed_test.go` (new) — registry surface tests
- `internal/distributed/a2a/types.go` (new)
- `internal/distributed/a2a/types_test.go` (new)
- `internal/distributed/a2a/doc.go` (new) — package overview citing the vendored proto
- `internal/distributed/conformancetest/conformancetest.go` (new)
- `internal/distributed/conformancetest/conformancetest_test.go` (new)
- `internal/distributed/drivers/loopback/loopback.go` (new)
- `internal/distributed/drivers/loopback/agent.go` (new)
- `internal/distributed/drivers/loopback/loopback_test.go` (new)
- `internal/config/config.go` (modified) — `DistributedConfig`
- `internal/config/loader.go` / `validate.go` (modified)
- `cmd/harbor/main.go` (modified) — additive blank import
- `AGENTS.md` (modified) — §3 layout adds `internal/distributed/`
- `CLAUDE.md` (modified) — verbatim mirror
- `docs/specifications/README.md` (new)
- `docs/specifications/a2a.proto` (vendored from `a2aproject/A2A` @ `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`)
- `docs/plans/phase-22-distributed.md` (this file)
- `docs/plans/README.md` (modified)
- `docs/glossary.md` (modified)
- `docs/decisions.md` (modified) — D-031 entry
- `scripts/smoke/phase-22.sh` (new)
- `examples/harbor.yaml` (modified) — document `distributed.*` fields
- `go.mod` / `go.sum` (modified if needed) — no new direct deps expected (Phase 22 is hand-written Go; no protobuf / gRPC pull-in here)

`internal/distributed/` is a new top-level subdirectory; AGENTS.md §3 is updated in the same PR.

## Public API surface

```go
package distributed

import (
    "context"
    "encoding/json"
    "errors"
    "time"

    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/distributed/a2a"
    "github.com/hurtener/Harbor/internal/events"
    "github.com/hurtener/Harbor/internal/identity"
    "github.com/hurtener/Harbor/internal/tasks"
)

type BusEnvelope struct {
    Edge, Source, Target string
    Identity             identity.Quadruple
    TaskID               tasks.TaskID
    EventID              events.EventID
    Payload              json.RawMessage
    Headers              map[string]string
    Meta                 map[string]any
    Timestamp            time.Time
}

type MessageBus interface {
    Publish(ctx context.Context, env BusEnvelope) error
    Close(ctx context.Context) error
}

type RemoteCallRequest struct {
    AgentURL  string
    Kind      RemoteCallKind  // "send" | "stream" | "subscribe"
    ContextID string          // A2A context_id; empty for new contexts
    TaskID    string          // A2A task_id; empty for new tasks
    Message   a2a.Message
    Config    a2a.SendMessageConfiguration
    Timeout   time.Duration
}

type RemoteCallResult struct {
    Task       a2a.Task
    HTTPStatus int  // 0 when transport doesn't carry HTTP semantics
}

type RemoteEventStream interface {
    Recv(ctx context.Context) (a2a.StreamResponse, error)
    Close() error
}

type RemoteTaskSnapshot a2a.Task
type RemoteTaskEventStream RemoteEventStream

type RemoteTaskFilter struct {
    Tenant string
    PageSize int
    PageToken string
}

type RemoteTransport interface {
    Send(ctx context.Context, req RemoteCallRequest) (RemoteCallResult, error)
    Stream(ctx context.Context, req RemoteCallRequest) (RemoteEventStream, error)
    GetTask(ctx context.Context, taskID, contextID string) (*RemoteTaskSnapshot, error)
    ListTasks(ctx context.Context, filter RemoteTaskFilter) ([]RemoteTaskSnapshot, error)
    Cancel(ctx context.Context, taskID, contextID string) error
    Subscribe(ctx context.Context, taskID, contextID string) (RemoteTaskEventStream, error)
    CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
    GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
    ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
    DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error
    GetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error)
    Close(ctx context.Context) error
}

var (
    ErrBusClosed        = errors.New("distributed: bus is closed")
    ErrTransportClosed  = errors.New("distributed: remote transport is closed")
    ErrIdentityRequired = errors.New("distributed: identity required (tenant/user/session)")
    ErrUnknownDriver    = errors.New("distributed: unknown driver")
    ErrAgentNotFound    = errors.New("distributed: A2A agent not registered with this transport")
    ErrTaskNotFound     = errors.New("distributed: A2A task not found")
    ErrInvalidPart      = errors.New("distributed: invalid A2A Part (oneof empty)")
)

type BusFactory      func(deps Dependencies) (MessageBus, error)
type RemoteFactory   func(deps Dependencies) (RemoteTransport, error)

type Dependencies struct {
    EventBus events.EventBus
    Cfg      config.DistributedConfig
}

func RegisterBus(name string, factory BusFactory)
func RegisterRemoteTransport(name string, factory RemoteFactory)
func OpenBus(ctx context.Context, deps Dependencies) (MessageBus, error)
func OpenRemoteTransport(ctx context.Context, deps Dependencies) (RemoteTransport, error)
func RegisteredBusDrivers() []string
func RegisteredRemoteTransportDrivers() []string
```

A2A types live in the sibling `internal/distributed/a2a` package; consumers (Phase 29 southbound, eventual Phase 89 northbound) import that package.

## Test plan

- **Unit:** `BusEnvelope` validation (identity-required); `RemoteCallRequest.Kind` discrimination; A2A type marshal/unmarshal round-trip; A2A `Part` oneof discrimination matrix.
- **Integration:** `conformancetest.RunBus` + `RunRemoteTransport` against the loopback driver (in-package). Wave-end E2E (`test/integration/wave6_test.go`) wires loopback + tasks + state + sessions to prove the cross-subsystem composition end-to-end.
- **Conformance:** the suite IS the gate. The loopback driver's pass is the proof that the contract is implementable; Phase 29 (A2A southbound) inherits the suite verbatim.
- **Concurrency / leak (D-025):** `Concurrent_Publish_NoRace` for MessageBus; `Concurrent_Send_NoRace` for RemoteTransport. N≥128 each; baseline restored.

## Smoke script additions

- `scripts/smoke/phase-22.sh`:
  - `go test -race -count=1 -timeout 90s ./internal/distributed/...` → OK on green.
  - `skip "phase 22: distributed contracts have no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/distributed`: 85%.
- `internal/distributed/a2a`: 85% (heavy on data-shape coverage, low on code paths).
- `internal/distributed/drivers/loopback`: 85%.
- `internal/distributed/conformancetest`: not gated (precedent: Phase 07 / 17).

## Dependencies

- Phase 09 (envelopes / messages) — `BusEnvelope` reuses `events.EventID` and the typed event bus's identity-quadruple shape.
- Phase 20 (TaskRegistry) — `BusEnvelope.TaskID` references `tasks.TaskID`.
- Phase 05 (events / EventBus) — the loopback `MessageBus` driver routes envelopes through the typed event bus.

## Risks / open questions

- **A2A spec drift between vendored copy and upstream.** Mitigation: the proto is committed at a pinned SHA; bumping is a `deps(specs):` PR with explicit reviewer attention. The Go shapes in `internal/distributed/a2a/types.go` are hand-transcribed; their `types_test.go` includes golden-byte tests against the vendored proto's text-form examples (when the spec provides them).
- **Hand-transcribed Go vs `protoc`-generated Go.** Phase 22 chooses hand-transcribed because:
  1. Phase 22 should not pull `google.golang.org/grpc` / `google.golang.org/protobuf` until Phase 29 ships an actual gRPC binding (avoid forcing transitive deps on consumers that don't need them).
  2. The Go shapes are easier to evolve in lockstep with Harbor's idioms (`identity.Quadruple` integration, slog logging, error wrapping) when hand-written.
  3. Phase 29 may use `protoc` for the wire binding; the resulting types will be wrapped to match `internal/distributed/a2a/types.go` (or the wire-binding driver will translate at the boundary). The seam is owned by Phase 29.
- **`oneof` translation idiom.** Go has no native discriminated unions; the chosen idiom is one Go interface per oneof + concrete types per variant + a `Kind() string` method for runtime discrimination. Documented inline; tested by `types_test.go`.
- **MessageBus `Publish` is at-least-once but loopback is exactly-once.** The loopback driver delivers exactly-once; the contract documents at-least-once so consumers MUST be idempotent. Future durable drivers (NATS / Postgres-as-queue) WILL produce duplicates; consumers that work against loopback also work against them. This is intentional API hardening from t=0.
- **`RemoteTransport.Stream` resource lifetime.** The `RemoteEventStream` returned MUST be `Close()`d by the caller to release resources (channel + goroutine). The conformance suite's `Stream_RespectsClose` covers it; consumers MUST defer `Close()`.
- **Push notification config storage.** V1's loopback driver stores configs in memory keyed by `(tenant, taskID, configID)`. Phase 29 + post-V1 durable drivers will persist via StateStore. V1 contract does NOT mandate durability for these configs.
- **`internal/distributed/` as a new top-level subdirectory.** AGENTS.md §3 update bundled in this PR. Per AGENTS.md §15 ("If you discover a rule that should exist but doesn't, add it"); reasonable per-§3 since the master plan's "subsystem: distributed" already implies the directory.

## Glossary additions

- **`MessageBus`** — Harbor's at-least-once cross-worker fan-out edge. V1 ships in-process loopback; durable backends (NATS / Redis Streams / Postgres-as-queue) are post-V1 phase 86. Handlers MUST be idempotent on `(TaskID, Edge, EventID)`.
- **`RemoteTransport`** — Harbor's cross-process / cross-host call surface, designed end-to-end against the A2A v1 spec. V1 ships an in-process loopback driver; the production A2A wire driver is Phase 29 (southbound).
- **`BusEnvelope`** — the unit `MessageBus.Publish` accepts. Carries identity quadruple, task ID, edge / source / target labels, and the redacted payload.
- **`A2A` (Agent-to-Agent)** — the open protocol Harbor adopts for cross-agent communication. Vendored spec at `docs/specifications/a2a.proto`; full spec compliance is settled per D-007.
- **`AgentCard`** — A2A's self-describing manifest for an agent. Carries name, capabilities, skills, supported interfaces (gRPC / JSON-RPC / HTTP+JSON), security schemes. Harbor consumes peers' AgentCards through `RemoteTransport.GetExtendedAgentCard`.
- **`AgentInterface`** — A2A's declaration of a target URL + protocol binding (`JSONRPC` / `GRPC` / `HTTP+JSON`) + protocol version. An `AgentCard` carries one or more `AgentInterface`s.
- **`AgentSkill`** — A2A's declaration of a distinct capability the agent exposes.
- **`Part` (A2A)** — A2A's discriminated message-content carrier (oneof: text, raw bytes, URL, structured data). Each part carries `media_type` + optional `filename`. Distinct from Harbor's `ContentPart` (D-021); the LLM-side multimodal types map onto A2A `Part` at the southbound boundary.
- **`Task` (A2A)** — A2A's task abstraction. Distinct from Harbor's `tasks.Task` (Phase 20): Harbor's task is the local-runtime unit; A2A's `Task` is what a remote agent uses to model the same execution. Mapping happens at the Phase 29 boundary.
- **`TaskStatusUpdateEvent` / `TaskArtifactUpdateEvent`** — A2A streaming-event types delivered via `SendStreamingMessage` / `SubscribeToTask`. Harbor's `RemoteEventStream.Recv` returns these.
- **`TaskPushNotificationConfig`** — A2A's per-task push-notification configuration (URL + auth credentials + optional token). Harbor's `RemoteTransport` exposes CRUD; V1 stores in memory, post-V1 + Phase 29 add durability + outbound dispatch.
- **`SecurityScheme`** — A2A's discriminated union of supported authentication schemes (API key, HTTP auth, OAuth 2.0, OpenID Connect, mTLS). Used by `AgentCard.security_schemes`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (AGENTS.md §3 update mirrored to CLAUDE.md)
- [ ] All cross-references resolve
- [ ] Coverage targets met
- [ ] Multi-isolation: identity-required tests pass for both bus + transport
- [ ] **Concurrent-reuse tests pass** — `Concurrent_Publish_NoRace` + `Concurrent_Send_NoRace` with N≥128 under `-race` (D-025).
- [ ] If new vocabulary: glossary updated (yes — A2A vocabulary listed).
- [ ] D-031 entry filed in decisions.md (full A2A v1 surface mapping + vendored proto pin).
- [ ] If a brief finding was departed from: yes — surface expansion documented in "Findings I'm departing from."
