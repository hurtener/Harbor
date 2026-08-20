package types

// SkillPublicationSkill is the reviewed body accepted by the organization
// publication surface. It intentionally mirrors the bounded agent-pack body;
// authority, scope, and hashes are server-derived and never accepted from the
// wire.
type SkillPublicationSkill = AgentConfigAgentPackItem

// SkillPublicationMetadata is the content-free publication projection. No
// skill body or support bytes are present in this type.
type SkillPublicationMetadata struct {
	PublicationID string `json:"publication_id"`
	RevisionID    string `json:"revision_id"`
	Name          string `json:"name"`
	ContentHash   string `json:"content_hash"`
	State         string `json:"state"`
	Generation    uint64 `json:"generation"`
	RuntimeID     string `json:"runtime_id"`
}

// SkillPublicationReference is the exact pinned reference installed on one
// target Agent. It contains no body.
type SkillPublicationReference struct {
	AgentID       string `json:"agent_id"`
	PublicationID string `json:"publication_id"`
	RevisionID    string `json:"revision_id"`
	ContentHash   string `json:"content_hash"`
	Generation    uint64 `json:"generation"`
	RuntimeID     string `json:"runtime_id"`
	State         string `json:"state"`
}

// SkillPublicationReceipt is a content-free mutation receipt safe to replay.
type SkillPublicationReceipt struct {
	OperationID   string `json:"operation_id"`
	Operation     string `json:"operation"`
	PublicationID string `json:"publication_id,omitempty"`
	RevisionID    string `json:"revision_id,omitempty"`
	Generation    uint64 `json:"generation"`
	State         string `json:"state"`
	BeforeHash    string `json:"before_hash,omitempty"`
	AfterHash     string `json:"after_hash,omitempty"`
}

type SkillPublicationPublishRequest struct {
	Identity       IdentityScope         `json:"identity"`
	Name           string                `json:"name"`
	Skill          SkillPublicationSkill `json:"skill"`
	IdempotencyKey string                `json:"idempotency_key"`
	ExpectedAbsent bool                  `json:"expected_absent"`
}

type SkillPublicationPublishResponse struct {
	Publication     SkillPublicationMetadata `json:"publication"`
	Receipt         SkillPublicationReceipt  `json:"receipt"`
	ProtocolVersion string                   `json:"protocol_version"`
}

type SkillPublicationListRequest struct {
	Identity IdentityScope `json:"identity"`
}
type SkillPublicationListResponse struct {
	Publications    []SkillPublicationMetadata `json:"publications,omitempty"`
	ProtocolVersion string                     `json:"protocol_version"`
}

type SkillPublicationGetRequest struct {
	Identity      IdentityScope `json:"identity"`
	PublicationID string        `json:"publication_id"`
}
type SkillPublicationGetResponse struct {
	Publication     SkillPublicationMetadata `json:"publication"`
	ProtocolVersion string                   `json:"protocol_version"`
}

type SkillPublicationSuccessorRequest struct {
	Identity            IdentityScope         `json:"identity"`
	PublicationID       string                `json:"publication_id"`
	ExpectedGeneration  uint64                `json:"expected_generation"`
	ExpectedContentHash string                `json:"expected_content_hash"`
	Skill               SkillPublicationSkill `json:"skill"`
	IdempotencyKey      string                `json:"idempotency_key"`
}
type SkillPublicationSuccessorResponse struct {
	Publication     SkillPublicationMetadata `json:"publication"`
	Receipt         SkillPublicationReceipt  `json:"receipt"`
	ProtocolVersion string                   `json:"protocol_version"`
}

type SkillPublicationRetireRequest struct {
	Identity            IdentityScope `json:"identity"`
	PublicationID       string        `json:"publication_id"`
	ExpectedGeneration  uint64        `json:"expected_generation"`
	ExpectedContentHash string        `json:"expected_content_hash"`
	IdempotencyKey      string        `json:"idempotency_key"`
}
type SkillPublicationRetireResponse struct {
	Publication     SkillPublicationMetadata `json:"publication"`
	Receipt         SkillPublicationReceipt  `json:"receipt"`
	ProtocolVersion string                   `json:"protocol_version"`
}

type SkillPublicationAvailableRequest struct {
	Identity IdentityScope `json:"identity"`
}
type SkillPublicationAvailableResponse struct {
	Publications    []SkillPublicationMetadata `json:"publications,omitempty"`
	ProtocolVersion string                     `json:"protocol_version"`
}

type SkillPublicationInstallRequest struct {
	Identity       IdentityScope `json:"identity"`
	AgentID        string        `json:"agent_id"`
	PublicationID  string        `json:"publication_id"`
	RevisionID     string        `json:"revision_id"`
	ExpectedAbsent bool          `json:"expected_absent"`
	IdempotencyKey string        `json:"idempotency_key"`
}
type SkillPublicationInstallResponse struct {
	Reference       SkillPublicationReference `json:"reference"`
	Receipt         SkillPublicationReceipt   `json:"receipt"`
	ProtocolVersion string                    `json:"protocol_version"`
}

type SkillPublicationUpdateRequest struct {
	Identity            IdentityScope `json:"identity"`
	AgentID             string        `json:"agent_id"`
	PublicationID       string        `json:"publication_id"`
	RevisionID          string        `json:"revision_id"`
	ExpectedGeneration  uint64        `json:"expected_generation"`
	ExpectedContentHash string        `json:"expected_content_hash"`
	IdempotencyKey      string        `json:"idempotency_key"`
}
type SkillPublicationUpdateResponse struct {
	Reference       SkillPublicationReference `json:"reference"`
	Receipt         SkillPublicationReceipt   `json:"receipt"`
	ProtocolVersion string                    `json:"protocol_version"`
}

type SkillPublicationRemoveRequest struct {
	Identity            IdentityScope `json:"identity"`
	AgentID             string        `json:"agent_id"`
	ExpectedGeneration  uint64        `json:"expected_generation"`
	ExpectedContentHash string        `json:"expected_content_hash"`
	IdempotencyKey      string        `json:"idempotency_key"`
}
type SkillPublicationRemoveResponse struct {
	Receipt         SkillPublicationReceipt `json:"receipt"`
	ProtocolVersion string                  `json:"protocol_version"`
}

type SkillPublicationReferencesListRequest struct {
	Identity IdentityScope `json:"identity"`
}
type SkillPublicationReferencesListResponse struct {
	References      []SkillPublicationReference `json:"references,omitempty"`
	ProtocolVersion string                      `json:"protocol_version"`
}
