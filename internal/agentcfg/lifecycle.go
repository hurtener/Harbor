package agentcfg

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// MaxLifecycleRecordBytes bounds the durable lifecycle envelope before it can
// establish authority for either configuration or session-owned state.
const MaxLifecycleRecordBytes = 64 * 1024

// LifecycleRecordState classifies one present lifecycle envelope. Missing is
// intentionally not represented: StateStore.ErrNotFound remains the only
// proof that no lifecycle slot exists.
type LifecycleRecordState uint8

const (
	LifecycleRecordInvalid LifecycleRecordState = iota
	LifecycleRecordActive
	LifecycleRecordTerminal
)

// ClassifyLifecycleRecord strictly decodes the durable agent lifecycle
// envelope. The registry and session-owned resolvers share this authority
// shape: accepting different representations would permit a config write to
// reactivate a record a resolver considers terminal or corrupt.
func ClassifyLifecycleRecord(data []byte) LifecycleRecordState {
	if len(data) == 0 || len(data) > MaxLifecycleRecordBytes {
		return LifecycleRecordInvalid
	}
	if err := ValidateUniqueJSONFields(data); err != nil {
		return LifecycleRecordInvalid
	}
	var envelope struct {
		Schema     *int            `json:"schema"`
		RevisionID *string         `json:"revision_id"`
		UpdatedAt  *time.Time      `json:"updated_at"`
		Retirement json.RawMessage `json:"retirement"`
	}
	if err := decodeStrictLifecycleJSON(data, &envelope); err != nil || envelope.Schema == nil || envelope.RevisionID == nil || envelope.UpdatedAt == nil {
		return LifecycleRecordInvalid
	}
	if (*envelope.Schema != 0 && *envelope.Schema != 1) || envelope.UpdatedAt.IsZero() {
		return LifecycleRecordInvalid
	}
	if *envelope.RevisionID == "" {
		if len(envelope.Retirement) != 0 && !validRetirementEnvelope(envelope.Retirement) {
			return LifecycleRecordInvalid
		}
		return LifecycleRecordTerminal
	}
	if len(envelope.Retirement) != 0 {
		// A record cannot be simultaneously active and retired. Treating this
		// mixed authority as active would let malformed durable state bypass the
		// terminal fence used by session-owned records.
		return LifecycleRecordInvalid
	}
	if *envelope.RevisionID != strings.TrimSpace(*envelope.RevisionID) {
		return LifecycleRecordInvalid
	}
	return LifecycleRecordActive
}

// validRetirementEnvelope mirrors the exact persisted terminal shape without
// importing a concrete registry driver into the shared lifecycle authority.
// Optional discovery fields are the resumable session-record keyset checkpoint;
// every other unknown or malformed member fails closed.
func validRetirementEnvelope(data []byte) bool {
	if len(data) == 0 || string(data) == "null" || ValidateUniqueJSONFields(data) != nil {
		return false
	}
	var record struct {
		OperationID      *string                      `json:"operation_id"`
		RetiredAt        *time.Time                   `json:"retired_at"`
		PriorRevisionID  *string                      `json:"prior_revision_id"`
		PriorContentHash *string                      `json:"prior_content_hash"`
		Generation       *uint64                      `json:"generation"`
		Completed        *bool                        `json:"completed"`
		PendingEvent     *retirementEventEnvelope     `json:"pending_event"`
		Discovery        *retirementDiscoveryEnvelope `json:"discovery"`
		ManifestFrozen   *bool                        `json:"manifest_frozen"`
		ManifestCount    *uint64                      `json:"manifest_count"`
		ManifestDigest   *string                      `json:"manifest_digest"`
		CleanupCompleted *uint64                      `json:"cleanup_completed"`
		CleanupDigest    *string                      `json:"cleanup_digest"`
		ScrubCompleted   *uint64                      `json:"scrub_completed"`
	}
	if decodeStrictLifecycleJSON(data, &record) != nil || record.OperationID == nil || record.RetiredAt == nil || record.Generation == nil || record.Completed == nil || record.ManifestCount == nil || record.ManifestDigest == nil || record.CleanupCompleted == nil || record.CleanupDigest == nil || record.ScrubCompleted == nil {
		return false
	}
	if *record.OperationID == "" || *record.OperationID != strings.TrimSpace(*record.OperationID) || len(*record.OperationID) > 128 || record.RetiredAt.IsZero() || *record.Generation == 0 {
		return false
	}
	if !validLifecycleDigest(*record.ManifestDigest) || !validLifecycleDigest(*record.CleanupDigest) || *record.CleanupCompleted > *record.ManifestCount || *record.ScrubCompleted > *record.CleanupCompleted {
		return false
	}
	if record.Discovery != nil {
		if record.Discovery.Stage == nil || (*record.Discovery.Stage != "config" && *record.Discovery.Stage != "signed_oauth_mcp" && *record.Discovery.Stage != "personal" && *record.Discovery.Stage != "legacy") || record.Discovery.Continuation == nil || record.Discovery.ConfigIndex == nil {
			return false
		}
		if (*record.Discovery.Stage == "config" && *record.Discovery.Continuation != "") || (*record.Discovery.Stage != "config" && *record.Discovery.ConfigIndex != 0) {
			return false
		}
	}
	frozen := record.ManifestFrozen != nil && *record.ManifestFrozen
	allCleanupScrubbed := *record.CleanupCompleted == *record.ManifestCount && *record.ScrubCompleted == *record.ManifestCount
	if frozen == (record.Discovery != nil) ||
		(!frozen && (*record.Completed || *record.CleanupCompleted != 0 || *record.ScrubCompleted != 0)) ||
		(*record.Completed && !allCleanupScrubbed) ||
		(*record.ManifestCount == 0 && *record.ManifestDigest != emptyLifecycleManifestDigest()) ||
		(*record.ManifestCount > 0 && *record.ManifestDigest == emptyLifecycleManifestDigest()) ||
		(*record.CleanupCompleted == 0 && *record.CleanupDigest != emptyLifecycleManifestDigest()) ||
		(*record.CleanupCompleted > 0 && *record.CleanupDigest == emptyLifecycleManifestDigest()) ||
		(*record.Completed && *record.CleanupDigest != *record.ManifestDigest) {
		return false
	}
	if record.PendingEvent != nil && !validRetirementPendingEvent(record.PendingEvent, frozen, *record.Completed) {
		return false
	}
	if frozen && allCleanupScrubbed && !*record.Completed && (record.PendingEvent == nil || record.PendingEvent.Stage == nil || *record.PendingEvent.Stage != "progress") {
		return false
	}
	return true
}

func emptyLifecycleManifestDigest() string {
	return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}

type retirementEventEnvelope struct {
	Stage *string `json:"stage"`
	Class *string `json:"class"`
}

type retirementDiscoveryEnvelope struct {
	Stage        *string `json:"stage"`
	Continuation *string `json:"continuation"`
	ConfigIndex  *uint64 `json:"config_index"`
}

func validRetirementPendingEvent(event *retirementEventEnvelope, frozen, completed bool) bool {
	if event.Stage == nil {
		return false
	}
	class := ""
	if event.Class != nil {
		class = *event.Class
	}
	switch *event.Stage {
	case "started":
		return class == "" && !frozen && !completed
	case "progress":
		return frozen && !completed && validLifecycleCleanupClass(class)
	case "completed":
		return frozen && completed && (class == "" || validLifecycleCleanupClass(class))
	default:
		return false
	}
}

func validLifecycleCleanupClass(class string) bool {
	switch class {
	case "mcp_connection", "oauth_provider", RetirementCleanupClassSignedOAuthMCPPair, "session_personal", "legacy_session_overlay":
		return true
	default:
		return false
	}
}

func validLifecycleDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ValidateUniqueJSONFields rejects duplicate member names recursively in every
// object, including objects nested in arrays. Authority decoders call it
// before typed decoding so encoding/json's last-key-wins behavior is never an
// authorization decision.
func ValidateUniqueJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := validateUniqueJSONValue(decoder, first); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON document")
	}
	return nil
}

func validateUniqueJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("expected JSON object member name")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate JSON object member %q", name)
			}
			seen[name] = struct{}{}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := validateUniqueJSONValue(decoder, value); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := validateUniqueJSONValue(decoder, value); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func decodeStrictLifecycleJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON document")
	}
	return nil
}
