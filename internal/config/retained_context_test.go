package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestSessionsRetainedContext_ExplicitBound(t *testing.T) {
	t.Parallel()
	if config.Defaults().Sessions.RetainedContextTurns != 0 {
		t.Fatal("default enables new retention")
	}
	for _, n := range []int{-1, 0, 1, 32, 33} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := defaultsForCore()
			c.Sessions.RetainedContextTurns = n
			err := c.ValidateCore()
			if n < 0 || n > config.MaxRetainedContextTurns {
				if err == nil || !strings.Contains(err.Error(), "sessions.retained_context_turns") {
					t.Fatalf("bad bound: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
