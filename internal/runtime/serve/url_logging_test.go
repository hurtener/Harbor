package serve

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestBoot_HTTPErrorLogsRedactURLQueries(t *testing.T) {
	opts := baseOptions(t)
	buf := &stderrSyncWriter{b: &strings.Builder{}}
	opts.Logger = slog.New(slog.NewJSONHandler(buf, nil))
	cfg, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg = []byte(strings.Replace(string(cfg), "bind_addr: 127.0.0.1:0", "bind_addr: 127.0.0.1:0\n  debug_addr: 127.0.0.1:0", 1))
	if err := os.WriteFile(opts.ConfigPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	h := bootTest(t, context.Background(), opts)
	if h.server.ErrorLog == nil || h.debugServer == nil || h.debugServer.ErrorLog == nil {
		t.Fatal("HTTP servers must route standard-library errors through the safe logger")
	}
	h.server.ErrorLog.Print("request /v1/events?access_token=server-url-sentinel&type=task.completed")
	h.debugServer.ErrorLog.Print("request /debug/pprof?key=debug-url-sentinel&seconds=1")
	buf.mu.Lock()
	out := buf.b.String()
	buf.mu.Unlock()
	if strings.Contains(out, "server-url-sentinel") || strings.Contains(out, "debug-url-sentinel") {
		t.Fatalf("HTTP error log leaked URL secret: %s", out)
	}
	for _, want := range []string{"REDACTED", "task.completed", "seconds=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic %q missing: %s", want, out)
		}
	}
}
