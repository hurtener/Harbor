// Phase 73m (D-129) — the `harbor console` subcommand boot test.
//
// D-091 pins the Console deployment: the static SvelteKit build is
// baked into `cmd/harbor` via `embed.FS` and served EXCLUSIVELY by the
// `harbor console` subcommand. This test boots the real `harbor console`
// subprocess, asserts:
//
//  1. `harbor console --help` exits 0 (the subcommand exists).
//  2. `harbor console --bind 127.0.0.1:0` boots, prints the bound port,
//     and serves the embedded Console index at `/` (200 OK).
//  3. `/healthz` reports the `console` subcommand label.
//  4. A SvelteKit client-side route (`/settings`) serves the SPA shell
//     (the adapter-static fallback) — never a 404.
//
// It is the §17 wiring test for `harbor console`: it composes the
// production binary (`bin/harbor console`) and asserts the embedded-
// asset surface + the Protocol surface wire together on one port.
//
// The subcommand boots an embedded Runtime with the LLM seam — so the
// test sets `HARBOR_DEV_ALLOW_MOCK=1` (the §13 dev-only escape hatch)
// so the boot does not fail closed on a missing provider.
package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	consoleBuildOnce sync.Once
	consoleBinPath   string
	consoleBuildErr  error
)

// buildHarborForConsole compiles ./bin/harbor once for the console
// boot test.
func buildHarborForConsole(t *testing.T) string {
	t.Helper()
	consoleBuildOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
		if err != nil {
			consoleBuildErr = err
			return
		}
		bin := filepath.Join(root, "bin", "harbor-phase73m-test")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/harbor")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if buildErr := cmd.Run(); buildErr != nil {
			consoleBuildErr = fmt.Errorf("go build harbor: %w: %s", buildErr, stderr.String())
			return
		}
		consoleBinPath = bin
	})
	if consoleBuildErr != nil {
		t.Fatalf("build harbor: %v", consoleBuildErr)
	}
	return consoleBinPath
}

var consoleBoundRe = regexp.MustCompile(`HARBOR_(?:DEV_)?BOUND=([^\s]+)`)

// TestE2E_HarborConsole_HelpExitsZero — `harbor console --help` exits 0.
// The e2e Playwright harness probes exactly this to decide whether the
// subcommand is available.
func TestE2E_HarborConsole_HelpExitsZero(t *testing.T) {
	bin := buildHarborForConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "console", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harbor console --help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "console") {
		t.Errorf("harbor console --help output missing the subcommand name; got:\n%s", out)
	}
}

// TestE2E_HarborConsole_BootsAndServesEmbeddedBuild — the headline E2E:
// `harbor console` boots, serves the embedded Console index at `/`, and
// reports the `console` label on /healthz.
func TestE2E_HarborConsole_BootsAndServesEmbeddedBuild(t *testing.T) {
	bin := buildHarborForConsole(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "console", "--bind", "127.0.0.1:0")
	// The embedded Runtime opens the LLM seam; the dev-only mock escape
	// hatch (§13 / D-089) keeps the test hermetic.
	cmd.Env = append(os.Environ(), "HARBOR_DEV_ALLOW_MOCK=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start harbor console: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	// Read stderr until the bound-port line appears, bounded by a real-
	// time deadline (no sleep-as-synchronisation — CLAUDE.md §17.4).
	baseURL := readConsoleBoundURL(t, stderr)

	// (2) the embedded Console index serves at `/`.
	status, body := consoleGET(t, baseURL+"/")
	if status != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, "Harbor Console") {
		t.Errorf("GET / body does not contain the Console shell; got:\n%s", body)
	}

	// (3) /healthz reports the `console` subcommand label.
	hStatus, hBody := consoleGET(t, baseURL+"/healthz")
	if hStatus != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", hStatus)
	}
	if !strings.Contains(hBody, `"subcommand":"console"`) {
		t.Errorf("/healthz body = %q, want it to carry the console subcommand label", hBody)
	}

	// (4) a SvelteKit client-side route serves the SPA shell, not a 404.
	sStatus, _ := consoleGET(t, baseURL+"/settings")
	if sStatus != http.StatusOK {
		t.Errorf("GET /settings (SPA route) status = %d, want 200 (index fallback)", sStatus)
	}
}

// readConsoleBoundURL reads the child's stderr until the
// `HARBOR_*_BOUND=host:port` line appears, then returns the base URL.
func readConsoleBoundURL(t *testing.T, stderr io.Reader) string {
	t.Helper()
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if m := consoleBoundRe.FindStringSubmatch(acc.String()); m != nil {
					ch <- result{url: "http://" + m[1]}
					return
				}
			}
			if err != nil {
				ch <- result{err: fmt.Errorf("stderr closed before bound line; saw:\n%s", acc.String())}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("harbor console boot: %v", r.err)
		}
		return r.url
	case <-time.After(30 * time.Second):
		t.Fatal("harbor console did not emit a bound-port line within 30s")
		return ""
	}
}

// consoleGET issues a GET and returns the status + body.
func consoleGET(t *testing.T, url string) (int, string) {
	t.Helper()
	// Retry briefly — the listener is bound before serve starts.
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(body)
	}
	t.Fatalf("GET %s: %v", url, lastErr)
	return 0, ""
}
