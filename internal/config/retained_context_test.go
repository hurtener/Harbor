package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestMemoryRecentTurns_BoundAndResolution(t *testing.T) {
	t.Parallel()
	if got := (config.MemoryConfig{Strategy: "rolling_summary"}).RecentTurnsResolved(); got != 20 {
		t.Fatalf("zero recent_turns resolved to %d, want 20", got)
	}
	if got := (config.MemoryConfig{Strategy: "none", RecentTurns: 20}).RecentTurnsResolved(); got != 0 {
		t.Fatalf("none resolved to %d, want disabled", got)
	}
	for _, n := range []int{-1, 0, 1, 32, 33} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := defaultsForCore()
			c.Memory.Strategy, c.Memory.RecentTurns = "rolling_summary", n
			err := c.ValidateCore()
			if n < 0 || n > config.MaxMemoryRecentTurns {
				if err == nil || !strings.Contains(err.Error(), "memory.recent_turns") {
					t.Fatalf("bad bound: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
