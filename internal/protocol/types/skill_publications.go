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

// SkillPublicationReadRequest reads the body of one organization
// publication revision. An admin caller may name any revision of any
// publication in the tenant; any other verified caller may read only an
// active publication's current revision (exactly what
// `skills.publications.available` lists). RevisionID is optional; when set
// for a non-admin caller it must equal the current revision.
type SkillPublicationReadRequest struct {
	Identity      IdentityScope `json:"identity"`
	PublicationID string        `json:"publication_id"`
	RevisionID    string        `json:"revision_id,omitempty"`
}

// SkillPublicationReadResponse carries the reviewed structured body and its
// canonical SKILL.md rendering. Publications carry no support files.
type SkillPublicationReadResponse struct {
	Publication     SkillPublicationMetadata `json:"publication"`
	RevisionID      string                   `json:"revision_id"`
	Skill           SkillPublicationSkill    `json:"skill"`
	Document        string                   `json:"document"`
	ProtocolVersion string                   `json:"protocol_version"`
}

// SkillPublicationCandidate is the content-free projection of a frozen,
// immutable snapshot of one caller's personal skill submitted for
// organization-admin review. Name, title and description are short listing
// text (the same projection user-skill summaries expose); the body is
// returned only by `skills.publications.candidates.read`.
type SkillPublicationCandidate struct {
	CandidateID         string `json:"candidate_id"`
	Name                string `json:"name"`
	Title               string `json:"title,omitempty"`
	Description         string `json:"description,omitempty"`
	SubmitterUser       string `json:"submitter_user"`
	AgentID             string `json:"agent_id"`
	PackageHash         string `json:"package_hash,omitempty"`
	ContentHash         string `json:"content_hash"`
	TargetPublicationID string `json:"target_publication_id,omitempty"`
	Note                string `json:"note,omitempty"`
	State               string `json:"state"`
	DecisionNote        string `json:"decision_note,omitempty"`
	DecidedBy           string `json:"decided_by,omitempty"`
	PublicationID       string `json:"publication_id,omitempty"`
	RevisionID          string `json:"revision_id,omitempty"`
	PriorContentHash    string `json:"prior_content_hash,omitempty"`
	Generation          uint64 `json:"generation"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
	RuntimeID           string `json:"runtime_id"`
}

// SkillPublicationCandidateSubmitRequest freezes one of the caller's own
// personal skills on an Agent in the caller's signed reach into a pending
// candidate. A skill with support files is refused.
type SkillPublicationCandidateSubmitRequest struct {
	Identity            IdentityScope `json:"identity"`
	AgentID             string        `json:"agent_id"`
	Name                string        `json:"name"`
	TargetPublicationID string        `json:"target_publication_id,omitempty"`
	Note                string        `json:"note,omitempty"`
	IdempotencyKey      string        `json:"idempotency_key"`
}

// SkillPublicationCandidateSubmitResponse returns the pending candidate.
type SkillPublicationCandidateSubmitResponse struct {
	Candidate       SkillPublicationCandidate `json:"candidate"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// SkillPublicationCandidateListRequest lists candidates. An admin caller
// sees every candidate in the tenant; any other caller only their own.
type SkillPublicationCandidateListRequest struct {
	Identity IdentityScope `json:"identity"`
	State    string        `json:"state,omitempty"`
}

// SkillPublicationCandidateListResponse lists candidates newest first.
type SkillPublicationCandidateListResponse struct {
	Candidates      []SkillPublicationCandidate `json:"candidates"`
	ProtocolVersion string                      `json:"protocol_version"`
}

// SkillPublicationCandidateReadRequest reads one candidate's frozen body.
// Only an admin or the submitter may read it.
type SkillPublicationCandidateReadRequest struct {
	Identity    IdentityScope `json:"identity"`
	CandidateID string        `json:"candidate_id"`
}

// SkillPublicationCandidateReadResponse carries the frozen body and its
// canonical SKILL.md rendering.
type SkillPublicationCandidateReadResponse struct {
	Candidate       SkillPublicationCandidate `json:"candidate"`
	Skill           SkillPublicationSkill     `json:"skill"`
	Document        string                    `json:"document"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// SkillPublicationCandidateAcceptRequest publishes a pending candidate as a
// new publication (mode "new") or as an immutable successor revision of an
// existing publication (mode "successor"). Admin only.
type SkillPublicationCandidateAcceptRequest struct {
	Identity                       IdentityScope `json:"identity"`
	CandidateID                    string        `json:"candidate_id"`
	ExpectedGeneration             uint64        `json:"expected_generation"`
	Mode                           string        `json:"mode"`
	Name                           string        `json:"name,omitempty"`
	PublicationID                  string        `json:"publication_id,omitempty"`
	ExpectedPublicationGeneration  uint64        `json:"expected_publication_generation,omitempty"`
	ExpectedPublicationContentHash string        `json:"expected_publication_content_hash,omitempty"`
	DecisionNote                   string        `json:"decision_note,omitempty"`
	IdempotencyKey                 string        `json:"idempotency_key"`
}

// SkillPublicationCandidateAcceptResponse returns the accepted candidate and
// the publication it produced.
type SkillPublicationCandidateAcceptResponse struct {
	Candidate       SkillPublicationCandidate `json:"candidate"`
	Publication     SkillPublicationMetadata  `json:"publication"`
	Receipt         SkillPublicationReceipt   `json:"receipt"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// SkillPublicationCandidateRejectRequest rejects a pending candidate. Admin
// only.
type SkillPublicationCandidateRejectRequest struct {
	Identity           IdentityScope `json:"identity"`
	CandidateID        string        `json:"candidate_id"`
	ExpectedGeneration uint64        `json:"expected_generation"`
	DecisionNote       string        `json:"decision_note"`
	IdempotencyKey     string        `json:"idempotency_key"`
}

// SkillPublicationCandidateRejectResponse returns the rejected candidate.
type SkillPublicationCandidateRejectResponse struct {
	Candidate       SkillPublicationCandidate `json:"candidate"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// SkillPublicationCandidateWithdrawRequest lets the submitter withdraw a
// pending candidate.
type SkillPublicationCandidateWithdrawRequest struct {
	Identity           IdentityScope `json:"identity"`
	CandidateID        string        `json:"candidate_id"`
	ExpectedGeneration uint64        `json:"expected_generation"`
	IdempotencyKey     string        `json:"idempotency_key"`
}

// SkillPublicationCandidateWithdrawResponse returns the withdrawn candidate.
type SkillPublicationCandidateWithdrawResponse struct {
	Candidate       SkillPublicationCandidate `json:"candidate"`
	ProtocolVersion string                    `json:"protocol_version"`
}
