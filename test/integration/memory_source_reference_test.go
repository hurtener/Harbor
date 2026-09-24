package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func memorySourcePost(t *testing.T, url, route string, id identity.Identity, request any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return htPost(t, url, route, id, string(body))
}

// The real BuildMux must connect memory.get references to artifacts.get without
// making a second retained copy. Reads stay bounded and source deletion wins.
func TestMemorySourceReference_HTTPReadAndDeletion(t *testing.T) {
	rig := newHeavyThresholdRig(t, 0)
	srv := httptest.NewServer(rig.mux)
	t.Cleanup(srv.Close)
	id := identity.Identity{TenantID: "ref-tenant", UserID: "ref-user", SessionID: "ref-session"}
	quad := identity.Quadruple{Identity: id}
	htSeedHeavyTurn(t, rig.memory, id, htConsoleBand)
	view, err := rig.memory.Inspect(t.Context(), quad)
	if err != nil || len(view.Items) != 1 {
		t.Fatalf("inspect: %v (%d items)", err, len(view.Items))
	}
	item := view.Items[0]
	status, body := memorySourcePost(t, srv.URL, "/v1/memory/get", id, prototypes.MemoryGetRequest{Key: item.Key})
	if status != http.StatusOK {
		t.Fatalf("memory.get: %d %s", status, body)
	}
	var detail prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Detail.ValueArtifact == nil || len(detail.Detail.Value) != 0 {
		t.Fatal("heavy memory was not referenced")
	}
	ref := detail.Detail.ValueArtifact
	scope := prototypes.ArtifactScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	request := prototypes.ArtifactsGetRequest{Scope: scope, ID: ref.ID, Offset: 7, MaxBytes: 83}
	status, body = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
	if status != http.StatusOK {
		t.Fatalf("artifacts.get: %d %s", status, body)
	}
	var got prototypes.ArtifactsGetResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.ReturnedBytes != 83 || got.Offset != 7 || got.TotalSizeBytes != int64(len(item.Value)) || !bytes.Equal(got.Content, item.Value[7:90]) || got.Ref.SHA256 != ref.SHA256 {
		t.Fatal("source read did not preserve exact bytes and bounded-window metadata")
	}
	stored, err := rig.artifact.List(t.Context(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})
	if err != nil || len(stored) != 0 {
		t.Fatalf("independent memory copies: %d, %v", len(stored), err)
	}
	for _, foreign := range []identity.Identity{
		{TenantID: "foreign", UserID: id.UserID, SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: "foreign", SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: id.UserID, SessionID: "foreign"},
	} {
		status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", foreign, request)
		if status != http.StatusForbidden {
			t.Fatalf("forged body identity accepted: %d", status)
		}
		ownRequest := request
		ownRequest.Scope = prototypes.ArtifactScope{Tenant: foreign.TenantID, User: foreign.UserID, Session: foreign.SessionID}
		status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", foreign, ownRequest)
		if status != http.StatusNotFound {
			t.Fatalf("foreign source lookup: %d", status)
		}
	}
	status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get_ref", id, prototypes.ArtifactsGetRefRequest{Scope: scope, ID: ref.ID})
	if status != http.StatusNotImplemented {
		t.Fatalf("source reference allowed presign: %d", status)
	}
	if _, err := rig.memory.Delete(t.Context(), quad, item.Key); err != nil {
		t.Fatal(err)
	}
	status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
	if status != http.StatusNotFound {
		t.Fatalf("deleted source remained readable: %d", status)
	}
	status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get_ref", id, prototypes.ArtifactsGetRefRequest{Scope: scope, ID: ref.ID})
	if status != http.StatusNotFound {
		t.Fatalf("deleted source remained presignable: %d", status)
	}

	// Even a blob created below the Protocol in the reserved namespace must
	// never be a fallback when the memory source is unavailable.
	// The in-memory artifact driver appends '_' plus twelve digest characters.
	// Keep the forged ID at the source ID's full length, so rejection exercises
	// missing-source resolution rather than only the short-ID validation branch.
	forged, err := rig.artifact.PutText(t.Context(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, "not a memory source", artifacts.PutOpts{Namespace: ref.ID[:len(ref.ID)-13]})
	if err != nil {
		t.Fatal(err)
	}
	request.ID = forged.ID
	status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
	if status != http.StatusNotFound {
		t.Fatalf("reserved reference fell through to blob storage: %d", status)
	}
	status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.put", id, prototypes.ArtifactsPutRequest{Scope: scope, Bytes: []byte("forged"), Opts: prototypes.ArtifactsPutOpts{Namespace: "memory_source"}})
	if status != http.StatusBadRequest {
		t.Fatalf("reserved namespace upload accepted: %d", status)
	}
	if err := rig.memory.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	request.ID = ref.ID
	status, body = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
	if status != http.StatusInternalServerError || bytes.Contains(body, item.Value[:20]) {
		t.Fatalf("memory read failure was hidden or leaked source bytes: %d", status)
	}
}

func TestMemorySourceReference_HTTPConcurrentIsolation(t *testing.T) {
	rig := newHeavyThresholdRig(t, 0)
	srv := httptest.NewServer(rig.mux)
	t.Cleanup(srv.Close)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id := identity.Identity{TenantID: fmt.Sprintf("tenant-%d", i%4), UserID: fmt.Sprintf("user-%d", i%8), SessionID: fmt.Sprintf("session-%d", i)}
			quad := identity.Quadruple{Identity: id}
			if _, err := rig.memory.Put(t.Context(), quad, memory.ConversationTurn{UserMessage: id.SessionID, AssistantResponse: "private answer"}); err != nil {
				t.Error(err)
				return
			}
			view, err := rig.memory.Inspect(t.Context(), quad)
			if err != nil || len(view.Items) != 1 {
				t.Errorf("inspection: %v", err)
				return
			}
			ref, err := memory.SourceReference(quad, view.Items[0])
			if err != nil {
				t.Error(err)
				return
			}
			request := prototypes.ArtifactsGetRequest{Scope: prototypes.ArtifactScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, ID: ref.ID}
			status, body := memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
			var got prototypes.ArtifactsGetResponse
			if status != http.StatusOK || json.Unmarshal(body, &got) != nil || !bytes.Equal(got.Content, view.Items[0].Value) {
				t.Errorf("own read failed: %d", status)
				return
			}
			id.SessionID = fmt.Sprintf("session-%d", (i+1)%128)
			request.Scope.Session = id.SessionID
			status, _ = memorySourcePost(t, srv.URL, "/v1/control/artifacts.get", id, request)
			if status != http.StatusNotFound {
				t.Errorf("cross-session source read: %d", status)
			}
		})
	}
	wg.Wait()
}
