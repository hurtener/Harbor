package agentcfg

import "testing"

func TestClassifyLifecycleRecord_RetirementEnvelopeStrictAndTerminal(t *testing.T) {
	valid := []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":2,"cleanup":[{"Class":"session_personal","Resource":"opaque","Completed":false}],"completed":false,"discovery":{"stage":"legacy","continuation":""}}}`)
	if got := ClassifyLifecycleRecord(valid); got != LifecycleRecordTerminal {
		t.Fatalf("valid retirement classification=%v want terminal", got)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "mixed active and retirement", data: `{"schema":1,"revision_id":"active","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":1,"completed":true}}`},
		{name: "missing completed", data: `{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":1}}`},
		{name: "unknown retirement field", data: `{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":1,"completed":true,"reactivate":true}}`},
		{name: "frozen with discovery", data: `{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z","retirement":{"operation_id":"op","retired_at":"2026-08-02T00:00:00Z","generation":1,"completed":true,"manifest_frozen":true,"discovery":{"stage":"personal","continuation":""}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyLifecycleRecord([]byte(tc.data)); got != LifecycleRecordInvalid {
				t.Fatalf("classification=%v want invalid", got)
			}
		})
	}
}
