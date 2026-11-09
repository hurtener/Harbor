package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProvider_AutoFallbackLogHidesEndpointQuery(t *testing.T) {
	srv := newMockServer()
	sse := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return srv.server }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("sessionid") == "" {
			http.Error(w, "streamable transport rejected", http.StatusBadRequest)
			return
		}
		sse.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	p, err := New(Config{
		Name: "fallback-log", URL: server.URL + "/mcp?token=fixture-secret",
		TransportMode: TransportAuto, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err != nil {
		t.Fatalf("auto fallback failed: %v", err)
	}
	if p.SelectedMode() != TransportSSE {
		t.Fatalf("selected mode = %q, want SSE", p.SelectedMode())
	}
	logged := logs.String()
	if !strings.Contains(logged, "auto-fallback streamable-http -> sse") ||
		strings.Contains(logged, "fixture-secret") || strings.Contains(logged, server.URL) {
		t.Fatalf("fallback log exposed endpoint or missed success: %q", logged)
	}
}
