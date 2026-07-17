package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// BenchmarkLayoutTranscript400 guards the layout cache: a 400-block session
// with markdown must lay out in well under a millisecond per frame, or
// scrolling long conversations turns to sludge (uncached this measured ~20ms).
func BenchmarkLayoutTranscript400(b *testing.B) {
	now := time.Now()
	blocks := []projection.Block{}
	for i := range 200 {
		blocks = append(blocks,
			projection.Block{ID: fmt.Sprintf("user:%d", i), Kind: "user", Text: fmt.Sprintf("question %d about a fairly long topic with details", i), At: now},
			projection.Block{ID: fmt.Sprintf("text:%d", i), Kind: "text", Text: fmt.Sprintf("Answer %d with **bold** and `code`.\n\n- bullet one\n- bullet two\n\nA second paragraph with more prose to wrap across lines.", i), At: now},
		)
	}
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	m := NewOperationalModel(120, 40, ui.NewTheme(ui.ModeDark, ui.ProfileTrueColor), true, projection.Projection{Identity: id, Blocks: blocks})
	m.state.Route = "session"
	b.ResetTimer()
	for b.Loop() {
		_, _, _, _ = m.layoutTranscript(110)
	}
}
