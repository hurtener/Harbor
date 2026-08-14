package drafter

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// skillmd.go — the deterministic renderer of the canonical
// resource-free SKILL.md document for a validated
// `skillpkg.PackageSkill`.
//
// The rendered document is the ONE artifact this lane persists, and it
// is the exact document shape the canonical complete-skill-package
// ingest (and its validate/commit workflow) accepts: `---`-fenced
// closed frontmatter (name / title / trigger / task_type / tags /
// required_tools / required_namespaces / required_tags) followed by
// the description and the canonical body sections (Steps,
// Preconditions, Failure modes).
//
// The renderer is fail-loud about renderability: a draft whose text
// cannot survive the canonical single-document ingest (embedded
// support references — the draft is resource-free — `## `-heading
// prose, or multi-line list items) is refused with
// ErrUnrenderableSkill rather than persisted as an artifact the
// validate/commit workflow would reject.

// draftFrontmatter mirrors the CLOSED package frontmatter key set. The
// canonical ingest accepts exactly these keys and rejects every other
// (authority-bearing or unknown) key, so this lane can never emit an
// envelope field the ingest would reject or reinterpret.
type draftFrontmatter struct {
	Name               string   `yaml:"name"`
	Title              string   `yaml:"title,omitempty"`
	Trigger            string   `yaml:"trigger"`
	TaskType           string   `yaml:"task_type,omitempty"`
	Tags               []string `yaml:"tags,omitempty"`
	RequiredTools      []string `yaml:"required_tools,omitempty"`
	RequiredNamespaces []string `yaml:"required_namespaces,omitempty"`
	RequiredTags       []string `yaml:"required_tags,omitempty"`
}

// RenderSkillMD renders the canonical resource-free SKILL.md document
// bytes for a validated PackageSkill. The skill is validated first (a
// structurally invalid draft has no document), then checked for
// renderability, then serialized deterministically.
func RenderSkillMD(s skillpkg.PackageSkill) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := checkRenderable(s); err != nil {
		return nil, err
	}
	rawFM, err := renderFrontmatter(s)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(rawFM)
	b.WriteString("---\n")
	b.WriteString(renderBody(s))
	doc := b.Bytes()
	if int64(len(doc)) > skillpkg.MaxPackageSkillMDBytes {
		return nil, fmt.Errorf("%w: document is %d bytes (limit %d)",
			ErrDraftDocumentTooLarge, len(doc), skillpkg.MaxPackageSkillMDBytes)
	}
	return doc, nil
}

// checkRenderable verifies the draft's text can survive the canonical
// resource-free single-document ingest:
//
//   - the assembled body carries NO support reference (image or
//     relative/absolute link) — the draft is resource-free, so any
//     such reference would fail the ingest's self-containment check;
//   - the description contains no `## `-heading line — the canonical
//     body state machine treats every `## ` line as a section
//     heading, so a `## ` line in prose would misclassify or fail;
//   - every procedural list item is a single line — the canonical
//     state machine accepts only `- ` list items in sections.
func checkRenderable(s skillpkg.PackageSkill) error {
	if strings.Contains(s.Description, "\n## ") || strings.HasPrefix(s.Description, "## ") {
		return fmt.Errorf("%w: description contains a section heading", ErrUnrenderableSkill)
	}
	for field, items := range map[string][]string{
		"steps": s.Steps, "preconditions": s.Preconditions, "failure_modes": s.FailureModes,
	} {
		for _, item := range items {
			if strings.ContainsRune(item, '\n') || strings.ContainsRune(item, '\r') {
				return fmt.Errorf("%w: %s item contains an embedded newline", ErrUnrenderableSkill, field)
			}
		}
	}
	body := renderBody(s)
	for _, r := range skillpkg.ScanSupportRefs(body) {
		kind, _, _ := skillpkg.SplitDest(r.Dest)
		switch r.Kind {
		case skillpkg.SupportRefImage:
			return fmt.Errorf("%w: image reference %q (a draft is resource-free)", ErrUnrenderableSkill, r.Dest)
		case skillpkg.SupportRefLink:
			if kind != skillpkg.DestScheme && kind != skillpkg.DestFragment {
				return fmt.Errorf("%w: link reference %q (a draft is resource-free)", ErrUnrenderableSkill, r.Dest)
			}
		}
	}
	return nil
}

// renderFrontmatter emits the closed frontmatter YAML for the draft.
// Values are emitted through the YAML serializer verbatim (goccy
// quotes anything that needs it and block-scalars multi-line values —
// block scalar content is indented, so neither can collide with the
// ingest's fence scan). The marshaler's trailing newline is KEPT so
// the closing `---` fence lands at the start of its own line.
func renderFrontmatter(s skillpkg.PackageSkill) ([]byte, error) {
	fm := draftFrontmatter{
		Name:               s.Name,
		Title:              s.Title,
		Trigger:            s.Trigger,
		TaskType:           s.TaskType,
		Tags:               s.Tags,
		RequiredTools:      s.RequiredTools,
		RequiredNamespaces: s.RequiredNS,
		RequiredTags:       s.RequiredTags,
	}
	out, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("%w: render frontmatter: %v", ErrUnrenderableSkill, err)
	}
	// A column-0 `---` inside the frontmatter would be misread as the
	// closing fence by the ingest. The serializer quotes or indents
	// everything that could produce one, but the check stays as a
	// cheap structural guarantee.
	for _, line := range bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n")) {
		if string(line) == "---" {
			return nil, fmt.Errorf("%w: frontmatter contains a standalone fence line", ErrUnrenderableSkill)
		}
	}
	return out, nil
}

// renderBody emits the canonical logical body: the description
// followed by each non-empty canonical section in canonical order,
// mirroring the canonical export body emission so the round trip is
// byte-exact.
func renderBody(s skillpkg.PackageSkill) string {
	var b strings.Builder
	if s.Description != "" {
		b.WriteString(s.Description)
		b.WriteByte('\n')
	}
	sections := []struct {
		heading string
		items   []string
	}{
		{"## Steps", s.Steps},
		{"## Preconditions", s.Preconditions},
		{"## Failure modes", s.FailureModes},
	}
	for _, sec := range sections {
		if len(sec.items) == 0 {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(sec.heading)
		b.WriteByte('\n')
		b.WriteByte('\n')
		for _, item := range sec.items {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
