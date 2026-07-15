package main

import (
	"errors"
	"strings"
	"testing"
)

func TestTUICmd_HelpStatesAvailabilityHonestlyAndHasNoAttachFlag(t *testing.T) {
	stdout, stderr, err := runRoot(t, []string{"tui", "--help"})
	if err != nil || stderr != "" {
		t.Fatalf("help err=%v stderr=%q", err, stderr)
	}
	for _, text := range []string{"not available in this release", "does not connect to a Runtime", "availability"} {
		if !strings.Contains(stdout, text) {
			t.Errorf("help missing %q:\n%s", text, stdout)
		}
	}
	root := NewRootCmd()
	command, _, findErr := root.Find([]string{"tui"})
	if findErr != nil || command.Flags().Lookup("attach") != nil {
		t.Fatalf("operational attach flag exposed: %v", findErr)
	}
}

func TestTUICmd_ExecutionFailsLoudlyWithoutLaunching(t *testing.T) {
	_, stderr, err := runRoot(t, []string{"tui"})
	var cli CLIError
	if err == nil || !errors.As(err, &cli) || cli.Code != CodeNotImplemented || !strings.Contains(stderr, "not available yet") {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
}
