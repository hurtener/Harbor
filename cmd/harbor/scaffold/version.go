package scaffold

import (
	"regexp"
	"runtime/debug"
)

// FallbackModuleVersion is the `github.com/hurtener/Harbor` module
// version the generated `go.mod` requires when the scaffolding binary
// cannot name its own release version — an un-stamped source build
// (`go build ./cmd/harbor`, `go run`, `go test`), whose version string
// is the "v0.0.0-dev" sentinel and whose build info reports "(devel)".
//
// It MUST name a version published to the module proxy, so the
// generated project builds out of the box (`go mod tidy && go build`)
// with no manual edit. BUMP IT WHEN A NEW HARBOR RELEASE IS TAGGED —
// a stale value only costs the scaffolded project an older Harbor, but
// a value that was never tagged breaks the generated project's build.
//
// The value is not only what a scaffolded `go.mod` requires: the CLI
// reports it as `harbor --version` on any un-stamped source build, so a
// stale pin misreports the running binary as well. `scripts/drift-audit.sh`
// checks it against the PUBLISHED release tags — read from the remote, not
// from the working branch's own CHANGELOG, which a release-heal merge can
// leave behind without the pin noticing.
//
// The scaffold golden fixtures pin this value (an un-stamped `go test`
// binary always resolves to it), so a bump regenerates them:
//
//	go test ./cmd/harbor -run TestScaffold_Golden -update
const FallbackModuleVersion = "v1.25.0"

// releaseVersionRE matches the version strings that name a real,
// proxy-resolvable Harbor release tag: `vX.Y.Z`, optionally with a
// conventional pre-release suffix (`-rc.1`, `-beta.2`).
//
// It deliberately REJECTS the shapes that are not fetchable module
// versions even though they are syntactically semver-ish: the
// "v0.0.0-dev" un-stamped sentinel, and the `git describe` derivatives
// the release script produces off an untagged/dirty tree
// (`v1.13.0-4-gdeadbee`, `...-dirty`). Those fall back to
// FallbackModuleVersion rather than emitting a `require` line no
// proxy can resolve.
//
// CAVEAT for a future major: this accepts `v2.0.0` and above, but Go's
// semantic-import-versioning rule requires the module PATH to carry a
// `/v2` suffix at v2+, and `go.mod.tmpl` hardcodes the unsuffixed
// `github.com/hurtener/Harbor`. A v2 cut must update the template's
// require path (and Harbor's own module path) in the same change, or
// the generated project will not resolve.
var releaseVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)[.0-9A-Za-z-]*)?$`)

// resolveModuleVersion picks the Harbor module version the generated
// `go.mod` requires, in priority order:
//
//  1. binaryVersion — the scaffolding binary's own release version, as
//     stamped at link time by the release build. A release artifact
//     knows exactly which Harbor it ships.
//  2. The main module's version from the binary's embedded build info —
//     the `go install github.com/hurtener/Harbor/cmd/harbor@vX.Y.Z`
//     path, where nothing is link-stamped but the go command records
//     the module version.
//  3. FallbackModuleVersion — an un-stamped source build. The last
//     published release is the honest best guess, and it keeps the
//     generated project buildable.
func resolveModuleVersion(binaryVersion string) string {
	if releaseVersionRE.MatchString(binaryVersion) {
		return binaryVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info != nil {
		if releaseVersionRE.MatchString(info.Main.Version) {
			return info.Main.Version
		}
	}
	return FallbackModuleVersion
}
