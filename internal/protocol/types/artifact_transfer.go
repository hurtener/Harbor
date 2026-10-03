package types

import "time"

// ArtifactTransferEndpoint binds one runtime audience and exact storage owner.
// Epoch is the boot-controlled transfer-policy generation, not a credential.
type ArtifactTransferEndpoint struct {
	Audience string `json:"audience"`
	Tenant   string `json:"tenant"`
	User     string `json:"user"`
	Session  string `json:"session"`
	Epoch    uint64 `json:"epoch"`
}

// ArtifactTransferGrant authorizes one immutable artifact copy to one recipient.
// It carries no URL, credential, or file content. Both runtimes verify the signed
// envelope independently; destination admission additionally requires its owner.
type ArtifactTransferGrant struct {
	Version     int                      `json:"version"`
	KeyID       string                   `json:"key_id"`
	TransferID  string                   `json:"transfer_id"`
	Purpose     string                   `json:"purpose"`
	Source      ArtifactTransferEndpoint `json:"source"`
	Destination ArtifactTransferEndpoint `json:"destination"`
	ArtifactID  string                   `json:"artifact_id"`
	SHA256      string                   `json:"sha256"`
	MimeType    string                   `json:"mime_type"`
	SizeBytes   int64                    `json:"size_bytes"`
	IssuedAt    time.Time                `json:"issued_at"`
	ExpiresAt   time.Time                `json:"expires_at"`
	Signature   string                   `json:"signature"`
}

// ArtifactTransferReceipt is the durable, content-free result of a transfer.
// SHA256 is the immutable content version at both ends; artifact IDs are scoped
// locators. An importing state is unresolved, never evidence of completed copy.
type ArtifactTransferReceipt struct {
	TransferID            string                   `json:"transfer_id"`
	GrantSHA256           string                   `json:"grant_sha256"`
	State                 string                   `json:"state"`
	Source                ArtifactTransferEndpoint `json:"source"`
	Destination           ArtifactTransferEndpoint `json:"destination"`
	SourceArtifactID      string                   `json:"source_artifact_id"`
	DestinationArtifactID string                   `json:"destination_artifact_id,omitempty"`
	SHA256                string                   `json:"sha256"`
	MimeType              string                   `json:"mime_type"`
	SizeBytes             int64                    `json:"size_bytes"`
	ExpiresAt             time.Time                `json:"expires_at"`
}

// ArtifactsTransferRequest is used to admit an import or dispatch an export.
type ArtifactsTransferRequest struct {
	Scope ArtifactScope         `json:"scope"`
	Grant ArtifactTransferGrant `json:"grant"`
}

// ArtifactsTransferStatusRequest selects an owner's export or import receipt.
type ArtifactsTransferStatusRequest struct {
	Scope      ArtifactScope `json:"scope"`
	TransferID string        `json:"transfer_id"`
	Direction  string        `json:"direction"`
}

// ArtifactsExportAnswerRequest selects only one exact sealed final answer.
// The digest is over the UTF-8 answer bytes, never a transcript or JSON wrapper.
type ArtifactsExportAnswerRequest struct {
	Scope          ArtifactScope `json:"scope"`
	RequestID      string        `json:"request_id"`
	TaskID         string        `json:"task_id"`
	TurnID         string        `json:"turn_id"`
	TurnVersion    int64         `json:"turn_version"`
	AnswerSequence int64         `json:"answer_sequence"`
	SHA256         string        `json:"sha256"`
	SizeBytes      int64         `json:"size_bytes"`
}

// ArtifactsExportAnswerResponse binds an immutable source artifact to its exact
// sealed answer and consumed input revision. It contains no answer bytes.
type ArtifactsExportAnswerResponse struct {
	RequestID                 string `json:"request_id"`
	ArtifactID                string `json:"artifact_id"`
	SHA256                    string `json:"sha256"`
	MimeType                  string `json:"mime_type"`
	SizeBytes                 int64  `json:"size_bytes"`
	TaskID                    string `json:"task_id"`
	TurnID                    string `json:"turn_id"`
	TurnVersion               int64  `json:"turn_version"`
	AnswerSequence            int64  `json:"answer_sequence"`
	IncorporatedInputRevision uint64 `json:"incorporated_input_revision"`
}
