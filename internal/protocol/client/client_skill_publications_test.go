package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestRuntimeClient_SkillPublicationsMethods_UseCanonicalRoutesAndClientIdentity(t *testing.T) {
	wantIdentity := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	tests := []struct {
		name string
		path string
		call func(RuntimeClient) error
	}{
		{
			name: "publish", path: "/v1/control/skills.publications.publish",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsPublish(context.Background(), types.SkillPublicationPublishRequest{
					Identity: types.IdentityScope{Tenant: "forged", User: "forged", Session: "forged"}, Name: "ops", IdempotencyKey: "p", ExpectedAbsent: true,
				})
				return err
			},
		},
		{
			name: "list", path: "/v1/control/skills.publications.list",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsList(context.Background(), types.SkillPublicationListRequest{Identity: types.IdentityScope{Tenant: "forged"}})
				return err
			},
		},
		{
			name: "get", path: "/v1/control/skills.publications.get",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsGet(context.Background(), types.SkillPublicationGetRequest{Identity: types.IdentityScope{Tenant: "forged"}, PublicationID: "pub"})
				return err
			},
		},
		{
			name: "successor", path: "/v1/control/skills.publications.publish_successor",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsSuccessor(context.Background(), types.SkillPublicationSuccessorRequest{Identity: types.IdentityScope{Tenant: "forged"}, PublicationID: "pub", IdempotencyKey: "s"})
				return err
			},
		},
		{
			name: "retire", path: "/v1/control/skills.publications.retire",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsRetire(context.Background(), types.SkillPublicationRetireRequest{Identity: types.IdentityScope{Tenant: "forged"}, PublicationID: "pub", IdempotencyKey: "r"})
				return err
			},
		},
		{
			name: "available", path: "/v1/control/skills.publications.available",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsAvailable(context.Background(), types.SkillPublicationAvailableRequest{Identity: types.IdentityScope{Tenant: "forged"}})
				return err
			},
		},
		{
			name: "install", path: "/v1/control/skills.publications.install",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsInstall(context.Background(), types.SkillPublicationInstallRequest{Identity: types.IdentityScope{Tenant: "forged"}, AgentID: "agent", PublicationID: "pub", RevisionID: "rev", IdempotencyKey: "i"})
				return err
			},
		},
		{
			name: "update", path: "/v1/control/skills.publications.update",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsUpdate(context.Background(), types.SkillPublicationUpdateRequest{Identity: types.IdentityScope{Tenant: "forged"}, AgentID: "agent", PublicationID: "pub", RevisionID: "rev", IdempotencyKey: "u"})
				return err
			},
		},
		{
			name: "remove", path: "/v1/control/skills.publications.remove",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsRemove(context.Background(), types.SkillPublicationRemoveRequest{Identity: types.IdentityScope{Tenant: "forged"}, AgentID: "agent", IdempotencyKey: "d"})
				return err
			},
		},
		{
			name: "references-list", path: "/v1/control/skills.publications.references.list",
			call: func(c RuntimeClient) error {
				_, err := c.SkillPublicationsReferencesList(context.Background(), types.SkillPublicationReferencesListRequest{Identity: types.IdentityScope{Tenant: "forged"}})
				return err
			},
		},
	}
	lastPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		var body struct {
			Identity types.IdentityScope `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request %s: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Identity != wantIdentity {
			t.Errorf("request identity = %+v, want client identity %+v", body.Identity, wantIdentity)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol_version":"0.1.0"}`))
	}))
	defer server.Close()

	client, ok := testClient(t, server).(RuntimeClient)
	if !ok {
		t.Fatal("New client does not implement RuntimeClient")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// The handler above checks the request identity; this assertion
			// pins only route/response behavior here so each typed method is
			// exercised without sharing mutable request state.
			if err := test.call(client); err != nil {
				t.Fatalf("call: %v", err)
			}
			if lastPath != test.path {
				t.Fatalf("path = %q, want %q", lastPath, test.path)
			}
		})
	}
}

func TestRuntimeClient_SkillPublicationsMethodSet(t *testing.T) {
	var _ RuntimeClient = (*client)(nil)
	for _, method := range []methods.Method{
		methods.MethodSkillsPublicationsPublish,
		methods.MethodSkillsPublicationsList,
		methods.MethodSkillsPublicationsGet,
		methods.MethodSkillsPublicationsSuccessor,
		methods.MethodSkillsPublicationsRetire,
		methods.MethodSkillsPublicationsAvailable,
		methods.MethodSkillsPublicationsInstall,
		methods.MethodSkillsPublicationsUpdate,
		methods.MethodSkillsPublicationsRemove,
		methods.MethodSkillsPublicationsReferencesList,
	} {
		if !methods.IsSkillPublicationMethod(method) {
			t.Fatalf("method %q is not registered as skill-publication", method)
		}
	}
}
