package sessionpicker

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestModel_GenerationScopeAndSelection(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "one"}
	m := New(id, "one")
	var old, current Request
	m, old = m.Search("a")
	m, current = m.Search("b")
	if m.IsCurrent(old) || !m.IsCurrent(current) {
		t.Fatal("request generation fence is not observable to dialog consumers")
	}
	m = m.Apply(old, types.SessionsListResponse{Rows: []types.SessionRow{{SessionID: "stale"}}})
	if len(m.Rows()) != 0 {
		t.Fatal("stale search applied")
	}
	m = m.Apply(current, types.SessionsListResponse{Rows: []types.SessionRow{{SessionID: "two", LastActivityAt: time.Now()}}})
	target, ok := m.Select("two")
	if !ok || target.Session != "two" || target.Tenant != "t" {
		t.Fatalf("target=%#v %v", target, ok)
	}
}

func TestModel_CrossScopeMissingSelectionAndCurrent(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "one"}
	m, request := New(id, "one").Search("")
	foreign := request
	foreign.Identity.User = "other"
	m = m.Apply(foreign, types.SessionsListResponse{Rows: []types.SessionRow{{SessionID: "foreign"}}})
	if len(m.Rows()) != 0 {
		t.Fatal("foreign scope applied")
	}
	m = m.SetCurrent("two")
	if _, ok := m.Select("missing"); ok {
		t.Fatal("missing selection accepted")
	}
}
