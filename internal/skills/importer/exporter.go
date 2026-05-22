package importer

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/hurtener/Harbor/internal/skills"
)

// artifactRefRegexp matches `artifact://<ID>` references the
// importer substituted into the body on Import. Capture group 1 is
// the ID.
var artifactRefRegexp = regexp.MustCompile(`artifact://([A-Za-z0-9_\-]+)`)

// doExport is the top-level Export pipeline. Split out so it's
// testable without the importer struct.
//
// Pipeline:
//
//  1. Read the raw frontmatter from `Skill.Extra["_importer.frontmatter_raw"]`
//     (stashed by Import). If absent — caller built the Skill by
//     hand — synthesise frontmatter from the structured fields.
//  2. Emit `---\n<raw>---\n`.
//  3. Emit the description (after de-substituting attachment refs).
//  4. Emit each canonical section that has items, in canonical order.
//
// The result is byte-stable on every spec-compliant source produced
// by Import — asserted on the golden corpus via `bytes.Equal`.
func doExport(skill skills.Skill, attachments ImportArtifacts) ([]byte, error) {
	// Build reverse lookup: id -> path.
	idToPath := make(map[string]string, len(attachments.PathToRef))
	for _, m := range attachments.PathToRef {
		idToPath[m.Ref.ID] = m.Path
	}

	var out bytes.Buffer

	// 1. Frontmatter.
	rawFM, ok := skillFrontmatterRaw(skill)
	if !ok {
		// Synthesise from structured fields. Round-trip from a
		// synthetic Skill is not byte-stable against an unrelated
		// source — the round-trip invariant gates Import->Export
		// only. The synthesised form is deterministic so a
		// caller-built Skill round-trips byte-stable through
		// THIS exporter (Export->Import->Export idempotent).
		rawFM = synthesiseFrontmatter(skill)
	}
	out.WriteString(frontmatterFence)
	out.WriteByte('\n')
	out.Write(rawFM)
	out.WriteString(frontmatterFence)
	out.WriteByte('\n')

	// 2. Description (de-substitute artifact refs).
	descOut, err := desubstituteArtifacts(skill.Description, idToPath)
	if err != nil {
		return nil, err
	}
	if descOut != "" {
		out.WriteString(descOut)
		out.WriteByte('\n')
	}

	// 3. Sections in canonical order.
	sections := []struct {
		Section canonicalSection
		Items   []string
	}{
		{sectionSteps, skill.Steps},
		{sectionPreconditions, skill.Preconditions},
		{sectionFailureModes, skill.FailureModes},
	}
	for _, sec := range sections {
		if len(sec.Items) == 0 {
			continue
		}
		out.WriteByte('\n')
		out.WriteString(canonicalHeading(sec.Section))
		out.WriteByte('\n')
		out.WriteByte('\n')
		for _, item := range sec.Items {
			it, err := desubstituteArtifacts(item, idToPath)
			if err != nil {
				return nil, err
			}
			out.WriteString("- ")
			out.WriteString(it)
			out.WriteByte('\n')
		}
	}

	return out.Bytes(), nil
}

// skillFrontmatterRaw returns the raw frontmatter slice stashed in
// Skill.Extra by Import. False if the slot is absent (caller-built
// Skill).
func skillFrontmatterRaw(s skills.Skill) ([]byte, bool) {
	if s.Extra == nil {
		return nil, false
	}
	v, ok := s.Extra["_importer.frontmatter_raw"]
	if !ok {
		return nil, false
	}
	str, ok := v.(string)
	if !ok {
		return nil, false
	}
	return []byte(str), true
}

// synthesiseFrontmatter emits a deterministic YAML frontmatter for a
// caller-built Skill that did not pass through Import. Used by the
// Phase 41 generator path (planned) and by tests that build a Skill
// directly. Field ordering matches the goccy/go-yaml default for the
// frontmatterFields struct: name, title, trigger, task_type, tags,
// required_tools, required_namespaces, required_tags, scope. Empty
// fields are omitted.
//
// This is NOT used in the byte-stable round-trip path — that path
// always reads from Extra["_importer.frontmatter_raw"]. The synthetic
// path is its own deterministic shape: Export of a synthetic Skill
// followed by Import followed by Export reproduces the synthetic
// shape (`Export -> Import -> Export` idempotent).
func synthesiseFrontmatter(s skills.Skill) []byte {
	var b bytes.Buffer
	emitStr := func(k, v string) {
		if v == "" {
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	emitList := func(k string, v []string) {
		if len(v) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", k)
		for _, item := range v {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
	}
	emitStr("name", s.Name)
	emitStr("title", s.Title)
	emitStr("trigger", s.Trigger)
	emitStr("task_type", s.TaskType)
	emitList("tags", s.Tags)
	emitList("required_tools", s.RequiredTools)
	emitList("required_namespaces", s.RequiredNS)
	emitList("required_tags", s.RequiredTags)
	if s.Scope != "" && s.Scope != skills.ScopeProject {
		// Only emit scope when it diverges from the default; the
		// default is implicit so omitting it in source is allowed.
		emitStr("scope", string(s.Scope))
	}
	return b.Bytes()
}

// desubstituteArtifacts replaces every `artifact://<ID>` token in s
// with its corresponding source-side path from idToPath. Returns
// wrapped ErrInvalidAttachmentRef when an ID is absent from
// idToPath — Export will not silently emit a dangling ref.
func desubstituteArtifacts(s string, idToPath map[string]string) (string, error) {
	if !strings.Contains(s, artifactScheme) {
		return s, nil
	}
	var firstErr error
	out := artifactRefRegexp.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		// Strip "artifact://" prefix to get the ID.
		id := strings.TrimPrefix(match, artifactScheme)
		path, ok := idToPath[id]
		if !ok {
			firstErr = fmt.Errorf("%w: id=%q has no path mapping",
				ErrInvalidAttachmentRef, id)
			return match
		}
		return path
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}
