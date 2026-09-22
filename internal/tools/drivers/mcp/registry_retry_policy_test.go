package mcp

import (
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestRegistry_ExplicitEmptyRetryOnIsNotDefaulted(t *testing.T) {
	r := NewRegistry()
	policy := tools.ToolPolicy{RetryOn: []tools.ErrorClass{}}
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider:  &stubProvider{id: "srv", toolNames: []string{"read"}},
		Transport: "stdio", Policy: policy,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := r.GetServer(idCtx(t), "srv")
	if err != nil {
		t.Fatal(err)
	}
	if view.Policy.RetryOn == nil || len(view.Policy.RetryOn) != 0 {
		t.Fatalf("registry widened explicit no-retry policy: %+v", view.Policy)
	}
}
