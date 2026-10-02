package stream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/sessions/turns"
)

func TestSessionTurnOutputManifest_WireDistinguishesLegacyAndKnownEmpty(t *testing.T) {
	row := turns.TurnRow{TurnID: "task", TaskID: "task", SessionID: "session", Sealed: true, Status: turns.StatusComplete, OutputManifest: turns.OutputManifestSeal{Version: 1, SHA256: strings.Repeat("a", 64), InputRevision: 9}}
	wire := projectSessionTurnRow(row)
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if wire.OutputManifest.Version != 1 || wire.OutputManifest.SHA256 != row.OutputManifest.SHA256 || wire.OutputManifest.InputRevision != 9 || !strings.Contains(string(raw), `"output_manifest":{"version":1,`) {
		t.Fatalf("lost known empty seal: %s", raw)
	}
	row.OutputManifest = turns.OutputManifestSeal{}
	legacy := projectSessionTurnRow(row)
	if legacy.OutputManifest.Version != 0 || legacy.OutputManifest.SHA256 != "" {
		t.Fatal("legacy invented provenance")
	}
}
