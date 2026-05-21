// Harbor Console — unified typed Protocol client package (D-121,
// CONVENTIONS.md §6).
//
// This barrel is the public surface of the `protocol/` package: the unified
// `HarborClient`, the injectable `ProtocolClient` interface, and the single
// `ProtocolError` class. A page imports from `$lib/protocol` and never
// hand-rolls `fetch` (CLAUDE.md §4.5 rule 5, §13).
//
// NOTE: the legacy generated stub `$lib/protocol.ts` (artifacts wire types,
// D-120) and the five legacy per-page clients still coexist transiently until
// the page-internal refactor wave migrates each page onto `HarborClient`
// (D-121 "Findings I'm departing from"). New Console pages use THIS package.

export {
	HarborClient,
	Transport,
	ToolsNamespace,
	TasksNamespace,
	ControlNamespace,
	MemoryNamespace,
	FlowsNamespace,
	ArtifactsNamespace,
	MCPNamespace,
	MCPServersNamespace,
	EventsNamespace,
	AgentsNamespace,
	SessionsNamespace,
	TopologyNamespace,
	RunsNamespace,
	RuntimeNamespace,
	PauseNamespace,
	PostureNamespace,
	AuthNamespace,
	type ProtocolClient,
	type HarborClientOptions
} from './client.js';

export { ProtocolError, type ProtocolErrorBody } from './errors.js';

export type {
	TaskStatus,
	TaskKind,
	TaskIdentity,
	TaskRow,
	TaskFilter,
	TaskListAggregates,
	TaskListStatusCounterStrip,
	TaskListCursor,
	TaskListRequest,
	TaskListResponse,
	TaskDetail
} from './tasks.js';

export type {
	TopologyNodeKind,
	TopologyNode,
	TopologyEdge,
	TopologyProjection
} from './topology.js';

// NOTE: `SubsystemHealth` / `RuntimeHealth` are NOT re-exported here from
// `settings.js` — the barrel sources those two names from `posture.js`
// below (the dedicated runtime-posture module, which also carries
// `HealthStatus`). `settings.ts` keeps its own structurally-identical
// copies for its internal page use; barrel consumers get the single
// `posture.js` definition.
export {
	MOCK_MODE_BANNER,
	type Capability,
	type RuntimeInfo,
	type SubsystemDriver,
	type RuntimeDrivers,
	type RateLimitView,
	type IdentityTierView,
	type GovernancePostureResponse,
	type LLMPostureResponse,
	type AuthRotateTokenResponse
} from './settings.js';

export {
	WINDOW_SPEC,
	isEventArtifactRef,
	type Event,
	type EventArtifactRef,
	type EventFilter,
	type EventBucket,
	type EventAggregateRequest,
	type EventAggregateResponse,
	type TimeWindow
} from './events.js';

export type {
	RuntimeCounters,
	RuntimeHealth,
	SubsystemHealth,
	HealthStatus
} from './posture.js';

export {
	DEFAULT_PAUSE_LIST_PAGE_SIZE,
	MAX_PAUSE_LIST_PAGE_SIZE,
	type PauseSnapshot,
	type PauseSnapshotState,
	type PauseArtifactRef,
	type PauseFilter,
	type PauseListRequest,
	type PauseListResponse
} from './pause.js';

export {
	DEFAULT_MEMORY_LIST_PAGE_SIZE,
	MAX_MEMORY_LIST_PAGE_SIZE,
	type MemoryScope,
	type MemoryStrategyName,
	type MemoryDriverName,
	type IdentityScope,
	type MemoryItem,
	type MemoryFilter,
	type MemoryListRequest,
	type MemoryAggregates,
	type MemoryListResponse,
	type MemoryArtifactRef,
	type MemoryMetadata,
	type MemoryGetRequest,
	type MemoryItemDetail,
	type MemoryGetResponse,
	type MemoryHealthAggregate,
	type MemoryHealthResponse
} from './memory-types.js';
