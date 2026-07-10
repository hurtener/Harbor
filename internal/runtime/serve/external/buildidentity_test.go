// buildidentity_test.go — in-package coverage for the build-identity
// resolution (the test binary carries build info, so both fields resolve
// to non-empty values or the documented fallback).
package external

import "testing"

func TestBuildIdentity_NeverEmpty(t *testing.T) {
	version, commit := buildIdentity()
	if version == "" {
		t.Error("buildIdentity returned an empty version — runtime.info must never report empty")
	}
	if commit == "" {
		t.Error("buildIdentity returned an empty commit — runtime.info must never report empty")
	}
}
