package protocolclient_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExternalModule_CompilesCuratedProtocolClient(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	dir := t.TempDir()
	goMod := "module external.example/client\n\ngo 1.26\n\nrequire github.com/hurtener/Harbor v0.0.0\nreplace github.com/hurtener/Harbor => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	source := `package external

import (
	"context"
	client "github.com/hurtener/Harbor/sdk/protocolclient"
)

func compile(ctx context.Context) error {
	identity := client.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	c, err := client.New(client.Connection{
		BaseURL: "http://127.0.0.1:18080",
		Token: client.TokenSourceFunc(func(_ context.Context, requested client.IdentityScope) (string, error) {
			_ = requested
			return "compile-only", nil
		}),
		Identity: identity,
	})
	if err != nil { return err }
	var _ client.Client = c
	_ = client.ExternalGrantReadiness{Supported: true, Mode: "disabled"}
	clone := c.WithSession("other")
	_, _ = clone.RuntimeInfo(ctx)
	_, _ = clone.RuntimeHealth(ctx)
	_, _ = clone.Start(ctx, client.StartRequest{Query: "hello"})
	_, _ = clone.TasksList(ctx, client.TaskListRequest{})
	_, _ = clone.TasksGet(ctx, client.TaskGetRequest{ID: "task"})
	_, _ = clone.SessionsList(ctx, client.SessionsListRequest{})
	_, _ = clone.SessionsInspect(ctx, client.SessionsInspectRequest{SessionID: "other"})
	_, _ = clone.SessionsSetTitle(ctx, client.SessionsSetTitleRequest{SessionID: "other", Title: "title"})
	_, _ = clone.SessionsDelete(ctx)
	_, _ = clone.StateHistory(ctx, client.StateHistoryRequest{})
	_, _ = clone.PauseList(ctx, client.PauseListRequest{})
	_, _ = clone.Control(ctx, client.MethodCancel, client.ControlRequest{Identity: client.IdentityScope{Run: "run"}})
	_, _ = clone.ArtifactsPut(ctx, client.ArtifactsPutRequest{Bytes: []byte("x")})
	_, _ = clone.ArtifactsList(ctx, client.ArtifactsListRequest{})
	stream, _ := clone.Subscribe(ctx, client.StreamOptions{LastEventID: "1"})
	if stream != nil { _ = stream.Close() }
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "client_test.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "test", "-mod=mod", "./...")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("external module compile: %v\n%s", err, output)
	}
}
