package skillpkg_test

// Strict canonical-decoder adversarial suite.
//
// FromCanonicalBytes is the bounded, closed, EXACT decoder: it accepts
// only byte-for-byte canonical serializations. This file pins every
// adversarial class the decoder must reject — authority-looking
// unknown fields, duplicate object keys (top-level, nested skill, and
// nested support entries), trailing JSON, oversized input and
// oversized whitespace, and non-canonical alternate forms (key
// reordering, whitespace, unsorted tags/supports, explicit empty
// lists, null for omitted fields, trailing newlines). A canonical
// input still round-trips.

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

func TestFromCanonicalBytes_RejectsUnknownFields(t *testing.T) {
	// A structurally valid package carrying an unknown field at any
	// object level — including authority-bearing names — is rejected:
	// the canonical wire is closed, so caller content can never ride
	// along outside the hash.
	cases := []struct {
		name string
		in   string
	}{
		{"top-level scope", `{"name":"x","scope":"user","skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"top-level origin", `{"name":"x","origin":"remote","skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"top-level tenant", `{"name":"x","tenant":"t","skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"top-level arbitrary", `{"name":"x","x-custom":"v","skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"skill-level authority", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"],"authority":"admin"}}`},
		{"skill-level agent", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"],"agent":"a"}}`},
		{"skill-level audience", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"],"audience":"public"}}`},
		{"support-level scope", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"]},"supports":[{"path":"a.txt","mime":"text/plain; charset=utf-8","size":1,"digest":"` + strings.Repeat("0", 64) + `","scope":"user"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := skillpkg.FromCanonicalBytes([]byte(c.in)); err == nil {
				t.Fatalf("FromCanonicalBytes(%s): expected unknown-field rejection, got nil", c.name)
			} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
				t.Fatalf("FromCanonicalBytes(%s): err=%v, want ErrInvalidPackage", c.name, err)
			}
		})
	}
}

func TestFromCanonicalBytes_RejectsDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"top-level name", `{"name":"x","name":"y","skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"top-level skill", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"]},"skill":{"name":"x","trigger":"t","steps":["s"]}}`},
		{"skill-level trigger", `{"name":"x","skill":{"name":"x","trigger":"t","trigger":"u","steps":["s"]}}`},
		{"skill-level steps", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"],"steps":["u"]}}`},
		{"support-level path", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"]},"supports":[{"path":"a.txt","path":"b.txt","mime":"text/plain; charset=utf-8","size":1,"digest":"` + strings.Repeat("0", 64) + `"}]}`},
		{"support-level digest", `{"name":"x","skill":{"name":"x","trigger":"t","steps":["s"]},"supports":[{"path":"a.txt","mime":"text/plain; charset=utf-8","size":1,"digest":"` + strings.Repeat("0", 64) + `","digest":"` + strings.Repeat("1", 64) + `"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := skillpkg.FromCanonicalBytes([]byte(c.in)); err == nil {
				t.Fatalf("FromCanonicalBytes(%s): expected duplicate-key rejection, got nil", c.name)
			} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
				t.Fatalf("FromCanonicalBytes(%s): err=%v, want ErrInvalidPackage", c.name, err)
			}
		})
	}
}

func TestFromCanonicalBytes_RejectsTrailingContent(t *testing.T) {
	cb, err := skillpkg.CanonicalBytes(testPackage())
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	for _, suffix := range []string{
		` {}`, `[]`, ` garbage`, `null`, `"x"`,
	} {
		bad := append(append([]byte(nil), cb...), []byte(suffix)...)
		if _, err := skillpkg.FromCanonicalBytes(bad); err == nil {
			t.Fatalf("FromCanonicalBytes(canonical + %q): expected trailing-content rejection, got nil", suffix)
		} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
			t.Fatalf("FromCanonicalBytes(canonical + %q): err=%v, want ErrInvalidPackage", suffix, err)
		}
	}
}

func TestFromCanonicalBytes_RejectsOversizedInput(t *testing.T) {
	// The input byte bound is enforced BEFORE decoding: an oversized
	// document (even pure whitespace) cannot trigger unbounded
	// allocation.
	big := make([]byte, skillpkg.MaxCanonicalBytes+1)
	copy(big, " ")
	if _, err := skillpkg.FromCanonicalBytes(big); err == nil {
		t.Fatal("FromCanonicalBytes(oversized): expected rejection, got nil")
	} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
		t.Fatalf("FromCanonicalBytes(oversized): err=%v, want ErrInvalidPackage", err)
	}

	// Oversized whitespace padding a valid canonical form at the end:
	// the bound still rejects before any decode work.
	cb, err := skillpkg.CanonicalBytes(testPackage())
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	padded := make([]byte, skillpkg.MaxCanonicalBytes+1+len(cb))
	for i := range padded {
		padded[i] = ' '
	}
	copy(padded[len(padded)-len(cb):], cb)
	if _, err := skillpkg.FromCanonicalBytes(padded); err == nil {
		t.Fatal("FromCanonicalBytes(oversized whitespace + canonical): expected rejection, got nil")
	}
}

func TestFromCanonicalBytes_RoundTripAndNonCanonicalForms(t *testing.T) {
	cb, err := skillpkg.CanonicalBytes(testPackage())
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	canon := string(cb)

	// The canonical form round-trips.
	got, err := skillpkg.FromCanonicalBytes(cb)
	if err != nil {
		t.Fatalf("FromCanonicalBytes(canonical): %v", err)
	}
	if _, err := skillpkg.CanonicalBytes(got); err != nil {
		t.Fatalf("CanonicalBytes(round-tripped): %v", err)
	}

	// Non-canonical alternate bytes for the SAME logical package are
	// all rejected: key reordering + whitespace, trailing newline,
	// leading whitespace, and internal whitespace.
	cases := []struct {
		name string
		in   string
	}{
		{"reordered keys + whitespace", `{"version":"1.0.0","name":"demo-skill","skill":{"trigger":"when the user asks for a demo","steps":["step one","step two"],"title":"Demo","required_ns":["ns-a"],"required_tags":["tag-a"],"required_tools":["tool-a"],"preconditions":["precondition one"],"name":"demo-skill","failure_modes":["failure one"],"description":"A demo skill.","tags":["alpha","beta"],"task_type":"code"},"supports":[{"path":"assets/logo.png","digest":"` + sha256Hex(pngBytes()) + `","size":` + strconv.Itoa(len(pngBytes())) + `,"mime":"image/png"},{"path":"examples/demo.json","digest":"` + sha256Hex([]byte(`{"demo": true}`)) + `","size":13,"mime":"application/json"}]}`},
		{"trailing newline", canon + "\n"},
		{"leading whitespace", "  " + canon},
		{"internal whitespace", strings.Replace(canon, `{"name":`, `{ "name": `, 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := skillpkg.FromCanonicalBytes([]byte(c.in)); err == nil {
				t.Fatalf("FromCanonicalBytes(%s): expected non-canonical rejection, got nil", c.name)
			} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
				t.Fatalf("FromCanonicalBytes(%s): err=%v, want ErrInvalidPackage", c.name, err)
			}
		})
	}
}

func TestFromCanonicalBytes_RejectsAlternateArrayAndNullForms(t *testing.T) {
	// Unsorted tags, explicit empty lists, `null` where the canonical
	// form omits the field, and a reversed support array are all
	// alternate bytes for the same logical package — and all are
	// rejected, because the accepted bytes must be EXACTLY
	// CanonicalBytes.
	p := testPackage()
	cb, err := skillpkg.CanonicalBytes(p)
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	canon := string(cb)

	unsortedTags := strings.Replace(canon, `"tags":["alpha","beta"]`, `"tags":["beta","alpha"]`, 1)
	explicitEmpty := strings.Replace(canon, `"version":"1.0.0"`, `"version":"1.0.0","tags":[]`, 1)
	nullVersion := strings.Replace(canon, `"version":"1.0.0"`, `"version":null`, 1)
	reversedSupports := strings.Replace(canon,
		`{"path":"assets/logo.png"`, `{"path":"__REV__"`, 1)
	reversedSupports = strings.Replace(reversedSupports,
		`{"path":"examples/demo.json"`, `{"path":"assets/logo.png"`, 1)
	reversedSupports = strings.Replace(reversedSupports, `{"path":"__REV__"`, `{"path":"examples/demo.json"}`, 1)

	for _, c := range []struct {
		name string
		in   string
	}{
		{"unsorted tags", unsortedTags},
		{"explicit empty list", explicitEmpty},
		{"null for omitted field", nullVersion},
		{"reversed supports", reversedSupports},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.in == canon {
				t.Fatalf("test construction failed: %s did not mutate the canonical form", c.name)
			}
			if _, err := skillpkg.FromCanonicalBytes([]byte(c.in)); err == nil {
				t.Fatalf("FromCanonicalBytes(%s): expected rejection, got nil", c.name)
			} else if !errors.Is(err, skillpkg.ErrInvalidPackage) {
				t.Fatalf("FromCanonicalBytes(%s): err=%v, want ErrInvalidPackage", c.name, err)
			}
		})
	}
}
