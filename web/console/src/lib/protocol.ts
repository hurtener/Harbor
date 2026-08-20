// HAND-MAINTAINED, mechanically LOCKSTEP-GATED — keep in lockstep with
// internal/protocol/singlesource.CanonicalWireTypes.
//
// The per-page wire-type interfaces (this module + `$lib/protocol/<page>.ts`
// + `$lib/sessions/types.ts` + `$lib/flows/types.ts`) are hand-maintained,
// but a field-level lockstep gate (`make protocol-ts-gen-check`, D-223) now
// holds them to the Go single source: a Go generator emits the committed
// wire manifest `$lib/protocol/wire-manifest.gen.json` from
// `CanonicalWireTypes`, and a TS-source scan
// (`scripts/check-protocol-ts-lockstep.mjs`, in `npm run lint`) fails CI when
// a hand-written interface drops, renames, or mistypes a manifest field, or
// when a new Go wire type lands with neither a TS interface nor a justified
// entry in `scripts/protocol-ts-untyped-allow.json`. Any Go-side wire-type
// change MUST still be mirrored here by hand AND the manifest regenerated
// (`make protocol-ts-gen`); the gate catches a missed mirror.
//
// FULL GENERATION of the per-domain TypeScript type modules (D-093's
// original ambition) is a deferred future deliverable; until it lands the
// `cmd/harbor-gen-protocol-ts` name is reserved for it. Today the Go tool is
// `cmd/harbor-protocol-ts-lockstep` — it VERIFIES lockstep, it does not
// generate this file.
//
// Per-page wire types live in `$lib/protocol/<page>.ts` (Tools / Memory
// / MCP / Flows / Sessions / Agents / Artifacts). This module retains
// only the Console-DB-facing `OperatorIdentity` shape; the artifacts
// wire types moved to `$lib/protocol/artifacts.ts` and are
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
  ArtifactsGetRequest,
  ArtifactsGetResponse,
  ArtifactsGetRefRequest,
  ArtifactsGetRefResponse
} from './protocol/artifacts.js';

export type {
  SkillPublicationAvailableRequest,
  SkillPublicationAvailableResponse,
  SkillPublicationGetRequest,
  SkillPublicationGetResponse,
  SkillPublicationInstallRequest,
  SkillPublicationInstallResponse,
  SkillPublicationListRequest,
  SkillPublicationListResponse,
  SkillPublicationMetadata,
  SkillPublicationPublishRequest,
  SkillPublicationPublishResponse,
  SkillPublicationReceipt,
  SkillPublicationReference,
  SkillPublicationReferencesListRequest,
  SkillPublicationReferencesListResponse,
  SkillPublicationRemoveRequest,
  SkillPublicationRemoveResponse,
  SkillPublicationRetireRequest,
  SkillPublicationRetireResponse,
  SkillPublicationSuccessorRequest,
  SkillPublicationSuccessorResponse,
  SkillPublicationUpdateRequest,
  SkillPublicationUpdateResponse
} from './protocol/skill-publications.js';

/**
 * The Console's view of the operator identity carried by every Protocol
 * request. Hashed into the Console DB `operator_id` row-scope key — see
 * `src/lib/db/schema.ts`.
 */
export interface OperatorIdentity {
  tenantID: string;
  userID: string;
}
