package tasks

import (
	"errors"
	"fmt"
	"testing"
)

func TestSpawnRefusalProofDoesNotHideUnknownAcceptance(t *testing.T) {
	refusal := errors.New("synthetic pre-acceptance validation")
	unknown := errors.New("synthetic uncertain task write")
	marked := RejectBeforeSpawn(refusal)
	if !errors.Is(marked, refusal) || RejectBeforeSpawn(nil) != nil {
		t.Fatal("proof changed the underlying validation error")
	}
	for _, err := range []error{marked, fmt.Errorf("registry: %w", marked), errors.Join(marked, RejectBeforeSpawn(refusal))} {
		if !IsRejectedBeforeSpawn(err) {
			t.Fatal("lost explicit pre-acceptance proof", err)
		}
	}
	for _, err := range []error{nil, refusal, unknown, errors.Join(marked, unknown), fmt.Errorf("registry: %w", errors.Join(marked, unknown))} {
		if IsRejectedBeforeSpawn(err) {
			t.Fatal("unknown task acceptance was treated as a proved refusal", err)
		}
	}
}
