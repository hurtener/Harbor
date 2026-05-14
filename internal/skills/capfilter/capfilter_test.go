package capfilter_test

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/skills/capfilter"
)

func TestBuildSet_EmptyInputYieldsNil(t *testing.T) {
	t.Parallel()
	if got := capfilter.BuildSet(nil); got != nil {
		t.Errorf("BuildSet(nil) = %v, want nil", got)
	}
	if got := capfilter.BuildSet([]string{}); got != nil {
		t.Errorf("BuildSet([]) = %v, want nil", got)
	}
}

func TestBuildSet_MembershipLookup(t *testing.T) {
	t.Parallel()
	set := capfilter.BuildSet([]string{"a", "b", "b"})
	for _, k := range []string{"a", "b"} {
		if _, ok := set[k]; !ok {
			t.Errorf("BuildSet: %q missing from set", k)
		}
	}
	if _, ok := set["c"]; ok {
		t.Error("BuildSet: unexpected member \"c\"")
	}
}

func TestSubset(t *testing.T) {
	t.Parallel()
	allowed := capfilter.BuildSet([]string{"x", "y", "z"})
	tests := []struct {
		name     string
		required []string
		allowed  map[string]struct{}
		want     bool
	}{
		{"empty required is vacuous subset of populated", nil, allowed, true},
		{"empty required is vacuous subset of nil", nil, nil, true},
		{"non-empty required against nil allowed default-denies", []string{"x"}, nil, false},
		{"full subset passes", []string{"x", "y"}, allowed, true},
		{"single miss fails the whole check", []string{"x", "w"}, allowed, false},
		{"exact match passes", []string{"x", "y", "z"}, allowed, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := capfilter.Subset(tc.required, tc.allowed); got != tc.want {
				t.Errorf("Subset(%v, ...) = %v, want %v", tc.required, got, tc.want)
			}
		})
	}
}

func TestDisallowedNames(t *testing.T) {
	t.Parallel()
	allowed := capfilter.BuildSet([]string{"fs_read", "tool_search"})

	if got := capfilter.DisallowedNames(nil, allowed); got != nil {
		t.Errorf("DisallowedNames(nil) = %v, want nil", got)
	}

	got := capfilter.DisallowedNames([]string{"fs_read", "fs_write", "", "net_call"}, allowed)
	want := []string{"fs_write", "net_call"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DisallowedNames = %v, want %v (empty names skipped, allowed names excluded)", got, want)
	}

	// Nil allowed set → every non-empty required name is disallowed.
	gotAll := capfilter.DisallowedNames([]string{"a", "b"}, nil)
	if !reflect.DeepEqual(gotAll, []string{"a", "b"}) {
		t.Errorf("DisallowedNames against nil allowed = %v, want [a b]", gotAll)
	}
}

func TestReplacement_SelectsByToolSearchPresence(t *testing.T) {
	t.Parallel()
	withSearch := capfilter.BuildSet([]string{capfilter.ToolSearchName, "fs_read"})
	if got := capfilter.Replacement(withSearch); got != capfilter.ReplacementWithSearch {
		t.Errorf("Replacement(with tool_search) = %q, want %q", got, capfilter.ReplacementWithSearch)
	}
	withoutSearch := capfilter.BuildSet([]string{"fs_read"})
	if got := capfilter.Replacement(withoutSearch); got != capfilter.ReplacementWithoutSearch {
		t.Errorf("Replacement(without tool_search) = %q, want %q", got, capfilter.ReplacementWithoutSearch)
	}
	if got := capfilter.Replacement(nil); got != capfilter.ReplacementWithoutSearch {
		t.Errorf("Replacement(nil) = %q, want %q", got, capfilter.ReplacementWithoutSearch)
	}
}

func TestScrub(t *testing.T) {
	t.Parallel()
	const repl = capfilter.ReplacementWithSearch

	// Word boundary: "email" must not match inside "emails".
	got := capfilter.Scrub("send email then list emails", []string{"email"}, repl)
	want := "send " + repl + " then list emails"
	if got != want {
		t.Errorf("Scrub word-boundary = %q, want %q", got, want)
	}

	// Empty text / empty disallowed are no-ops.
	if got := capfilter.Scrub("", []string{"x"}, repl); got != "" {
		t.Errorf("Scrub(empty text) = %q, want \"\"", got)
	}
	if got := capfilter.Scrub("untouched", nil, repl); got != "untouched" {
		t.Errorf("Scrub(no disallowed) = %q, want \"untouched\"", got)
	}

	// Regex metacharacters in a tool name are escaped, not interpreted.
	// `fs.write` has word chars at both ends so the `\b` anchors hold;
	// the `.` must be matched literally, not as "any char".
	got = capfilter.Scrub("call fs.write now", []string{"fs.write"}, repl)
	if got != "call "+repl+" now" {
		t.Errorf("Scrub metachar-escaped = %q, want %q", got, "call "+repl+" now")
	}
	// The escaped `.` must not match an arbitrary character: "fsxwrite"
	// must NOT be scrubbed by the pattern for "fs.write".
	got = capfilter.Scrub("call fsxwrite now", []string{"fs.write"}, repl)
	if got != "call fsxwrite now" {
		t.Errorf("Scrub must not interpret metachars: got %q, want unchanged", got)
	}

	// Multiple disallowed names all get scrubbed.
	got = capfilter.Scrub("use fs_write and net_call", []string{"fs_write", "net_call"}, repl)
	if got != "use "+repl+" and "+repl {
		t.Errorf("Scrub multi = %q", got)
	}
}
