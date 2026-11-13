package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

func TestBuildUserContent_SpeakerLabel(t *testing.T) {
	plain := buildUserContent(planner.RunContext{Goal: "hello"})
	if strings.Contains(plain, "Message from") || !strings.HasPrefix(plain, "User goal: hello") {
		t.Fatalf("unlabelled content changed: %q", plain)
	}
	labelled := buildUserContent(planner.RunContext{Goal: "hello", Speaker: "Ana"})
	if !strings.HasPrefix(labelled, "Message from: Ana\nUser goal: hello") {
		t.Fatalf("labelled content = %q", labelled)
	}
}
