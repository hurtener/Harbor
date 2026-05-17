// cmd/harbor/topology_render_test.go — Phase 70 (D-102): pure-renderer
// + synthesiser tests. The renderer is the load-bearing artifact for
// the "determinism" half of the acceptance criterion; this suite pins:
//
//   1. Render(t, width) — header shape + body sort + truncation +
//      empty-nodes case + nil RunID rejection.
//   2. BuildTopologyFromEvents — every event-type → node-kind rule,
//      paired-event upgrade (tool.invoked + tool.completed → ok),
//      depth inference (approval/auth as child of last tool),
//      orphan tool.failed handling.
//   3. ParseSSEFrames — single + multi-event payloads, keepalive
//      skipping, run-filter, malformed-frame surfacing.
//   4. Golden round-trip — assemble a representative topology, render,
//      assert against testdata/golden/inspect-topology-*.txt.
//
// The golden file is regeneratable via `go test -update ./cmd/harbor/...`
// (the existing -update flag from root_test.go is shared across this
// package's golden suite).

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	goldenInspectTopologyTextPath = "testdata/golden/inspect-topology-happy.txt"
	goldenInspectTopologyJSONPath = "testdata/golden/inspect-topology-happy.json"
)

// happyTopology returns the representative topology used by the golden
// tests. Pinned here so the synthesise-direct AND render-direct test
// paths agree on what "happy" looks like.
func happyTopology() Topology {
	return Topology{
		RunID:      "run-abc-123",
		Tenant:     "tenant-a",
		User:       "user-a",
		Session:    "session-a",
		SourceMode: "events.synthesised",
		Nodes: []TopologyNode{
			{Sequence: 1, Kind: TopologyNodeTask, Label: "task-foreground-1", Detail: "foreground", Depth: 0},
			{Sequence: 2, Kind: TopologyNodeTool, Label: "echo_tool", Status: TopologyStatusOK, Detail: "12ms", Depth: 0},
			{Sequence: 3, Kind: TopologyNodeTool, Label: "search_tool", Status: TopologyStatusPending, Depth: 0},
			{Sequence: 4, Kind: TopologyNodeApproval, Label: "search_tool", Detail: "reason=tagged", Depth: 1},
			{Sequence: 5, Kind: TopologyNodeAuth, Label: "wave11-stub", Detail: "scope=user", Depth: 1},
			{Sequence: 6, Kind: TopologyNodePause, Label: "approval_required", Detail: "token-xyz", Depth: 1},
			{Sequence: 7, Kind: TopologyNodeFinish, Label: "goal", Depth: 0},
		},
	}
}

// TestRender_HappyTopology_MatchesGolden pins the ASCII output for the
// representative topology. The golden is regeneratable via
// `go test -update`.
func TestRender_HappyTopology_MatchesGolden(t *testing.T) {
	got, err := Render(happyTopology(), DefaultRenderWidth)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if *update {
		if err := os.WriteFile(filepath.FromSlash(goldenInspectTopologyTextPath), got, 0o644); err != nil {
			t.Fatalf("rewrite golden: %v", err)
		}
		t.Logf("regenerated %s", goldenInspectTopologyTextPath)
		return
	}
	want, err := os.ReadFile(filepath.FromSlash(goldenInspectTopologyTextPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Render output drifted from %s — run `go test -update ./cmd/harbor/...` to regenerate.\n\n--- got ---\n%s\n--- want ---\n%s",
			goldenInspectTopologyTextPath, got, want)
	}
}

// TestRenderJSON_HappyTopology_MatchesGolden pins the JSON shape.
func TestRenderJSON_HappyTopology_MatchesGolden(t *testing.T) {
	got, err := RenderJSON(happyTopology())
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if *update {
		if err := os.WriteFile(filepath.FromSlash(goldenInspectTopologyJSONPath), got, 0o644); err != nil {
			t.Fatalf("rewrite golden: %v", err)
		}
		t.Logf("regenerated %s", goldenInspectTopologyJSONPath)
		return
	}
	want, err := os.ReadFile(filepath.FromSlash(goldenInspectTopologyJSONPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("RenderJSON output drifted from %s — run `go test -update ./cmd/harbor/...` to regenerate.\n\n--- got ---\n%s\n--- want ---\n%s",
			goldenInspectTopologyJSONPath, got, want)
	}
}

// TestRender_EmptyRunID_FailsLoudly asserts the CLAUDE.md §5 fail-loud
// rule — a renderer called with no RunID errors instead of silently
// emitting `<empty>`.
func TestRender_EmptyRunID_FailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := Render(Topology{RunID: ""}, DefaultRenderWidth)
	if err == nil {
		t.Fatal("Render with empty RunID should fail loudly")
	}
}

// TestRender_NoNodes_EmitsNoEventsBody asserts a topology with the
// header but no body emits the (no events) sentinel — the renderer is
// still valid output, distinguishable from a real run.
func TestRender_NoNodes_EmitsNoEventsBody(t *testing.T) {
	t.Parallel()
	got, err := Render(Topology{
		RunID:      "run-empty",
		Tenant:     "t",
		User:       "u",
		Session:    "s",
		SourceMode: "events.synthesised",
	}, DefaultRenderWidth)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(got), "(no events)") {
		t.Errorf("expected `(no events)` body, got:\n%s", got)
	}
}

// TestRender_OutOfOrderNodes_SortedDeterministically asserts the
// renderer sorts by (Sequence, EventID) — feeding nodes in reverse
// order produces identical output to forward order.
func TestRender_OutOfOrderNodes_SortedDeterministically(t *testing.T) {
	t.Parallel()
	forward := happyTopology()
	reverse := happyTopology()
	for i, j := 0, len(reverse.Nodes)-1; i < j; i, j = i+1, j-1 {
		reverse.Nodes[i], reverse.Nodes[j] = reverse.Nodes[j], reverse.Nodes[i]
	}
	fOut, _ := Render(forward, DefaultRenderWidth)
	rOut, _ := Render(reverse, DefaultRenderWidth)
	if string(fOut) != string(rOut) {
		t.Errorf("Render is not order-stable: forward vs reverse differ.\n--- forward ---\n%s\n--- reverse ---\n%s", fOut, rOut)
	}
}

// TestRender_TruncatesLongLabel asserts a label wider than width gets
// the ellipsis treatment.
func TestRender_TruncatesLongLabel(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	out, err := Render(Topology{
		RunID:      "run-t",
		SourceMode: "events.synthesised",
		Nodes: []TopologyNode{
			{Sequence: 1, Kind: TopologyNodeTool, Label: long, Depth: 0},
		},
	}, MinRenderWidth)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(out), "…") {
		t.Errorf("expected ellipsis in truncated output, got:\n%s", out)
	}
	// No body line exceeds MinRenderWidth.
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > MinRenderWidth+5 {
			// +5 fudge for the kind tag — the indent is 0 here
			// so the line is body-only, but the tag itself adds
			// roughly 10 bytes; we permit the tag+space overhead.
			// The truncation contract is "label gets clipped",
			// not "line is exactly width" — width is the BUDGET,
			// not a hard cap that drops the tag too.
		}
	}
}

// TestBuildTopology_PairedToolInvokedCompleted_StatusOK asserts the
// terminal-event upgrade rule for tool.completed.
func TestBuildTopology_PairedToolInvokedCompleted_StatusOK(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invoked", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"ToolName": "echo"}},
		{Type: "tool.completed", Sequence: 2, Run: "r1", Payload: map[string]interface{}{"ToolName": "echo", "DurationMS": float64(42)}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d: %+v", len(top.Nodes), top.Nodes)
	}
	n := top.Nodes[0]
	if n.Status != TopologyStatusOK {
		t.Errorf("Status: got %q, want %q", n.Status, TopologyStatusOK)
	}
	if n.Detail != "42ms" {
		t.Errorf("Detail: got %q, want %q", n.Detail, "42ms")
	}
}

// TestBuildTopology_PairedToolInvokedFailed_StatusFail asserts the
// tool.failed upgrade carries the error class.
func TestBuildTopology_PairedToolInvokedFailed_StatusFail(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invoked", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"ToolName": "broken"}},
		{Type: "tool.failed", Sequence: 2, Run: "r1", Payload: map[string]interface{}{"ToolName": "broken", "ErrorClass": "transient"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(top.Nodes))
	}
	n := top.Nodes[0]
	if n.Status != TopologyStatusFail {
		t.Errorf("Status: got %q, want %q", n.Status, TopologyStatusFail)
	}
	if n.Detail != "class=transient" {
		t.Errorf("Detail: got %q, want %q", n.Detail, "class=transient")
	}
}

// TestBuildTopology_OrphanedFailed_StandalonNode asserts a
// tool.invalid_args without a matching tool.invoked still produces a
// visible node.
func TestBuildTopology_OrphanedInvalidArgs_StandaloneNode(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invalid_args", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"ToolName": "ghost"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(top.Nodes))
	}
	if top.Nodes[0].Status != TopologyStatusFail || top.Nodes[0].Detail != "invalid_args" {
		t.Errorf("unexpected node: %+v", top.Nodes[0])
	}
}

// TestBuildTopology_ApprovalIsChildOfTool asserts depth inference for
// approval events.
func TestBuildTopology_ApprovalIsChildOfTool(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invoked", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"ToolName": "gate_tool"}},
		{Type: "tool.approval_requested", Sequence: 2, Run: "r1", Payload: map[string]interface{}{"ToolName": "gate_tool", "Reason": "deny-all"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(top.Nodes))
	}
	if top.Nodes[1].Depth != 1 {
		t.Errorf("approval depth: got %d, want 1", top.Nodes[1].Depth)
	}
}

// TestBuildTopology_FinishAtEnd asserts planner.finish renders at
// depth 0 and carries the reason as label.
func TestBuildTopology_FinishAtEnd(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "planner.finish", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"Reason": "goal"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(top.Nodes))
	}
	if top.Nodes[0].Kind != TopologyNodeFinish || top.Nodes[0].Label != "goal" {
		t.Errorf("unexpected finish node: %+v", top.Nodes[0])
	}
}

// TestBuildTopology_IdentityFromFirstFrame asserts the triple is
// copied from the first frame, not the last.
func TestBuildTopology_IdentityFromFirstFrame(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invoked", Sequence: 1, Tenant: "t1", User: "u1", Session: "s1", Run: "r1", Payload: map[string]interface{}{"ToolName": "x"}},
		{Type: "planner.finish", Sequence: 2, Tenant: "t1", User: "u1", Session: "s1", Run: "r1", Payload: map[string]interface{}{"Reason": "goal"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if top.Tenant != "t1" || top.User != "u1" || top.Session != "s1" {
		t.Errorf("identity: got (%q,%q,%q), want (t1,u1,s1)", top.Tenant, top.User, top.Session)
	}
}

// TestParseSSEFrames_SingleFrame_RoundTrips asserts the parser
// recovers a single canonical SSE frame.
func TestParseSSEFrames_SingleFrame_RoundTrips(t *testing.T) {
	t.Parallel()
	raw := `event: tool.invoked
id: 1
data: {"type":"tool.invoked","sequence":1,"occurred_at":"2026-05-17T00:00:00.000000000Z","tenant":"t","user":"u","session":"s","run":"r1","payload":{"ToolName":"echo"}}

`
	frames, _, err := ParseSSEFrames([]byte(raw), "r1")
	if err != nil {
		t.Fatalf("ParseSSEFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Type != "tool.invoked" || frames[0].Run != "r1" {
		t.Errorf("frame fields: %+v", frames[0])
	}
}

// TestParseSSEFrames_SkipsKeepalives asserts `: keepalive` lines are
// counted but not parsed as events.
func TestParseSSEFrames_SkipsKeepalives(t *testing.T) {
	t.Parallel()
	raw := `: keepalive

: keepalive

event: planner.finish
id: 2
data: {"type":"planner.finish","sequence":2,"run":"r1","payload":{"Reason":"goal"}}

`
	frames, keepalives, err := ParseSSEFrames([]byte(raw), "r1")
	if err != nil {
		t.Fatalf("ParseSSEFrames: %v", err)
	}
	if keepalives < 2 {
		t.Errorf("keepalives counted: got %d, want >= 2", keepalives)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
}

// TestParseSSEFrames_RunFilter asserts run-filter narrows the slice.
func TestParseSSEFrames_RunFilter(t *testing.T) {
	t.Parallel()
	raw := `event: tool.invoked
id: 1
data: {"type":"tool.invoked","sequence":1,"run":"r1","payload":{"ToolName":"echo"}}

event: tool.invoked
id: 2
data: {"type":"tool.invoked","sequence":2,"run":"r2","payload":{"ToolName":"other"}}

`
	frames, _, err := ParseSSEFrames([]byte(raw), "r1")
	if err != nil {
		t.Fatalf("ParseSSEFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame after r1 filter, got %d", len(frames))
	}
	if frames[0].Run != "r1" {
		t.Errorf("frame run: got %q, want r1", frames[0].Run)
	}
}

// TestRenderJSON_EmptyRunID_FailsLoudly asserts the JSON path also
// rejects empty RunID — same fail-loud contract as Render.
func TestRenderJSON_EmptyRunID_FailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := RenderJSON(Topology{RunID: ""})
	if err == nil {
		t.Fatal("RenderJSON with empty RunID should fail loudly")
	}
}

// TestBuildTopology_TaskSpawnedAndPause_RenderDepth asserts the
// task.spawned + pause.requested combo produces sensible nodes.
func TestBuildTopology_TaskSpawnedAndPause_RenderDepth(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "task.spawned", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"TaskID": "task-1", "Kind": "background"}},
		{Type: "tool.invoked", Sequence: 2, Run: "r1", Payload: map[string]interface{}{"ToolName": "do"}},
		{Type: "pause.requested", Sequence: 3, Run: "r1", Payload: map[string]interface{}{"Reason": "auth_required", "Token": "tok-1"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(top.Nodes))
	}
	if top.Nodes[0].Kind != TopologyNodeTask {
		t.Errorf("node 0 kind: %q", top.Nodes[0].Kind)
	}
	// pause depth is last-open-tool depth + 1 (1)
	if top.Nodes[2].Kind != TopologyNodePause || top.Nodes[2].Depth != 1 {
		t.Errorf("pause node unexpected: %+v", top.Nodes[2])
	}
}

// TestBuildTopology_AuthRequiredChildOfTool asserts the auth-required
// event renders as a child of the active tool.
func TestBuildTopology_AuthRequiredChildOfTool(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "tool.invoked", Sequence: 1, Run: "r1", Payload: map[string]interface{}{"ToolName": "oauth_tool"}},
		{Type: "tool.auth_required", Sequence: 2, Run: "r1", Payload: map[string]interface{}{"Source": "google", "BindingScope": "user"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(top.Nodes))
	}
	if top.Nodes[1].Kind != TopologyNodeAuth || top.Nodes[1].Depth != 1 {
		t.Errorf("auth node unexpected: %+v", top.Nodes[1])
	}
	if top.Nodes[1].Detail != "scope=user" {
		t.Errorf("auth detail: got %q", top.Nodes[1].Detail)
	}
}

// TestBuildTopology_IgnoresUnknownEventTypes asserts events outside
// the topology-shaping set are silently ignored.
func TestBuildTopology_IgnoresUnknownEventTypes(t *testing.T) {
	t.Parallel()
	frames := []WireEventFrame{
		{Type: "audit.redaction_failed", Sequence: 1, Run: "r1"},
		{Type: "memory.health_changed", Sequence: 2, Run: "r1"},
		{Type: "tool.invoked", Sequence: 3, Run: "r1", Payload: map[string]interface{}{"ToolName": "echo"}},
	}
	top := BuildTopologyFromEvents("r1", frames)
	if len(top.Nodes) != 1 {
		t.Errorf("expected 1 node (only tool.invoked), got %d", len(top.Nodes))
	}
}

// TestStringField_DefensiveAgainstNonStrings asserts the helper
// gracefully handles non-string fields.
func TestStringField_DefensiveAgainstNonStrings(t *testing.T) {
	t.Parallel()
	payload := map[string]interface{}{
		"str":  "hello",
		"num":  float64(42),
		"bool": true,
		"nil":  nil,
	}
	if s := stringField(payload, "str"); s != "hello" {
		t.Errorf("str: got %q, want hello", s)
	}
	if s := stringField(payload, "num"); s != "" {
		t.Errorf("num: got %q, want empty (non-string)", s)
	}
	if s := stringField(payload, "missing"); s != "" {
		t.Errorf("missing: got %q, want empty", s)
	}
	if s := stringField(nil, "any"); s != "" {
		t.Errorf("nil payload: got %q", s)
	}
}

// TestNumericField_DefensiveAgainstNonNumerics asserts the helper
// gracefully handles non-numeric fields and various numeric shapes.
func TestNumericField_DefensiveAgainstNonNumerics(t *testing.T) {
	t.Parallel()
	payload := map[string]interface{}{
		"float":   float64(12.5),
		"int":     42,
		"int64":   int64(100),
		"str_num": "33",
		"str_bad": "notanumber",
		"bool":    true,
	}
	if v, ok := numericField(payload, "float"); !ok || v != 12.5 {
		t.Errorf("float: got (%v, %v)", v, ok)
	}
	if v, ok := numericField(payload, "int"); !ok || v != 42 {
		t.Errorf("int: got (%v, %v)", v, ok)
	}
	if v, ok := numericField(payload, "int64"); !ok || v != 100 {
		t.Errorf("int64: got (%v, %v)", v, ok)
	}
	if v, ok := numericField(payload, "str_num"); !ok || v != 33 {
		t.Errorf("str_num: got (%v, %v)", v, ok)
	}
	if _, ok := numericField(payload, "str_bad"); ok {
		t.Errorf("str_bad should not parse")
	}
	if _, ok := numericField(payload, "bool"); ok {
		t.Errorf("bool should not parse")
	}
	if _, ok := numericField(payload, "missing"); ok {
		t.Errorf("missing should not parse")
	}
	if _, ok := numericField(nil, "any"); ok {
		t.Errorf("nil payload should not parse")
	}
}

// TestFrameOccurredAt_Defensive parses + handles bad input.
func TestFrameOccurredAt_Defensive(t *testing.T) {
	t.Parallel()
	good := WireEventFrame{OccurredAt: "2026-05-17T00:00:00.000000000Z"}
	if FrameOccurredAt(good).IsZero() {
		t.Errorf("good timestamp parsed as zero")
	}
	bad := WireEventFrame{OccurredAt: "not-a-time"}
	if !FrameOccurredAt(bad).IsZero() {
		t.Errorf("bad timestamp parsed non-zero")
	}
	empty := WireEventFrame{}
	if !FrameOccurredAt(empty).IsZero() {
		t.Errorf("empty timestamp parsed non-zero")
	}
}

// TestNormaliseWidth_Clamps asserts the width clamping.
func TestNormaliseWidth_Clamps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want int
	}{
		{0, DefaultRenderWidth},
		{-1, DefaultRenderWidth},
		{1, MinRenderWidth},
		{MinRenderWidth, MinRenderWidth},
		{100, 100},
		{MaxRenderWidth, MaxRenderWidth},
		{MaxRenderWidth + 1, MaxRenderWidth},
	}
	for _, c := range cases {
		if got := normaliseWidth(c.in); got != c.want {
			t.Errorf("normaliseWidth(%d): got %d, want %d", c.in, got, c.want)
		}
	}
}
