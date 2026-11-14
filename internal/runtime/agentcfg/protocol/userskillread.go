package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// MaxSupportFileReadBytes bounds the support-file text a single
// `agent_config.user.skills.read` returns.
const MaxSupportFileReadBytes = 256 << 10

// ErrUserSkillSupportFileNotFound means the requested support path names no
// manifest entry of the caller's skill.
var ErrUserSkillSupportFileNotFound = errors.New("agentcfg/protocol: support file not found")

// userSkillView is the caller-owned skill as both read and candidate
// snapshot see it.
type userSkillView struct {
	skill       skills.Skill
	packageHash string
	document    []byte
	manifest    []skillpkg.SupportFile
	supports    map[string][]byte
}

// loadOwnSkill resolves the verified caller's own user-rung skill on an Agent
// in signed reach. A complete installed package is preferred; a skill that
// predates packages is read from the user rung and rendered canonically.
// Source authority is always the verified caller; the wire identity is only a
// compatibility witness.
func (s *UserSkillImportService) loadOwnSkill(ctx context.Context, wire prototypes.IdentityScope, agentID, rawName string) (userSkillView, error) {
	if err := ctx.Err(); err != nil {
		return userSkillView{}, err
	}
	id, q, err := s.resolveIdentityAndReach(ctx, agentID)
	if err != nil {
		return userSkillView{}, err
	}
	if (wire.Tenant != "" && wire.Tenant != id.TenantID) ||
		(wire.User != "" && wire.User != id.UserID) ||
		(wire.Session != "" && wire.Session != id.SessionID) {
		return userSkillView{}, ErrUserSkillImportIdentityRequired
	}
	if err := s.ensureNotRetiredImport(ctx, q, agentID); err != nil {
		return userSkillView{}, err
	}
	name := skillpkg.CanonicalName(rawName)
	if name == "" {
		return userSkillView{}, fmt.Errorf("%w: skill name is empty", ErrIdentityRequired)
	}
	unit, err := s.store.GetInstalledPackage(ctx, q, agentID, name)
	switch {
	case err == nil:
		return s.packageView(ctx, unit)
	case errors.Is(err, skills.ErrInstalledPackageNotFound):
		return s.legacyView(ctx, q, agentID, name)
	default:
		return userSkillView{}, fmt.Errorf("skill read: read caller package: %w", err)
	}
}

func (s *UserSkillImportService) packageView(ctx context.Context, unit skills.InstalledPackage) (userSkillView, error) {
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		return userSkillView{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	logical := unit.Package.Skill
	ingest := importer.PackageIngest{
		Skill: skills.Skill{
			Name: logical.Name, Title: logical.Title, Description: logical.Description,
			Trigger: logical.Trigger, TaskType: logical.TaskType, Tags: logical.Tags,
			Steps: logical.Steps, Preconditions: logical.Preconditions, FailureModes: logical.FailureModes,
			RequiredTools: logical.RequiredTools, RequiredNS: logical.RequiredNS, RequiredTags: logical.RequiredTags,
			Scope: skills.ScopeProject, Origin: skills.OriginPack,
		},
		Package: unit.Package.Normalize(), Hash: unit.PackageHash,
	}
	body, err := s.importer.MaterializePackageBody(ctx, ingest)
	if err != nil {
		return userSkillView{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	exported, err := s.importer.ExportPackage(ctx, ingest, body)
	if err != nil {
		return userSkillView{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	supports := make(map[string][]byte, len(unit.Package.Supports))
	for _, support := range unit.Package.Supports {
		supports[support.Path] = support.Data
	}
	return userSkillView{skill: unit.Skill, packageHash: unit.PackageHash, document: exported.Document, manifest: exported.Manifest, supports: supports}, nil
}

func (s *UserSkillImportService) legacyView(ctx context.Context, q identity.Quadruple, agentID, name string) (userSkillView, error) {
	list, err := s.store.List(ctx, q, skills.ListFilter{Scope: skills.ScopeUser, AgentID: agentID})
	if err != nil {
		return userSkillView{}, fmt.Errorf("skill read: list caller skills: %w", err)
	}
	for _, sk := range list {
		if sk.Name != name {
			continue
		}
		doc, err := s.importer.Export(ctx, sk, importer.ImportArtifacts{})
		if err != nil {
			return userSkillView{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
		}
		return userSkillView{skill: sk, document: doc}, nil
	}
	return userSkillView{}, fmt.Errorf("%w: name=%q (user scope)", skills.ErrSkillNotFound, name)
}

// Read returns the verified caller's own personal skill: its logical body,
// canonical SKILL.md document, support manifest and, when SupportPath names
// a manifest entry, that file's bounded UTF-8 text. It changes nothing.
func (s *UserSkillImportService) Read(ctx context.Context, req prototypes.AgentConfigUserSkillsReadRequest) (prototypes.AgentConfigUserSkillsReadResponse, error) {
	view, err := s.loadOwnSkill(ctx, req.Identity, req.AgentID, req.Name)
	if err != nil {
		return prototypes.AgentConfigUserSkillsReadResponse{}, err
	}
	out := prototypes.AgentConfigUserSkillsReadResponse{
		Name: view.skill.Name, PackageHash: view.packageHash, Skill: userSkillToWire(view.skill),
		Document: string(view.document), ProtocolVersion: prototypes.ProtocolVersion,
	}
	if !view.skill.UpdatedAt.IsZero() {
		out.UpdatedAt = view.skill.UpdatedAt.UTC().Format(time.RFC3339)
	}
	for _, entry := range view.manifest {
		out.SupportFiles = append(out.SupportFiles, prototypes.SkillSupportFileInfo{Path: entry.Path, MIME: entry.Mime, SizeBytes: entry.Size})
	}
	if path := req.SupportPath; path != "" {
		var found *skillpkg.SupportFile
		for i := range view.manifest {
			if view.manifest[i].Path == path {
				found = &view.manifest[i]
				break
			}
		}
		data, ok := view.supports[path]
		if found == nil || !ok {
			return prototypes.AgentConfigUserSkillsReadResponse{}, fmt.Errorf("%w: %q", ErrUserSkillSupportFileNotFound, path)
		}
		out.SupportFile = supportFileContent(*found, data)
	}
	return out, nil
}

// supportFileContent projects one support file as bounded text, or marks it
// binary. Truncation never splits a UTF-8 sequence.
func supportFileContent(entry skillpkg.SupportFile, data []byte) *prototypes.SkillSupportFileContent {
	out := &prototypes.SkillSupportFileContent{Path: entry.Path, MIME: entry.Mime, SizeBytes: entry.Size}
	if !textLikeMIME(entry.Mime) || !utf8.Valid(data) {
		out.Binary = true
		return out
	}
	if len(data) > MaxSupportFileReadBytes {
		cut := MaxSupportFileReadBytes
		for cut > 0 && !utf8.RuneStart(data[cut]) { //nolint:gosec // G602 false positive: len(data) > MaxSupportFileReadBytes >= cut
			cut--
		}
		data = data[:cut]
		out.Truncated = true
	}
	out.Text = string(data)
	return out
}

func textLikeMIME(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/yaml", "application/x-yaml", "application/toml", "application/xml",
		"application/javascript", "application/x-sh", "application/x-python", "application/x-shellscript", "application/sql":
		return true
	}
	return false
}

// userSkillToWire projects a stored personal skill without importer-private
// bookkeeping keys.
func userSkillToWire(sk skills.Skill) prototypes.AgentConfigAgentPackItem {
	item := agentPackItemToWire(skills.PackItemFromSkill(sk))
	item.Origin = string(sk.Origin)
	item.Scope = string(skills.ScopeUser)
	item.OriginRef = ""
	for key := range item.Extra {
		if strings.HasPrefix(key, "_") {
			delete(item.Extra, key)
		}
	}
	if len(item.Extra) == 0 {
		item.Extra = nil
	}
	return item
}

// PersonalSkillSnapshot is a frozen copy of a caller's own personal skill
// for organization review.
type PersonalSkillSnapshot struct {
	Skill        skills.Skill
	PackageHash  string
	SupportFiles int
}

// SnapshotPersonalSkill freezes the verified caller's own personal skill on
// an Agent in signed reach (the same authority as Read and export).
func (s *UserSkillImportService) SnapshotPersonalSkill(ctx context.Context, agentID, name string) (PersonalSkillSnapshot, error) {
	view, err := s.loadOwnSkill(ctx, prototypes.IdentityScope{}, agentID, name)
	if err != nil {
		return PersonalSkillSnapshot{}, err
	}
	return PersonalSkillSnapshot{Skill: view.skill, PackageHash: view.packageHash, SupportFiles: len(view.manifest)}, nil
}
