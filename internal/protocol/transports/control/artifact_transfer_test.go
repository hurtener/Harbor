package control_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/artifacts"
	artmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
	statemem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type transferJWTKeys struct{ public *ecdsa.PublicKey }

func (k transferJWTKeys) KeyByID(id string) (crypto.PublicKey, string, error) {
	if id != "fixture" {
		return nil, "", errors.New("unknown fixture key")
	}
	return k.public, "ES256", nil
}

func TestArtifactTransfer_AuthenticatedNativeProtocolRoundTrip(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	grantPublic, grantPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makeRuntime := func(audience string, sender transfer.Sender) (artifacts.ArtifactStore, http.Handler) {
		store, e := statemem.New(config.StateConfig{})
		if e != nil {
			t.Fatal(e)
		}
		arts, e := artmem.New(config.ArtifactsConfig{})
		if e != nil {
			t.Fatal(e)
		}
		bus, e := eventmem.New(config.EventsConfig{MaxSubscribersPerSession: 8, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second}, patterns.New())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			_ = bus.Close(context.Background())
			_ = arts.Close(context.Background())
			_ = store.Close(context.Background())
		})
		svc, e := transfer.New(transfer.Config{LegacyWritersDrained: true, Audience: audience, Epoch: 1, MaxBytes: 1024, Keys: map[string]ed25519.PublicKey{"grant": grantPublic}, Artifacts: arts, State: store, Bus: bus, Sender: sender, Clock: time.Now})
		if e != nil {
			t.Fatal(e)
		}
		surface, e := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{Store: arts, Redactor: patterns.New(), Bus: bus, Clock: time.Now, DriverName: "inmem", MaxBodyBytes: 4096, FetchDefaultMaxBytes: 1024, FetchHardMaxBytes: 4096, Transfer: svc})
		if e != nil {
			t.Fatal(e)
		}
		core, cleanup := newTestSurface(t)
		t.Cleanup(cleanup)
		h, e := control.NewHandler(core, control.WithArtifactsSurface(surface))
		if e != nil {
			t.Fatal(e)
		}
		v, e := auth.NewValidator(transferJWTKeys{&key.PublicKey}, auth.WithAudience(audience), auth.WithRedactor(patterns.New()))
		if e != nil {
			t.Fatal(e)
		}
		router := http.NewServeMux()
		router.Handle(control.RoutePattern, h)
		authorized := auth.Middleware(v)(router)
		edge := svc.Handler()
		return arts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == transfer.ImportPath {
				edge.ServeHTTP(w, r)
				return
			}
			authorized.ServeHTTP(w, r)
		})
	}
	targetArts, targetHandler := makeRuntime("target", nil)
	target := httptest.NewServer(targetHandler)
	defer target.Close()
	sender, err := transfer.NewHTTPSender(transfer.HTTPConfig{Peers: map[string]string{"target": target.URL}, Timeout: time.Second, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	sourceArts, sourceHandler := makeRuntime("source", sender)
	source := httptest.NewServer(sourceHandler)
	defer source.Close()
	src := types.ArtifactTransferEndpoint{Audience: "source", Tenant: "tenant", User: "source-owner", Session: "source-session", Epoch: 1}
	dst := types.ArtifactTransferEndpoint{Audience: "target", Tenant: "tenant", User: "recipient", Session: "recipient-session", Epoch: 1}
	data := []byte{0, 255, 1, 2, 128}
	ref, err := sourceArts.PutBytes(t.Context(), artifacts.ArtifactScope{TenantID: src.Tenant, UserID: src.User, SessionID: src.Session}, data, artifacts.PutOpts{MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := transfer.Sign(types.ArtifactTransferGrant{Version: 1, KeyID: "grant", TransferID: "native-copy", Purpose: "fixture", Source: src, Destination: dst, ArtifactID: ref.ID, SHA256: ref.SHA256, MimeType: ref.MimeType, SizeBytes: ref.SizeBytes, IssuedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute)}, grantPrivate)
	if err != nil {
		t.Fatal(err)
	}
	token := func(e types.ArtifactTransferEndpoint) string {
		claims := jwt.MapClaims{"aud": e.Audience, "tenant": e.Tenant, "user": e.User, "session": e.Session, "exp": time.Now().Add(time.Minute).Unix()}
		signed := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
		signed.Header["kid"] = "fixture"
		raw, e2 := signed.SignedString(key)
		if e2 != nil {
			t.Fatal(e2)
		}
		return raw
	}
	call := func(base string, m methods.Method, body any, bearer string) (int, types.ArtifactTransferReceipt) {
		raw, e2 := json.Marshal(body)
		if e2 != nil {
			t.Fatal(e2)
		}
		req, e2 := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/control/"+string(m), bytes.NewReader(raw))
		if e2 != nil {
			t.Fatal(e2)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		response, e2 := http.DefaultClient.Do(req)
		if e2 != nil {
			t.Fatal(e2)
		}
		defer func() { _ = response.Body.Close() }()
		var receipt types.ArtifactTransferReceipt
		if response.StatusCode == http.StatusOK {
			if e2 = json.NewDecoder(response.Body).Decode(&receipt); e2 != nil {
				t.Fatal(e2)
			}
		}
		return response.StatusCode, receipt
	}
	scope := func(e types.ArtifactTransferEndpoint) types.ArtifactScope {
		return types.ArtifactScope{Tenant: e.Tenant, User: e.User, Session: e.Session}
	}
	request := types.ArtifactsTransferRequest{Scope: scope(dst), Grant: g}
	if status, _ := call(target.URL, methods.MethodArtifactsPrepareImport, request, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous prepare %d", status)
	}
	if status, _ := call(target.URL, methods.MethodArtifactsPrepareImport, request, token(dst)); status != http.StatusOK {
		t.Fatalf("prepare %d", status)
	}
	request.Scope = scope(src)
	foreign := src
	foreign.User = "unrelated"
	request.Scope = scope(foreign)
	if status, _ := call(source.URL, methods.MethodArtifactsTransfer, request, token(foreign)); status != http.StatusForbidden {
		t.Fatalf("foreign export %d", status)
	}
	request.Scope = scope(src)
	status, receipt := call(source.URL, methods.MethodArtifactsTransfer, request, token(src))
	if status != http.StatusOK || receipt.State != "completed" {
		t.Fatalf("native transfer %d %#v", status, receipt)
	}
	got, found, err := targetArts.Get(t.Context(), artifacts.ArtifactScope{TenantID: dst.Tenant, UserID: dst.User, SessionID: dst.Session}, receipt.DestinationArtifactID)
	if err != nil || !found || !bytes.Equal(got, data) {
		t.Fatalf("recipient bytes %v %v", found, err)
	}
}
