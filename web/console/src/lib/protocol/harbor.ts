// Harbor Console — unified typed Protocol *client* barrel (D-121,
// CONVENTIONS.md §6).
//
// This barrel is the public CLIENT surface of the `protocol/` package:
// the unified `HarborClient`, every per-subsystem namespace, the
// injectable `ProtocolClient` interface, and the single `ProtocolError`
// class. A page imports its Runtime client from `$lib/protocol/harbor.js`
// and never hand-rolls `fetch` (CLAUDE.md §4.5 rule 5, §13).
//
// CONVENTION (D-132 / Wave 13 NIT cleanup): this barrel re-exports ONLY
// the client surface — `HarborClient`, the namespaces, `ProtocolClient`,
// `ProtocolError`. Per-page WIRE TYPES are NOT re-exported here; a
// consumer imports them directly from their owning module
// (`$lib/protocol/<page>.ts` — `tasks.ts`, `events.ts`, `memory-types.ts`,
// `topology.ts`, `posture.ts`, `pause.ts`, `settings.ts`, `agents.ts`,
// `tools.ts`, `flows.ts`, `mcp.ts`, `sessions.ts`, `artifacts.ts`). This
// keeps a single, unambiguous import path per symbol: the client from
// the barrel, every wire type from its module.
//
// NOTE: the legacy generated stub `$lib/protocol.ts` (artifacts wire
// types, D-120) and the five legacy per-page clients still coexist
// transiently until the page-internal refactor wave migrates each page
// onto `HarborClient` (D-121 "Findings I'm departing from"). New Console
// pages use THIS package.

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
	SearchNamespace,
	type ProtocolClient,
	type HarborClientOptions
} from './client.js';

export { ProtocolError, type ProtocolErrorBody } from './errors.js';
