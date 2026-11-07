package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestModelTiming_BoundedMetadataAndUnknownFields(t *testing.T) {
	for _, value := range []string{"https://provider.invalid/?key=private-query", "model?api_key=private-query", "provider\nprivate-query", strings.Repeat("s", 257)} {
		var logs bytes.Buffer
		RecordModelCall(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), ModelCallTiming{Provider: value, Model: value, Role: "private-query", ReasoningEffort: "private-query", Total: time.Millisecond})
		var record map[string]any
		if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["provider"] != "redacted" || record["model"] != "redacted" || record["role"] != "completion" {
			t.Fatalf("record=%v", record)
		}
		for _, key := range []string{"reasoning_effort", "ttft_ms", "input_tokens", "output_tokens"} {
			if _, ok := record[key]; ok {
				t.Errorf("unexpected %s", key)
			}
		}
		if strings.Contains(logs.String(), "private-query") {
			t.Fatal("unsafe metadata leaked")
		}
	}
}
