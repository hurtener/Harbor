// buildidentity_test.go — in-package coverage for the build-identity
// resolution (the test binary carries build info, so both fields resolve
// to non-empty values or the documented fallback).
package external

import (
	"errors"
	"testing"
)

func TestBuildIdentity_NeverEmpty(t *testing.T) {
	version, commit := buildIdentity()
	if version == "" {
		t.Error("buildIdentity returned an empty version — runtime.info must never report empty")
	}
	if commit == "" {
		t.Error("buildIdentity returned an empty commit — runtime.info must never report empty")
	}
}

func TestResolveFrameworkIdentity_ExplicitPairIsPreserved(t *testing.T) {
	framework, err := resolveFrameworkIdentity(FrameworkIdentity{
		Version: "v1.28.0",
		Commit:  "a052b0c7ef5323480b88869665e0f971b1496767",
	})
	if err != nil {
		t.Fatalf("resolveFrameworkIdentity: %v", err)
	}
	if framework.Version != "v1.28.0" || framework.Commit != "a052b0c7ef5323480b88869665e0f971b1496767" {
		t.Fatalf("explicit framework identity = %+v", framework)
	}
}

func TestResolveFrameworkIdentity_EmptyPairIsOmitted(t *testing.T) {
	framework, err := resolveFrameworkIdentity(FrameworkIdentity{})
	if err != nil {
		t.Fatalf("resolveFrameworkIdentity: %v", err)
	}
	if framework != (FrameworkIdentity{}) {
		t.Fatalf("empty framework identity = %+v, want omitted", framework)
	}
}

func TestResolveFrameworkIdentity_RejectsPartialPair(t *testing.T) {
	for _, framework := range []FrameworkIdentity{
		{Version: "v1.28.0"},
		{Commit: "a052b0c7ef5323480b88869665e0f971b1496767"},
	} {
		_, err := resolveFrameworkIdentity(framework)
		if !errors.Is(err, ErrFrameworkIdentityIncomplete) {
			t.Fatalf("resolveFrameworkIdentity(%+v) error = %v, want ErrFrameworkIdentityIncomplete", framework, err)
		}
	}
}
