package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

func artifactClaims(mode, id string, max float64) jwt.MapClaims {
	claims := validClaims(fixedNow)
	claims["scopes"] = []string{}
	claims["session_reach"] = []string{"sess-01HX0000000000000000000000"}
	claims[auth.ArtifactTransferClaim] = map[string]any{"mode": mode, "id": id, "max_bytes": max}
	return claims
}

func TestSignedArtifactTransferRejectsWideningAndOtherMethods(t *testing.T) {
	v, key := newRSValidator(t, fixedNow)
	good := artifactClaims("read", "upload_0123456789ab", 4096)
	verified, err := v.Validate(context.Background(), signRS256(t, key, good, "k1"))
	if err != nil || verified.ArtifactTransfer == nil || verified.ArtifactTransfer.ID != "upload_0123456789ab" {
		t.Fatalf("signed transfer absent: %+v, %v", verified.ArtifactTransfer, err)
	}
	for _, change := range []func(jwt.MapClaims){
		func(c jwt.MapClaims) { c["scopes"] = []string{"admin"} },
		func(c jwt.MapClaims) { c["session_reach"] = []string{"other-session"} },
		func(c jwt.MapClaims) { delete(c, "session_reach") },
		func(c jwt.MapClaims) { c["agent_reach"] = []string{"agent-a"} },
		func(c jwt.MapClaims) {
			c[auth.ArtifactTransferClaim] = map[string]any{"mode": "read", "id": "*", "max_bytes": 4096}
		},
		func(c jwt.MapClaims) {
			c[auth.ArtifactTransferClaim] = map[string]any{"mode": "write", "id": "", "max_bytes": 4096}
		},
	} {
		claims := artifactClaims("read", "upload_0123456789ab", 4096)
		change(claims)
		if _, err := v.Validate(context.Background(), signRS256(t, key, claims, "k1")); !errors.Is(err, auth.ErrArtifactTransferMalformed) {
			t.Fatalf("widened transfer accepted: %v", err)
		}
	}
	called := false
	handler := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
	for _, path := range []string{"/v1/control/start", "/v1/control/tasks.get", "/v1/control/artifacts.list", "/v1/control/artifacts.get_ref", "/v1/control/artifacts.put", "/v1/tools/list"} {
		called = false
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("Authorization", "Bearer "+signRS256(t, key, good, "k1"))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if called || w.Code != http.StatusForbidden {
			t.Fatalf("artifact-only token accessed %s: status=%d called=%t", path, w.Code, called)
		}
	}
	for _, path := range []string{"/v1/control/artifacts.get"} {
		called = false
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("Authorization", "Bearer "+signRS256(t, key, good, "k1"))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if !called || w.Code != http.StatusNoContent {
			t.Fatalf("artifact read method refused %s: status=%d called=%t", path, w.Code, called)
		}
	}
	called = false
	wrongSession := httptest.NewRequest(http.MethodPost, "/v1/control/artifacts.get", nil)
	wrongSession.Header.Set("Authorization", "Bearer "+signRS256(t, key, good, "k1"))
	wrongSession.Header.Set(auth.HeaderSession, "another-session")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, wrongSession)
	if called || w.Code != http.StatusForbidden {
		t.Fatalf("artifact transfer crossed session: status=%d called=%t", w.Code, called)
	}
	write := artifactClaims("write", "upload-operation", 4096)
	for _, path := range []string{"/v1/control/start", "/v1/control/tasks.get", "/v1/control/artifacts.list", "/v1/control/artifacts.get", "/v1/tools/list"} {
		called = false
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("Authorization", "Bearer "+signRS256(t, key, write, "k1"))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if called || w.Code != http.StatusForbidden {
			t.Fatalf("artifact upload token accessed %s: status=%d called=%t", path, w.Code, called)
		}
	}
	called = false
	put := httptest.NewRequest(http.MethodPost, "/v1/control/artifacts.put", nil)
	put.Header.Set("Authorization", "Bearer "+signRS256(t, key, write, "k1"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, put)
	if !called || w.Code != http.StatusNoContent {
		t.Fatalf("artifact upload token refused put: status=%d called=%t", w.Code, called)
	}
}
