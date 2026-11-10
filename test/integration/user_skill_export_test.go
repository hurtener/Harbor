package integration_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// This crosses the authenticated HTTP mux, production import/export service,
// durable skill driver and ordinary artifact byte-read API. A direct service
// round trip cannot detect a missing route or a broken wire projection.
func TestE2E_UserSkillExport_AuthenticatedPortableArtifact(t *testing.T) {
	s := newAgentReachStack(t, withPersonalPackageImports(t))
	id := arIdentity("personal-export")
	agent := s.stack.AgentConfigID
	token := arToken(t, s, id, nil, true, []string{agent})
	scope := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	artifactScope := prototypes.ArtifactScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for _, file := range []struct{ name, content string }{
		{"SKILL.md", "---\nname: portable-personal\ndescription: Portable test instructions\n---\n# Instructions\nRead [reference](docs/reference.md).\n"},
		{"docs/reference.md", "# Reference\nExact support bytes.\n"},
	} {
		w, err := zw.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	status, raw := arPost(t, s, "/v1/control/artifacts.put", token, prototypes.ArtifactsPutRequest{
		Scope: artifactScope, Bytes: archive.Bytes(),
		Opts: prototypes.ArtifactsPutOpts{Filename: "portable-personal.zip", MimeType: "application/zip"},
	})
	arAssertStatus(t, status, raw, http.StatusOK)
	uploaded := arDecode[prototypes.ArtifactsPutResponse](t, raw)
	validate := func(artifactID string) prototypes.AgentConfigUserSkillsImportValidateResponse {
		t.Helper()
		status, raw := arPost(t, s, "/v1/agent_config/user/skills/import_validate", token,
			prototypes.AgentConfigUserSkillsImportValidateRequest{Identity: scope, AgentID: agent, ArtifactID: artifactID})
		arAssertStatus(t, status, raw, http.StatusOK)
		return arDecode[prototypes.AgentConfigUserSkillsImportValidateResponse](t, raw)
	}
	review := validate(uploaded.Ref.ID)
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/import_commit", token,
		prototypes.AgentConfigUserSkillsImportCommitRequest{
			Identity: scope, AgentID: agent, Name: review.Review.Name,
			ProposalToken: review.ProposalToken, ReviewedPackageHash: review.PackageHash,
			ExpectedContentHash: review.ExpectedContentHash,
		})
	arAssertStatus(t, status, raw, http.StatusOK)
	exportReq := prototypes.AgentConfigUserSkillsExportRequest{Identity: scope, AgentID: agent, Name: "portable-personal"}
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/export", token, exportReq)
	arAssertStatus(t, status, raw, http.StatusOK)
	exported := arDecode[prototypes.AgentConfigUserSkillsExportResponse](t, raw)
	if exported.Name != exportReq.Name || exported.PackageHash != review.PackageHash || exported.ProtocolVersion != prototypes.ProtocolVersion || exported.SizeBytes <= 0 || exported.Artifact.SizeBytes != exported.SizeBytes || exported.Artifact.Scope != artifactScope || exported.Artifact.MimeType != "application/zip" {
		t.Fatalf("export metadata: %+v", exported)
	}
	status, raw = arPost(t, s, "/v1/control/artifacts.get", token,
		prototypes.ArtifactsGetRequest{Scope: artifactScope, ID: exported.Artifact.ID})
	arAssertStatus(t, status, raw, http.StatusOK)
	downloaded := arDecode[prototypes.ArtifactsGetResponse](t, raw)
	if downloaded.Truncated || int64(len(downloaded.Content)) != exported.SizeBytes {
		t.Fatalf("exported byte-read mismatch: %+v", downloaded)
	}
	zr, err := zip.NewReader(bytes.NewReader(downloaded.Content), int64(len(downloaded.Content)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || zr.File[0].Name != "SKILL.md" || zr.File[1].Name != "docs/reference.md" {
		t.Fatalf("exported archive members: %+v", zr.File)
	}
	if replay := validate(exported.Artifact.ID); replay.PackageHash != review.PackageHash {
		t.Fatalf("export re-import hash = %s, want %s", replay.PackageHash, review.PackageHash)
	}
	exportReq.Name = "missing-personal"
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/export", token, exportReq)
	arAssertStatus(t, status, raw, http.StatusNotFound)
	arAssertCode(t, raw, "not_found")
	foreign := id
	foreign.UserID = "other-personal-owner"
	foreignToken := arToken(t, s, foreign, nil, true, []string{agent})
	exportReq.Identity = prototypes.IdentityScope{Tenant: foreign.TenantID, User: foreign.UserID, Session: foreign.SessionID}
	exportReq.Name = "portable-personal"
	status, raw = arPost(t, s, "/v1/agent_config/user/skills/export", foreignToken, exportReq)
	arAssertStatus(t, status, raw, http.StatusNotFound)
	arAssertCode(t, raw, "not_found")
}
