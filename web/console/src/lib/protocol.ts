// HAND-MAINTAINED — keep in lockstep with
// internal/protocol/singlesource.CanonicalWireTypes.
//
// D-093 specifies a `cmd/harbor-gen-protocol-ts` generator that would
// regenerate this file from the Go `CanonicalWireTypes` single source,
// with a `make protocol-ts-gen-check` CI gate. That generator has NOT
// been built — Phase 72h committed this file as a hand-shaped stub and
// later phases hand-extended it. The Wave 13 §17.5 checkpoint (D-132 /
// W10) corrected the formerly-false `// CODE GENERATED … DO NOT EDIT`
// header to this accurate "hand-maintained" notice and filed a tracking
// issue for building the generator + the CI gate. Until then: this file
// is hand-maintained, and any Go-side wire-type change MUST be mirrored
// here by hand.
//
// Per-page wire types live in `$lib/protocol/<page>.ts` (Tools / Memory
// / MCP / Flows / Sessions / Agents / Artifacts). This module retains
// only the Console-DB-facing `OperatorIdentity` shape; the artifacts
// wire types moved to `$lib/protocol/artifacts.ts` (D-132 / W6) and are
// re-exported here for any legacy `$lib/protocol.js` import path.

export type {
  ArtifactScope,
  ArtifactSource,
  SizeRange,
  TimeRange,
  ArtifactRef,
  ArtifactRow,
  ArtifactsListRequest,
  ArtifactsListResponse,
  ArtifactsPutOpts,
  ArtifactsPutRequest,
  ArtifactsPutResponse,
  ArtifactsGetRefRequest,
  ArtifactsGetRefResponse
} from './protocol/artifacts.js';

/**
 * The Console's view of the operator identity carried by every Protocol
 * request. Hashed into the Console DB `operator_id` row-scope key — see
 * `src/lib/db/schema.ts`.
 */
export interface OperatorIdentity {
  tenantID: string;
  userID: string;
}
