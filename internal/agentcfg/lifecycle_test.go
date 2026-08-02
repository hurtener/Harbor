package agentcfg

import (
	"strings"
	"testing"
)

func TestClassifyLifecycleRecord_RetirementEnvelopeStrictAndTerminal(t *testing.T) {
	valid := []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":2,"completed":false,"discovery":{"stage":"legacy","continuation":"","config_index":0},"manifest_count":1,"manifest_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","cleanup_completed":0,"cleanup_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}`)
	if got := ClassifyLifecycleRecord(valid); got != LifecycleRecordTerminal {
		t.Fatalf("valid retirement classification=%v want terminal", got)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "mixed active and retirement", data: strings.Replace(string(valid), "\"revision_id\":\"\"", "\"revision_id\":\"active\"", 1)},
		{name: "missing completed", data: strings.Replace(string(valid), "\"completed\":false,", ",", 1)},
		{name: "unknown retirement field", data: strings.Replace(string(valid), "\"cleanup_completed\":0", "\"cleanup_completed\":0,\"reactivate\":true", 1)},
		{name: "frozen with discovery", data: strings.Replace(string(valid), "\"manifest_count\":1", "\"manifest_frozen\":true,\"manifest_count\":1", 1)},
		{name: "completed before cleanup", data: strings.Replace(string(valid), "\"completed\":false", "\"completed\":true", 1)},
		{name: "unknown pending stage", data: strings.Replace(string(valid), "\"completed\":false,", "\"completed\":false,\"pending_event\":{\"stage\":\"future\"},", 1)},
		{name: "progress without class", data: strings.Replace(string(valid), "\"completed\":false,", "\"completed\":false,\"pending_event\":{\"stage\":\"progress\"},", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyLifecycleRecord([]byte(tc.data)); got != LifecycleRecordInvalid {
				t.Fatalf("classification=%v want invalid", got)
			}
		})
	}
}

func TestValidateUniqueJSONFields_RejectsEveryNestedDuplicate(t *testing.T) {
	for _, data := range []string{
		`{"schema":1,"schema":1}`,
		`{"retirement":{"operation_id":"a","operation_id":"b"}}`,
		`{"retirement":{"pending_event":{"stage":"progress","stage":"completed"}}}`,
		`{"retirement":{"discovery":{"stage":"personal","stage":"legacy"}}}`,
		`{"cleanup":[{"class":"session_personal","class":"oauth_provider"}]}`,
		`{"target":{"identity":{"tenant_id":"a","tenant_id":"b"}}}`,
	} {
		if err := ValidateUniqueJSONFields([]byte(data)); err == nil {
			t.Fatalf("duplicate accepted: %s", data)
		}
	}
	if err := ValidateUniqueJSONFields([]byte(`{"cleanup":[{"class":"session_personal"}],"target":{"identity":{"tenant_id":"a"}}}`)); err != nil {
		t.Fatalf("valid nested JSON rejected: %v", err)
	}
}

func TestClassifyLifecycleRecord_PendingEventAndFrozenInvariants(t *testing.T) {
	const prefix = `{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":4,`
	const suffix = `,"manifest_frozen":true,"manifest_count":1,"manifest_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","cleanup_completed":0,"cleanup_digest":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}`
	validProgress := prefix + `"completed":false,"pending_event":{"stage":"progress","class":"session_personal"}` + suffix
	if got := ClassifyLifecycleRecord([]byte(validProgress)); got != LifecycleRecordTerminal {
		t.Fatalf("valid progress=%v", got)
	}
	for _, data := range []string{
		strings.Replace(validProgress, `"stage":"progress"`, `"stage":"progress","stage":"completed"`, 1),
		strings.Replace(validProgress, `"class":"session_personal"`, `"class":"unknown"`, 1),
		strings.Replace(validProgress, `"stage":"progress","class":"session_personal"`, `"stage":"started","class":"session_personal"`, 1),
		strings.Replace(validProgress, `"completed":false`, `"completed":true`, 1),
		strings.Replace(validProgress, `"cleanup_completed":0`, `"cleanup_completed":2`, 1),
		strings.Replace(validProgress, `"cleanup_completed":0`, `"cleanup_completed":1`, 1),
	} {
		if got := ClassifyLifecycleRecord([]byte(data)); got != LifecycleRecordInvalid {
			t.Fatalf("invalid pending/frozen state classified=%v data=%s", got, data)
		}
	}
}
