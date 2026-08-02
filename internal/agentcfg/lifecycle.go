package agentcfg

import (
	"bytes"
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
	if err := rejectDuplicateLifecycleFields(data); err != nil {
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
// Optional discovery fields are the resumable Phase 233a keyset checkpoint;
// every other unknown or malformed member fails closed.
func validRetirementEnvelope(data []byte) bool {
	if len(data) == 0 || string(data) == "null" || rejectDuplicateLifecycleFields(data) != nil {
		return false
	}
	var record struct {
		OperationID      *string                      `json:"operation_id"`
		RetiredAt        *time.Time                   `json:"retired_at"`
		PriorRevisionID  *string                      `json:"prior_revision_id"`
		PriorContentHash *string                      `json:"prior_content_hash"`
		Generation       *uint64                      `json:"generation"`
		Cleanup          []retirementCleanupEnvelope  `json:"cleanup"`
		Completed        *bool                        `json:"completed"`
		PendingEvent     *retirementEventEnvelope     `json:"pending_event"`
		Discovery        *retirementDiscoveryEnvelope `json:"discovery"`
		ManifestFrozen   *bool                        `json:"manifest_frozen"`
	}
	if decodeStrictLifecycleJSON(data, &record) != nil || record.OperationID == nil || record.RetiredAt == nil || record.Generation == nil || record.Completed == nil {
		return false
	}
	if *record.OperationID == "" || *record.OperationID != strings.TrimSpace(*record.OperationID) || len(*record.OperationID) > 128 || record.RetiredAt.IsZero() || *record.Generation == 0 {
		return false
	}
	for _, step := range record.Cleanup {
		if step.Class == nil || step.Resource == nil || step.Completed == nil || *step.Class == "" || *step.Resource == "" {
			return false
		}
	}
	if record.PendingEvent != nil && (record.PendingEvent.Stage == nil || *record.PendingEvent.Stage == "") {
		return false
	}
	if record.Discovery != nil {
		if record.Discovery.Stage == nil || (*record.Discovery.Stage != "personal" && *record.Discovery.Stage != "legacy") || record.Discovery.Continuation == nil {
			return false
		}
	}
	if record.ManifestFrozen != nil && *record.ManifestFrozen && record.Discovery != nil {
		return false
	}
	return true
}

type retirementCleanupEnvelope struct {
	Class     *string `json:"Class"`
	Resource  *string `json:"Resource"`
	Completed *bool   `json:"Completed"`
}

type retirementEventEnvelope struct {
	Stage *string `json:"stage"`
	Class *string `json:"class"`
}

type retirementDiscoveryEnvelope struct {
	Stage        *string `json:"stage"`
	Continuation *string `json:"continuation"`
}

func rejectDuplicateLifecycleFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := first.(json.Delim); !ok || delimiter != '{' {
		return errors.New("expected JSON object")
	}
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("expected JSON object member name")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate JSON object member %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON document")
	}
	return nil
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
