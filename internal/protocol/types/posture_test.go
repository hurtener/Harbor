package types_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestRuntimeInfoRequest_JSONRoundTrip(t *testing.T) {
	in := types.RuntimeInfoRequest{
		Identity: types.IdentityScope{
			Tenant:  "tenant-a",
			User:    "user-1",
			Session: "session-x",
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.RuntimeInfoRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Identity != in.Identity {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestRuntimeInfo_JSONRoundTrip(t *testing.T) {
	in := types.RuntimeInfo{
		InstanceID:      "inst-001",
		DisplayName:     "harbor-dev",
		BuildVersion:    "v0.0.0-dev",
		BuildCommit:     "abc1234",
		BuildDate:       "2026-05-19T00:00:00Z",
		BuildGoVersion:  "go1.26.0",
		ProtocolVersion: types.ProtocolVersion,
		Capabilities:    []types.Capability{types.CapTaskControl, types.CapRuntimePosture},
		UptimeSeconds:   3600,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.RuntimeInfo
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestRuntimeHealth_JSONRoundTrip(t *testing.T) {
	in := types.RuntimeHealth{
		Subsystems: []types.SubsystemHealth{
			{Subsystem: "events", Status: types.HealthStatusReady},
			{Subsystem: "state", Status: types.HealthStatusDegraded, Detail: "slow writes"},
			{Subsystem: "llm", Status: types.HealthStatusUnavailable, Detail: "not registered"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.RuntimeHealth
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestRuntimeCounters_JSONRoundTrip(t *testing.T) {
	in := types.RuntimeCounters{
		EventsPerSecond:       12.5,
		TasksRunning:          3,
		BackgroundJobsActive:  1,
		MCPConnectionsHealthy: 2,
		SessionsActive:        7,
		SnapshotAt:            1_747_000_000_000,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.RuntimeCounters
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestRuntimeDrivers_JSONRoundTrip(t *testing.T) {
	in := types.RuntimeDrivers{
		Subsystems: []types.SubsystemDriver{
			{Subsystem: "state", Driver: "sqlite", Mode: "readwrite"},
			{Subsystem: "artifacts", Driver: "inmem"},
			{Subsystem: "memory", Driver: "postgres", Mode: "readwrite"},
			{Subsystem: "eventlog", Driver: "inmem", Mode: "embedded"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.RuntimeDrivers
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestMetricsSnapshot_JSONRoundTrip(t *testing.T) {
	in := types.MetricsSnapshot{
		Counters: []types.NamedCounter{
			{Name: "harbor_events_total", Value: 42, Labels: map[string]string{"event_type": "task.spawned"}},
		},
		Histograms: []types.NamedHistogram{
			{
				Name:  "harbor_tool_latency_seconds",
				Count: 10,
				Sum:   3.5,
				Buckets: []types.HistogramBucket{
					{UpperBound: 0.1, Count: 4},
					{UpperBound: 1.0, Count: 10},
				},
			},
		},
		Gauges: []types.NamedGauge{
			{Name: "harbor_sessions_active", Value: 7},
		},
		SnapshotAt: 1_747_000_000_000,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.MetricsSnapshot
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// TestPostureWireTypes_NoOTelLeak is a defence-in-depth assertion that
// the posture wire types are plain JSON-serialisable structs — a
// MetricsSnapshot carrying an OpenTelemetry SDK type would not
// round-trip through encoding/json cleanly. The static smoke guard
// pins the import-graph side; this pins the wire shape.
func TestPostureWireTypes_NoOTelLeak(t *testing.T) {
	snap := types.MetricsSnapshot{
		Counters:   []types.NamedCounter{{Name: "c", Value: 1}},
		Histograms: []types.NamedHistogram{},
		Gauges:     []types.NamedGauge{},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("MetricsSnapshot is not cleanly JSON-marshalable — an OTel SDK type may have leaked: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("MetricsSnapshot marshalled to empty bytes")
	}
}
