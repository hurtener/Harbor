// cmd/harbor/cmd_console_test.go — Phase 73m (D-129) `harbor console`
// subcommand unit tests. Covers the subcommand wiring (cobra command
// shape + flags), the embedded-asset handler (SPA fallback, embed.FS
// serving), and the D-091 binding rule (`harbor dev --help` advertises
// no console-serving flag).

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// TestNewConsoleCmd_Shape — the `console` subcommand exists with the
// documented flag surface.
func TestNewConsoleCmd_Shape(t *testing.T) {
	cmd := newConsoleCmd()
	if cmd.Use != "console" {
		t.Errorf("cmd.Use = %q, want console", cmd.Use)
	}
	for _, flag := range []string{flagConsolePort, flagConsoleConfig, flagConsoleBind} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("console cmd missing --%s flag", flag)
		}
	}
}

// TestConsoleCmd_RegisteredOnRoot — `harbor console` resolves through
// the root command tree.
func TestConsoleCmd_RegisteredOnRoot(t *testing.T) {
	root := NewRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "console" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("console subcommand not registered on the root command")
	}
}

// TestDevHelp_DoesNotAdvertiseConsole — the D-091 binding rule:
// `harbor dev` must NOT advertise any console-serving flag. The Console
// build is served EXCLUSIVELY by `harbor console`.
func TestDevHelp_DoesNotAdvertiseConsole(t *testing.T) {
	cmd := newDevCmd()
	// `harbor dev` may MENTION the console in prose, but it must not
	// expose a flag that serves it. Assert no flag name contains
	// "console".
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if strings.Contains(strings.ToLower(f.Name), "console") {
			t.Errorf("harbor dev advertises a console-related flag %q — D-091 forbids serving the Console from harbor dev", f.Name)
		}
	})
}

// TestConsoleAssetHandler_ServesIndex — the embedded asset handler
// serves index.html at `/`.
func TestConsoleAssetHandler_ServesIndex(t *testing.T) {
	h, err := newConsoleAssetHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newConsoleAssetHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Harbor Console") {
		t.Errorf("GET / body does not contain the Console shell; got %q", rec.Body.String())
	}
}

// TestConsoleAssetHandler_SPAFallback — an unknown path (a SvelteKit
// client-side route) serves the index.html SPA shell, not a 404.
func TestConsoleAssetHandler_SPAFallback(t *testing.T) {
	h, err := newConsoleAssetHandler(nil)
	if err != nil {
		t.Fatalf("newConsoleAssetHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings (SPA route) status = %d, want 200 (index fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Harbor Console") {
		t.Error("SPA fallback did not serve the index shell")
	}
}

// TestConsoleAssetHandler_RejectsNonGET — the asset handler accepts
// GET / HEAD only.
func TestConsoleAssetHandler_RejectsNonGET(t *testing.T) {
	h, err := newConsoleAssetHandler(nil)
	if err != nil {
		t.Fatalf("newConsoleAssetHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / status = %d, want 405", rec.Code)
	}
}

// TestConsoleAssets_EmbedResolves — the embedded FS resolves. The embed
// directive (`//go:embed all:consoledist`) guarantees the directory
// exists (the committed `.gitkeep` keeps it present on a bare
// checkout); this pins that the sub-FS resolves without error.
func TestConsoleAssets_EmbedResolves(t *testing.T) {
	if _, err := consoleAssets(); err != nil {
		t.Fatalf("consoleAssets: %v", err)
	}
}
