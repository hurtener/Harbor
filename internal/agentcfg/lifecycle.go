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
		Schema     *int       `json:"schema"`
		RevisionID *string    `json:"revision_id"`
		UpdatedAt  *time.Time `json:"updated_at"`
	}
	if err := decodeStrictLifecycleJSON(data, &envelope); err != nil || envelope.Schema == nil || envelope.RevisionID == nil || envelope.UpdatedAt == nil {
		return LifecycleRecordInvalid
	}
	if (*envelope.Schema != 0 && *envelope.Schema != 1) || envelope.UpdatedAt.IsZero() {
		return LifecycleRecordInvalid
	}
	if *envelope.RevisionID == "" {
		return LifecycleRecordTerminal
	}
	if *envelope.RevisionID != strings.TrimSpace(*envelope.RevisionID) {
		return LifecycleRecordInvalid
	}
	return LifecycleRecordActive
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
