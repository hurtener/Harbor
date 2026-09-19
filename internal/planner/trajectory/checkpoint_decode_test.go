package trajectory_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

func TestCheckpointDecode_ExactNumbersAndCoverage(t *testing.T) {
	t.Parallel()
	for _, number := range []string{"9007199254740993", "18446744073709551615", "-9007199254740993", "1.234567890123456789"} {
		t.Run(number, func(t *testing.T) {
			tr := &trajectory.Trajectory{Query: "continue", Steps: []trajectory.Step{
				{Action: map[string]any{"version": json.Number(number)}, LLMObservation: map[string]any{"version": json.Number(number), "more": false}},
				{LLMObservation: "fresh"},
			}}
			digest, err := tr.PrefixDigest(1)
			if err != nil {
				t.Fatal(err)
			}
			tr.Summary = &trajectory.Summary{Facts: []string{"previous work"}, Coverage: &trajectory.SummaryCoverage{Version: 1, Generation: 1, ThroughStep: 1, PrefixDigest: digest}}
			encoded, err := tr.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := trajectory.Deserialize(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if start, err := restored.ReplayStart(); err != nil || start != 1 {
				t.Fatalf("valid checkpoint lost through decoding: start=%d err=%v", start, err)
			}
			back, err := restored.Serialize()
			if err != nil || !bytes.Equal(encoded, back) {
				t.Fatalf("exact evidence changed: %v", err)
			}
			restored.Steps[0].LLMObservation = "changed"
			if _, err := restored.ReplayStart(); !errors.Is(err, trajectory.ErrInvalidCoverage) {
				t.Fatalf("changed evidence accepted: %v", err)
			}
		})
	}
}

func TestCheckpointDecode_RejectsTrailingOrNullData(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`null`, `{} {}`, `{} null`, `{} trailing`, `[]`} {
		t.Run(input, func(t *testing.T) {
			got, err := trajectory.Deserialize([]byte(input))
			if err == nil || got != nil {
				t.Fatalf("invalid checkpoint accepted: %q", input)
			}
		})
	}
	if _, err := trajectory.Deserialize([]byte("{} \n\t")); err != nil {
		t.Fatalf("valid whitespace refused: %v", err)
	}
}

func TestCheckpointDecode_ConcurrentExactEvidence(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			number := fmt.Sprintf("900719925474%04d", 1000+i)
			input := fmt.Sprintf(`{"query":"session-%d","steps":[{"llm_observation":{"id":%s}}]}`, i, number)
			tr, err := trajectory.Deserialize([]byte(input))
			if err != nil {
				t.Error(err)
				return
			}
			back, err := tr.Serialize()
			if err != nil || !strings.Contains(string(back), `"id":`+number) || tr.Query != fmt.Sprintf("session-%d", i) {
				t.Errorf("round-trip lost exact scoped evidence: %v", err)
			}
		}()
	}
	wg.Wait()
}
