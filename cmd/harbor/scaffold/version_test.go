// cmd/harbor/scaffold/version_test.go — unit tests for the scaffold's
// release-version resolution (version.go): which binary-stamped version
// strings are trusted as real, proxy-resolvable Harbor releases, and
// which fall back to FallbackModuleVersion.

package scaffold

import "testing"

// TestReleaseVersionRE_AcceptsThreeComponentModuleVersions pins the
// accepted release grammar for a MODULE pin: the canonical
// three-component semver form (`v1.28.0`, `v1.26.12`) with an optional
// conventional alpha/beta/rc pre-release suffix. A stamped binary
// carrying any of these must resolve, never fall back. Two-component
// forms are deliberately absent — they are artifact-release tags, not
// proxy-resolvable module versions (see TestReleaseVersionRE_RejectsTwoComponentArtifactTags).
func TestReleaseVersionRE_AcceptsThreeComponentModuleVersions(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"v1.28.0",      // three-component patch form
		"v1.26.12",     // the current three-component fallback pin
		"v1.28.0-rc.1", // prerelease on three components
		"v2.0.0-rc.1",  // major prerelease
		"v1.0.0-alpha.2",
		"v1.0.0-beta",
	} {
		if !releaseVersionRE.MatchString(v) {
			t.Errorf("releaseVersionRE rejected %q — a stamped binary must resolve it, not fall back", v)
		}
	}
}

// TestReleaseVersionRE_RejectsTwoComponentArtifactTags pins the
// artifact-vs-module split at the scaffold seam: Harbor's GitHub
// artifact releases are tagged two-component (`v1.27`, `v1.28`) and
// the runtime DISPLAY accepts those stamps, but the module proxy does
// not resolve them, so the scaffold must never emit a `require` line
// for one. A v1.28-stamped release binary therefore scaffolds
// FallbackModuleVersion, not v1.28.
func TestReleaseVersionRE_RejectsTwoComponentArtifactTags(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"v1.27",      // two-component artifact tag
		"v1.28",      // two-component artifact tag — the planned release stamp
		"v1.28-rc.1", // two-component prerelease artifact tag
	} {
		if releaseVersionRE.MatchString(v) {
			t.Errorf("releaseVersionRE accepted %q — a two-component artifact tag is not a proxy-resolvable module version and must fall back", v)
		}
	}
}

// TestReleaseVersionRE_RejectsNonReleaseShapes pins the rejected shapes:
// the un-stamped sentinel, git-describe derivatives, malformed dotted
// strings, and anything that is not a fetchable module version. Those
// must fall back to FallbackModuleVersion.
func TestReleaseVersionRE_RejectsNonReleaseShapes(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"",                   // empty
		"v0.0.0-dev",         // un-stamped sentinel
		"v1.13.0-4-gdeadbee", // git-describe derivative
		"v1.13.0-dirty",      // dirty-tree derivative
		"v1.27.0.1",          // four-component dotted string
		"v1",                 // one-component
		"1.28.0",             // no v prefix
		"v1.28.0.",           // trailing dot
		"v1..28.0",           // empty minor
		"v1.2.3+build",       // build metadata is not a module version
		"v1.28.0-pre",        // non-conventional prerelease
		"v1.28.0-",           // trailing prerelease dash
	} {
		if releaseVersionRE.MatchString(v) {
			t.Errorf("releaseVersionRE accepted %q — it is not a proxy-resolvable Harbor module version and must fall back", v)
		}
	}
}

// TestResolveModuleVersion_StampedReleaseOrFallback pins the resolution
// priority: a stamped three-component module version (with or without a
// conventional prerelease) names the generated go.mod's require line
// as-is; every two-component artifact tag, malformed / non-release
// shape falls back to FallbackModuleVersion.
func TestResolveModuleVersion_StampedReleaseOrFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		binaryVersion string
		want          string
	}{
		{"three-component patch", "v1.28.0", "v1.28.0"},
		{"three-component prerelease", "v1.28.0-rc.1", "v1.28.0-rc.1"},
		{"major prerelease", "v2.0.0-rc.1", "v2.0.0-rc.1"},
		{"alpha prerelease", "v1.0.0-alpha.1", "v1.0.0-alpha.1"},
		{"beta prerelease", "v1.0.0-beta", "v1.0.0-beta"},
		{"two-component artifact tag falls back", "v1.28", FallbackModuleVersion},
		{"two-component prerelease artifact tag falls back", "v1.28-rc.1", FallbackModuleVersion},
		{"un-stamped sentinel falls back", "v0.0.0-dev", FallbackModuleVersion},
		{"git-describe derivative falls back", "v1.13.0-4-gdeadbee", FallbackModuleVersion},
		{"four-component falls back", "v1.27.0.1", FallbackModuleVersion},
		{"empty falls back", "", FallbackModuleVersion},
		{"one-component falls back", "v1", FallbackModuleVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveModuleVersion(tc.binaryVersion); got != tc.want {
				t.Errorf("resolveModuleVersion(%q) = %q, want %q", tc.binaryVersion, got, tc.want)
			}
		})
	}
}
