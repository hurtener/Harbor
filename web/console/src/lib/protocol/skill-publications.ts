/**
 * Skill-publication wire types — the `skills.publications.*` Protocol family.
 *
 * These interfaces mirror `internal/protocol/types/skill_publications.go`
 * field-for-field. The Go single source and the committed wire manifest are
 * mechanically checked against this hand-maintained Console module by
 * `make protocol-ts-gen-check`.
 *
 * `SkillPublicationSkill` is a Go alias of `AgentConfigAgentPackItem`, so the
 * canonical wire manifest uses that existing interface directly rather than
 * introducing a second TypeScript shape.
 */

import type { AgentConfigAgentPackItem } from './agentconfig.js';
import type { IdentityScope } from './memory-types.js';

export interface SkillPublicationAvailableRequest {
	identity: IdentityScope;
}

export interface SkillPublicationAvailableResponse {
	publications?: SkillPublicationMetadata[];
	protocol_version: string;
}

export interface SkillPublicationGetRequest {
	identity: IdentityScope;
	publication_id: string;
}

export interface SkillPublicationGetResponse {
	publication: SkillPublicationMetadata;
	protocol_version: string;
}

export interface SkillPublicationInstallRequest {
	identity: IdentityScope;
	agent_id: string;
	publication_id: string;
	revision_id: string;
	expected_absent: boolean;
	idempotency_key: string;
}

export interface SkillPublicationInstallResponse {
	reference: SkillPublicationReference;
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}

export interface SkillPublicationListRequest {
	identity: IdentityScope;
}

export interface SkillPublicationListResponse {
	publications?: SkillPublicationMetadata[];
	protocol_version: string;
}

export interface SkillPublicationMetadata {
	publication_id: string;
	revision_id: string;
	name: string;
	content_hash: string;
	state: string;
	generation: number;
	runtime_id: string;
}

export interface SkillPublicationPublishRequest {
	identity: IdentityScope;
	name: string;
	skill: AgentConfigAgentPackItem;
	idempotency_key: string;
	expected_absent: boolean;
}

export interface SkillPublicationPublishResponse {
	publication: SkillPublicationMetadata;
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}

export interface SkillPublicationReceipt {
	operation_id: string;
	operation: string;
	publication_id?: string;
	revision_id?: string;
	generation: number;
	state: string;
	before_hash?: string;
	after_hash?: string;
}

export interface SkillPublicationReference {
	agent_id: string;
	publication_id: string;
	revision_id: string;
	content_hash: string;
	generation: number;
	runtime_id: string;
	state: string;
}

export interface SkillPublicationReferencesListRequest {
	identity: IdentityScope;
}

export interface SkillPublicationReferencesListResponse {
	references?: SkillPublicationReference[];
	protocol_version: string;
}

export interface SkillPublicationRemoveRequest {
	identity: IdentityScope;
	agent_id: string;
	expected_generation: number;
	expected_content_hash: string;
	idempotency_key: string;
}

export interface SkillPublicationRemoveResponse {
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}

export interface SkillPublicationRetireRequest {
	identity: IdentityScope;
	publication_id: string;
	expected_generation: number;
	expected_content_hash: string;
	idempotency_key: string;
}

export interface SkillPublicationRetireResponse {
	publication: SkillPublicationMetadata;
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}

export interface SkillPublicationSuccessorRequest {
	identity: IdentityScope;
	publication_id: string;
	expected_generation: number;
	expected_content_hash: string;
	skill: AgentConfigAgentPackItem;
	idempotency_key: string;
}

export interface SkillPublicationSuccessorResponse {
	publication: SkillPublicationMetadata;
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}

export interface SkillPublicationUpdateRequest {
	identity: IdentityScope;
	agent_id: string;
	publication_id: string;
	revision_id: string;
	expected_generation: number;
	expected_content_hash: string;
	idempotency_key: string;
}

export interface SkillPublicationUpdateResponse {
	reference: SkillPublicationReference;
	receipt: SkillPublicationReceipt;
	protocol_version: string;
}
