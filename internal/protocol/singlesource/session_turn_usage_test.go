package singlesource_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

func TestCanonicalWireTypes_SessionUsageTurnRow(t *testing.T) {
	if got, ok := singlesource.CanonicalWireTypes["SessionUsageTurnRow"]; !ok || got != "types" {
		t.Fatalf("SessionUsageTurnRow canonical home = %q, present %v; want types/true", got, ok)
	}
}
