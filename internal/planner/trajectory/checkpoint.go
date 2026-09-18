package trajectory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SummaryCoverage identifies the exact prefix replaced by a portable summary.
// ThroughStep is an exclusive index in the original Steps slice. The runtime,
// never the summarizing model, produces this metadata.
type SummaryCoverage struct {
	Version      int    `json:"version"`
	Generation   uint64 `json:"generation"`
	ThroughStep  int    `json:"through_step"`
	PrefixDigest string `json:"prefix_digest"`
}

// ErrInvalidCoverage means a checkpoint cannot be matched to this trajectory.
var ErrInvalidCoverage = errors.New("trajectory: invalid summary coverage")

// ReplayStart returns the first step not represented by a validated checkpoint.
// Legacy summaries have no provable coverage; they never hide existing steps.
func (t *Trajectory) ReplayStart() (int, error) {
	if t == nil || t.Summary == nil || t.Summary.Coverage == nil {
		return 0, nil
	}
	c := t.Summary.Coverage
	if c.Version != 1 || c.Generation == 0 || c.ThroughStep < 1 || c.ThroughStep > len(t.Steps) || !t.Summary.HasContent() {
		return 0, ErrInvalidCoverage
	}
	digest, err := t.PrefixDigest(c.ThroughStep)
	if err != nil {
		return 0, err
	}
	if digest != c.PrefixDigest {
		return 0, ErrInvalidCoverage
	}
	return c.ThroughStep, nil
}

// ActiveSummary excludes an unversioned summary when its original steps remain
// available. Without original steps, legacy narrative is retained as partial
// context, without assigning it an invented coverage boundary.
func (t *Trajectory) ActiveSummary() *Summary {
	if t == nil || (t.Summary != nil && t.Summary.Coverage == nil && len(t.Steps) > 0) {
		return nil
	}
	return t.Summary
}

// PrefixDigest binds coverage to the query and permitted model-facing evidence.
// Diagnostic/raw duplicates and live tool handles are deliberately excluded.
// Canonical JSON makes the digest stable across typed-action JSON round trips.
func (t *Trajectory) PrefixDigest(end int) (string, error) {
	if t == nil || end < 0 || end > len(t.Steps) {
		return "", ErrInvalidCoverage
	}
	steps := make([]Step, end)
	for i, s := range t.Steps[:end] {
		steps[i] = ModelStep(s)
	}
	b, err := json.Marshal(struct {
		Query string `json:"query"`
		Steps []Step `json:"steps"`
	}{t.Query, steps})
	if err != nil {
		return "", fmt.Errorf("trajectory: prefix encoding: %w", err)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// ModelStep removes diagnostic-only copies from a compaction input. The latest
// permitted observation, action, visible preamble, and error remain exact.
func ModelStep(s Step) Step {
	obs := s.LLMObservation
	if obs == nil {
		obs = s.Observation
	}
	return Step{Action: s.Action, LLMObservation: obs, Error: s.Error, AssistantPreamble: s.AssistantPreamble}
}

// HasContent rejects vacuous summaries; an explanatory note is not work state.
func (s *Summary) HasContent() bool {
	if s == nil {
		return false
	}
	for _, group := range [][]string{s.Goals, s.Facts, s.Pending} {
		for _, value := range group {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return strings.TrimSpace(s.LastOutputDigest) != ""
}

// CloneSummary detaches generated or installed state from reusable summarizers.
func CloneSummary(s *Summary) *Summary {
	if s == nil {
		return nil
	}
	out := *s
	out.Goals = append([]string(nil), s.Goals...)
	out.Facts = append([]string(nil), s.Facts...)
	out.Pending = append([]string(nil), s.Pending...)
	if s.Coverage != nil {
		c := *s.Coverage
		out.Coverage = &c
	}
	return &out
}
