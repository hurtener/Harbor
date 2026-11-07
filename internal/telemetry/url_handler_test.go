package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/telemetry"
)

func TestURLRedactingHandler_PreservesDiagnosticsAndGroups(t *testing.T) {
	buf := &lockedBuf{}
	logger := slog.New(telemetry.URLRedactingHandler(slog.NewJSONHandler(buf, nil)))
	logger.With("bound_url", "/p?key=bound-url-sentinel&session=s1").WithGroup("request").Error(
		"GET /v1/events?token=message-url-sentinel&type=task.completed",
		"status", 503, "method", "GET", "duration_ms", 17,
		slog.Group("details", "path", "/p?%61ccess_token=nested-url-sentinel&resume_seq=42"),
		slog.Any("error", &url.Error{Op: "Get", URL: "https://host/p?id_token=error-url-sentinel", Err: errors.New("connection refused")}),
		slog.Any("url_value", &url.URL{Scheme: "https", Host: "host", RawQuery: "sig=stringer-url-sentinel"}),
		"malformed", "/p?code=malformed-url-sentinel%ZZ")
	logger.Info("structured", "details", map[string]any{
		"items": []any{struct{ URL string }{URL: "/p?code=structured-url-sentinel&session=s2"}},
		"err":   errors.New("Get https://host/p?key=structured-error-sentinel: refused"),
	})
	out := buf.String()
	for _, secret := range []string{"bound-url-sentinel", "message-url-sentinel", "nested-url-sentinel", "error-url-sentinel", "stringer-url-sentinel", "malformed-url-sentinel", "structured-url-sentinel", "structured-error-sentinel"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked: %s", secret, out)
		}
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.Split(out, "\n")[0]), &record); err != nil {
		t.Fatal(err)
	}
	request := record["request"].(map[string]any)
	if request["status"] != float64(503) || request["method"] != "GET" || request["duration_ms"] != float64(17) {
		t.Fatalf("request diagnostics changed: %v", request)
	}
	for _, want := range []string{"REDACTED", "task.completed", "session=s1", "resume_seq=42", "connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic %q missing: %s", want, out)
		}
	}
}

func TestURLRedactingHandler_UnsafeDepthFailsClosed(t *testing.T) {
	buf := &lockedBuf{}
	logger := slog.New(telemetry.URLRedactingHandler(slog.NewJSONHandler(buf, nil)))
	cyclic := map[string]any{"url": "/p?token=cyclic-url-sentinel"}
	cyclic["self"] = cyclic
	logger.Info("deep", "details", cyclic)
	if out := buf.String(); strings.Contains(out, "cyclic-url-sentinel") || !strings.Contains(out, "log emission blocked") {
		t.Fatalf("deep value was not blocked: %s", out)
	}
}

func TestURLRedactingHandler_QuotedQueriesInMessagesAndErrors(t *testing.T) {
	buf := &lockedBuf{}
	logger := slog.New(telemetry.URLRedactingHandler(slog.NewJSONHandler(buf, nil)))
	logger.Error(`GET /p?token='message-quoted-sentinel'&type=task.completed`,
		"error", errors.New(`Get "https://host/p?token=prefix'error-quoted-sentinel": failed`),
		"details", map[string]any{"items": []any{
			errors.New(`GET /p?access_token='nested-quoted-sentinel with whitespace'&session=s1`),
			`GET /p?token="nested-doublequoted-sentinel unclosed with whitespace`,
		}})
	out := buf.String()
	for _, secret := range []string{"message-quoted-sentinel", "error-quoted-sentinel", "nested-quoted-sentinel", "nested-doublequoted-sentinel", "with whitespace"} {
		if strings.Contains(out, secret) {
			t.Fatalf("quoted URL secret leaked: %s", out)
		}
	}
	for _, want := range []string{"REDACTED", "task.completed", "session=s1", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic %q missing: %s", want, out)
		}
	}
}

func TestURLRedactingHandler_ConcurrentReuse(t *testing.T) {
	buf := &lockedBuf{}
	logger := slog.New(telemetry.URLRedactingHandler(slog.NewJSONHandler(buf, nil)))
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger.With("session", fmt.Sprintf("s-%d", i)).InfoContext(ctx, "request", "path", fmt.Sprintf("/v1/events?access_token=concurrent-url-sentinel-%d&type=task.completed", i))
		})
	}
	wg.Wait()
	out := buf.String()
	if strings.Contains(out, "concurrent-url-sentinel") {
		t.Fatal("concurrent URL secret leaked")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 100 {
		t.Fatalf("got %d records, want 100", len(lines))
	}
	seen := make(map[string]bool)
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		session := record["session"].(string)
		if seen[session] {
			t.Fatalf("duplicate session %s", session)
		}
		seen[session] = true
	}
}

func TestURLRedactingHandler_HTTPPanicLog(t *testing.T) {
	buf := &lockedBuf{}
	handler := telemetry.URLRedactingHandler(slog.NewJSONHandler(buf, nil))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		panic(r.RequestURI)
	}))
	server.Config.ErrorLog = slog.NewLogLogger(handler, slog.LevelError)
	server.Start()
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/v1/events?access_token=panic-url-sentinel&type=task.completed")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("expected panic to close the connection")
	}
	out := buf.String()
	if strings.Contains(out, "panic-url-sentinel") || !strings.Contains(out, "REDACTED") || !strings.Contains(out, "task.completed") {
		t.Fatalf("panic log was not safely preserved: %s", out)
	}
}

func TestSlogBridge_RedactsURLQueries(t *testing.T) {
	logger, buf := newLogger(t)
	logger.Slog().Error("stream failed: /v1/events?access_token=canonical-url-sentinel&type=task.completed", "path", "/p?code=canonical-code-sentinel&session=s1")
	out := buf.String()
	if strings.Contains(out, "canonical-url-sentinel") || strings.Contains(out, "canonical-code-sentinel") {
		t.Fatalf("URL secret leaked through canonical logger: %s", out)
	}
	if !strings.Contains(out, "REDACTED") || !strings.Contains(out, "session=s1") {
		t.Fatalf("diagnostics missing: %s", out)
	}
}
