package artifactcontent

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
)

func TestWitness_ZeroJSONAndForeignInvocationCannotAuthorize(t *testing.T) {
	scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "first-writer"}
	ctx, err := WithInvocation(context.Background(), strings.Repeat("a", 64), "current-task", "native", scope)
	if err != nil {
		t.Fatal(err)
	}
	var empty Witness
	if empty.Present() {
		t.Fatal("zero witness present")
	}
	if _, err = empty.References(ctx); err == nil {
		t.Fatal("zero witness authorized")
	}
	w := Witness{binding: invocationBinding{ID: strings.Repeat("a", 64), TaskID: "current-task", Descriptor: "native", Scope: scope}}
	if _, err = w.References(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = w.References(ForDescriptor(ctx, "helper")); err == nil {
		t.Fatal("helper inherited direct witness")
	}
	for _, change := range []string{"invocation", "task", "session", "user", "tenant"} {
		copy := scope
		id := strings.Repeat("a", 64)
		task := "current-task"
		switch change {
		case "invocation":
			id = strings.Repeat("b", 64)
		case "task":
			task = "other"
		case "session":
			copy.SessionID = "other"
		case "user":
			copy.UserID = "other"
		case "tenant":
			copy.TenantID = "other"
		}
		foreign, err := WithInvocation(context.Background(), id, task, "native", copy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.References(foreign); err == nil {
			t.Fatalf("%s forged witness accepted", change)
		}
	}
}
