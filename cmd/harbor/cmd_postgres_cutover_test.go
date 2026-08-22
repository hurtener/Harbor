package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPostgresCutover_CopyRequiresFreezeBeforeOpeningDSNs(t *testing.T) {
	root := NewRootCmd()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"postgres", "cutover", "--mode", "copy", "--source", "postgres://source:5432/harbor", "--destination", "postgres://destination:5432/harbor", "--json"})
	if err := root.Execute(); err == nil {
		t.Fatal("copy without freeze acknowledgement succeeded")
	}
	if !strings.Contains(stderr.String(), `"code":"postgres_cutover_refused"`) || !strings.Contains(stderr.String(), "freeze") {
		t.Fatalf("stderr=%q, want structured freeze refusal", stderr.String())
	}
}

func TestPostgresCutover_CopyRejectsPgBouncerBeforeOpeningDSNs(t *testing.T) {
	root := NewRootCmd()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"postgres", "cutover", "--mode", "copy", "--freeze-ack", "--source", "postgres://source:5432/harbor", "--destination", "postgres://destination:6432/harbor", "--json"})
	if err := root.Execute(); err == nil {
		t.Fatal("copy through PgBouncer succeeded")
	}
	if !strings.Contains(stderr.String(), `"code":"postgres_cutover_refused"`) || !strings.Contains(stderr.String(), "5432") {
		t.Fatalf("stderr=%q, want direct-5432 refusal", stderr.String())
	}
}
