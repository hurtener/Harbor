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
	MemoryNamespace,
	FlowsNamespace,
	ArtifactsNamespace,
	MCPNamespace,
	MCPServersNamespace,
	type ProtocolClient,
	type HarborClientOptions
} from './client.js';

export { ProtocolError, type ProtocolErrorBody } from './errors.js';

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
