package auth

import (
	"context"
	"errors"
	"testing"
)

func TestParseAgentReadSessionApps(t *testing.T) {
	reach := []string{"agent-a"}
	if got, err := ParseAgentReadSessionApps(nil, nil); got || err != nil {
		t.Fatalf("absent = %v, %v", got, err)
	}
	if got, err := ParseAgentReadSessionApps(true, reach); !got || err != nil {
		t.Fatalf("true = %v, %v", got, err)
	}
	if got, err := ParseAgentReadSessionApps(false, nil); got || err != nil {
		t.Fatalf("false = %v, %v", got, err)
	}
	for _, bad := range []struct {
		raw   any
		reach []string
	}{{true, nil}, {"true", reach}, {1.0, reach}, {[]any{true}, reach}} {
		if _, err := ParseAgentReadSessionApps(bad.raw, bad.reach); !errors.Is(err, ErrAgentReadReachMalformed) {
			t.Fatalf("Parse(%v, %v) err = %v", bad.raw, bad.reach, err)
		}
	}
	if AgentReadSessionAppsAllowed(context.Background()) {
		t.Fatal("absent opt-in reads as allowed")
	}
	if !AgentReadSessionAppsAllowed(WithAgentReadSessionApps(context.Background(), true)) {
		t.Fatal("seated opt-in not visible")
	}
}
