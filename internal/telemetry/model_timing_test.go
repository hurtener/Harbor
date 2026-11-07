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

func TestModelTiming_ReportedCounts_PreservesZeroAndOmitsUnknown(t *testing.T) {
	zero, positive, negative := 0, 7, -1
	for _, tc := range []struct {
		name  string
		count *int
		known bool
	}{
		{name: "unknown"},
		{name: "reported zero", count: &zero, known: true},
		{name: "reported positive", count: &positive, known: true},
		{name: "invalid negative", count: &negative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			RecordModelCall(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)), ModelCallTiming{
				InputTokens: tc.count, OutputTokens: tc.count,
			})
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"input_tokens", "output_tokens"} {
				value, present := record[key]
				if present != tc.known || (present && value != float64(*tc.count)) {
					t.Fatalf("%s=%v present=%v, known=%v", key, value, present, tc.known)
				}
			}
		})
	}
}

func TestModelTiming_Outcome_TimeoutTakesPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		call ModelCallTiming
		want string
	}{
		{name: "ok", want: "ok"},
		{name: "error", call: ModelCallTiming{Failed: true}, want: "error"},
		{name: "canceled", call: ModelCallTiming{Failed: true, Canceled: true}, want: "canceled"},
		{name: "timeout", call: ModelCallTiming{Failed: true, TimedOut: true}, want: "timeout"},
		{name: "timeout over cancellation", call: ModelCallTiming{Failed: true, Canceled: true, TimedOut: true}, want: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			RecordModelCall(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)), tc.call)
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["outcome"] != tc.want {
				t.Fatalf("outcome=%v, want %s", record["outcome"], tc.want)
			}
		})
	}
}
