package posture

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestState_AttentionUsesCanonicalHealthAndStream(t *testing.T) {
	state := State{Health: types.RuntimeHealth{Subsystems: []types.SubsystemHealth{{Subsystem: "events", Status: types.HealthStatusDegraded}}}}
	if level, _ := state.Attention("disconnected", 0, 0); level != "error" {
		t.Fatal(level)
	}
	if level, _ := state.Attention("live", 1, 0); level != "warning" {
		t.Fatal(level)
	}
	if level, label := state.Attention("live", 0, 0); level != "warning" || label != "events degraded" {
		t.Fatalf("%s %s", level, label)
	}
	state.Health.Subsystems[0].Status = types.HealthStatusReady
	if level, _ := state.Attention("live", 0, 0); level != "success" {
		t.Fatal(level)
	}
}
