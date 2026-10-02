package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxProducedArtifacts bounds the immutable eligible output manifest per task.
const MaxProducedArtifacts = 32

// MaxOutputInvocationBranches matches the native parallel dispatch hard bound.
const MaxOutputInvocationBranches = 50

var (
	ErrOutputProvenance        = errors.New("tasks: invalid output provenance")
	ErrOutputInvocationUnknown = errors.New("tasks: prior output invocation outcome unresolved; do not invoke again")
	ErrOutputInvocationSettled = errors.New("tasks: prior output invocation settled; recover observation instead of invoking again")
)

// ProducedArtifact is a runtime-verified native binary output. It records the
// executed task's provenance independently of the blob's first-writer stamp.
// It contains neither bytes, source URLs, tool arguments nor credentials.
type ProducedArtifact struct {
	ID           string `json:"id"`
	SHA256       string `json:"sha256"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	InvocationID string `json:"invocation_id"`
	ContentIndex int    `json:"content_index"`
}

// OutputInvocationIntent names one deterministic native dispatch position.
// RequestSHA256 binds exact tool/arguments and consumed input revision; Position
// and Branch, not model-selected call identifiers, determine invocation identity.
type OutputInvocationIntent struct {
	Position      int64  `json:"position"`
	Branch        int    `json:"branch"`
	ToolName      string `json:"tool_name"`
	RequestSHA256 string `json:"request_sha256"`
}

// OutputInvocation retains only the current decision's bounded branch slots.
type OutputInvocation struct {
	ID     string                 `json:"id"`
	Intent OutputInvocationIntent `json:"intent"`
	State  string                 `json:"state"`
}

// OutputManifest is immutable after successful task completion. Nil means
// legacy/unknown provenance, never a known empty set. A new task starts at v1.
type OutputManifest struct {
	Version       int                `json:"version"`
	Position      int64              `json:"position"`
	Invocations   []OutputInvocation `json:"invocations,omitempty"`
	Artifacts     []ProducedArtifact `json:"artifacts,omitempty"`
	Uncertain     bool               `json:"uncertain,omitempty"`
	Sealed        bool               `json:"sealed,omitempty"`
	SHA256        string             `json:"sha256,omitempty"`
	InputRevision uint64             `json:"input_revision,omitempty"`
}

// CloneOutputManifest returns a defensive copy for concurrent registry reads.
func CloneOutputManifest(m *OutputManifest) *OutputManifest {
	if m == nil {
		return nil
	}
	out := *m
	out.Invocations = append([]OutputInvocation(nil), m.Invocations...)
	out.Artifacts = append([]ProducedArtifact(nil), m.Artifacts...)
	return &out
}
func outputID(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func outputHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ValidateOutputInvocation checks metadata shape without granting execution.
func ValidateOutputInvocation(i OutputInvocationIntent) error {
	if i.Position < 0 || i.Branch < 0 || i.Branch >= MaxOutputInvocationBranches || !outputID(i.ToolName, 255) || !outputHash(i.RequestSHA256) {
		return ErrOutputProvenance
	}
	return nil
}

// ValidateProducedArtifact checks a bounded exact native byte-version witness.
func ValidateProducedArtifact(a ProducedArtifact) error {
	media, _, err := mime.ParseMediaType(a.MIMEType)
	if !outputID(a.ID, 255) || !outputHash(a.SHA256) || !outputID(a.MIMEType, 255) || err != nil || !strings.Contains(media, "/") || a.SizeBytes <= 0 || a.SizeBytes > 64<<20 || !outputHash(a.InvocationID) || a.ContentIndex < 0 || a.ContentIndex >= MaxProducedArtifacts {
		return ErrOutputProvenance
	}
	return nil
}

// OutputManifestDigest binds the final eligible set to its exact task, owner,
// effective agent and consumed input revision. Invocation order is canonical.
func OutputManifestDigest(t *Task) (string, error) {
	if t == nil || t.OutputManifest == nil || t.OutputManifest.Version != 1 {
		return "", ErrOutputProvenance
	}
	refs := append([]ProducedArtifact(nil), t.OutputManifest.Artifacts...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].InvocationID == refs[j].InvocationID {
			return refs[i].ContentIndex < refs[j].ContentIndex
		}
		return refs[i].InvocationID < refs[j].InvocationID
	})
	raw, err := json.Marshal(struct {
		Version                      int
		TaskID                       TaskID
		Tenant, User, Session, Agent string
		InputRevision                uint64
		Artifacts                    []ProducedArtifact
	}{1, t.ID, t.Identity.TenantID, t.Identity.UserID, t.Identity.SessionID, t.AgentID, t.OutputManifest.InputRevision, refs})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateOutputManifest rejects corrupted or falsely sealed task provenance.
func ValidateOutputManifest(t *Task) error {
	if t == nil {
		return ErrOutputProvenance
	}
	m := t.OutputManifest
	if m == nil {
		return nil
	}
	if m.Version != 1 || m.Position < -1 || len(m.Invocations) > MaxOutputInvocationBranches || len(m.Artifacts) > MaxProducedArtifacts {
		return ErrOutputProvenance
	}
	seen := map[int]bool{}
	for _, i := range m.Invocations {
		if ValidateOutputInvocation(i.Intent) != nil || i.Intent.Position != m.Position || seen[i.Intent.Branch] || !outputHash(i.ID) {
			return ErrOutputProvenance
		}
		seen[i.Intent.Branch] = true
		switch i.State {
		case "pending", "succeeded", "failed":
		default:
			return ErrOutputProvenance
		}
	}
	type refKey struct {
		invocation string
		index      int
	}
	refs := map[refKey]bool{}
	for _, a := range m.Artifacts {
		if ValidateProducedArtifact(a) != nil {
			return ErrOutputProvenance
		}
		key := refKey{a.InvocationID, a.ContentIndex}
		if refs[key] {
			return ErrOutputProvenance
		}
		refs[key] = true
	}
	if m.Sealed {
		if m.Uncertain || t.Status != StatusComplete || t.Result == nil || m.InputRevision != t.Result.IncorporatedInputRevision {
			return ErrOutputProvenance
		}
		for _, i := range m.Invocations {
			if i.State == "pending" {
				return ErrOutputProvenance
			}
		}
		hash, err := OutputManifestDigest(t)
		if err != nil || hash != m.SHA256 {
			return ErrOutputProvenance
		}
	} else if m.SHA256 != "" {
		return ErrOutputProvenance
	}
	return nil
}

type outputTaskKey struct{}

// WithOutputTask binds a native run loop's engine-owned TaskID. It is not a
// Protocol request field and never derives from an artifact's storage stamp.
func WithOutputTask(ctx context.Context, id TaskID) context.Context {
	return context.WithValue(ctx, outputTaskKey{}, id)
}

// OutputTaskFromContext distinguishes native task execution from runless/App
// helpers. Absence does not create a task-output witness from session ownership.
func OutputTaskFromContext(ctx context.Context) (TaskID, bool) {
	id, ok := ctx.Value(outputTaskKey{}).(TaskID)
	return id, ok && id != ""
}
