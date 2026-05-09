package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

// EventStruct is the JSON-tag-style payload some downstream callers
// will hand to the redactor as a Go struct rather than map[string]any.
// The reflective walker must descend into it and redact the
// secret-named fields.
type EventStruct struct {
	RequestID    string  `json:"request_id"`
	APIKey       string  `json:"api_key"`
	Password     string  `json:"password"`
	NestedStruct *Nested `json:"nested,omitempty"`
	Items        []Item  `json:"items"`
	Tags         []string `json:"tags"`
	HiddenField  string  // unexported-style: no tag, reflected as "hiddenfield"
}

type Nested struct {
	Cookie string `json:"cookie"`
	Other  string `json:"other"`
}

type Item struct {
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
}

// YAMLEventStruct exercises the yaml-tag fallback path of fieldName.
type YAMLEventStruct struct {
	BindAddr string `yaml:"bind_addr"`
	APIKey   string `yaml:"api_key"`
}

func TestReflective_RedactsStructFieldsByJSONTag(t *testing.T) {
	driver := patterns.New()
	in := EventStruct{
		RequestID: "req-1",
		APIKey:    "real-key",
		Password:  "hunter2",
		NestedStruct: &Nested{
			Cookie: "session=secret",
			Other:  "ok",
		},
		Items: []Item{
			{Name: "first", AccessToken: "t1"},
			{Name: "second", AccessToken: "t2"},
		},
		Tags:        []string{"alpha", "beta"},
		HiddenField: "shown",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if m["request_id"] != "req-1" {
		t.Errorf("request_id = %v, want req-1", m["request_id"])
	}
	if m["api_key"] != audit.Placeholder {
		t.Errorf("api_key = %v, want %s", m["api_key"], audit.Placeholder)
	}
	if m["password"] != audit.Placeholder {
		t.Errorf("password = %v, want %s", m["password"], audit.Placeholder)
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested not a map: %T", m["nested"])
	}
	if nested["cookie"] != audit.Placeholder {
		t.Errorf("nested.cookie = %v, want %s", nested["cookie"], audit.Placeholder)
	}
	if nested["other"] != "ok" {
		t.Errorf("nested.other was redacted: %v", nested["other"])
	}
	items, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items not a slice: %T", m["items"])
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["name"] != "first" {
		t.Errorf("items[0].name = %v", first["name"])
	}
	if first["access_token"] != audit.Placeholder {
		t.Errorf("items[0].access_token = %v, want %s", first["access_token"], audit.Placeholder)
	}
}

func TestReflective_RedactsStructFieldsByYAMLTag(t *testing.T) {
	driver := patterns.New()
	in := YAMLEventStruct{
		BindAddr: "127.0.0.1:8080",
		APIKey:   "yaml-secret",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["bind_addr"] != "127.0.0.1:8080" {
		t.Errorf("bind_addr = %v, want 127.0.0.1:8080", m["bind_addr"])
	}
	if m["api_key"] != audit.Placeholder {
		t.Errorf("api_key = %v, want %s", m["api_key"], audit.Placeholder)
	}
}

func TestReflective_RedactsStructFieldsByGoFieldName(t *testing.T) {
	// A struct with no tags — fieldName must fall back to the
	// lowercased Go field name. APIKey → "apikey" matches the
	// canonical alias list.
	type Untagged struct {
		Name   string
		APIKey string
	}
	driver := patterns.New()
	in := Untagged{Name: "n", APIKey: "secret"}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["apikey"] != audit.Placeholder {
		t.Errorf("apikey = %v, want %s", m["apikey"], audit.Placeholder)
	}
	if m["name"] != "n" {
		t.Errorf("name = %v, want n", m["name"])
	}
}

func TestReflective_PointerToStruct(t *testing.T) {
	driver := patterns.New()
	in := &EventStruct{
		RequestID: "p-1",
		APIKey:    "ptr-secret",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if m["api_key"] != audit.Placeholder {
		t.Errorf("api_key = %v, want %s", m["api_key"], audit.Placeholder)
	}
}

func TestReflective_NilPointerStruct(t *testing.T) {
	driver := patterns.New()
	var in *EventStruct = nil
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact(nil-pointer): %v", err)
	}
	if out != nil {
		t.Errorf("Redact(nil-pointer) = %v, want nil", out)
	}
}

func TestReflective_NonStringKeyedMapPassesThrough(t *testing.T) {
	driver := patterns.New()
	in := map[int]string{1: "secret", 2: "another"}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	got, ok := out.(map[int]string)
	if !ok {
		t.Fatalf("non-string-keyed map type changed: %T", out)
	}
	if got[1] != "secret" || got[2] != "another" {
		t.Errorf("non-string-keyed map values changed: %v", got)
	}
}

func TestReflective_StringKeyedReflectMap(t *testing.T) {
	driver := patterns.New()
	// A typed map[string]string forces the reflective path.
	in := map[string]string{
		"api_key":   "real",
		"hostname":  "example.com",
		"password":  "hunter2",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any after reflection, got %T", out)
	}
	if m["api_key"] != audit.Placeholder {
		t.Errorf("api_key = %v, want %s", m["api_key"], audit.Placeholder)
	}
	if m["hostname"] != "example.com" {
		t.Errorf("hostname = %v, want example.com", m["hostname"])
	}
	if m["password"] != audit.Placeholder {
		t.Errorf("password = %v, want %s", m["password"], audit.Placeholder)
	}
}

func TestReflective_TypedSlice(t *testing.T) {
	driver := patterns.New()
	in := []EventStruct{
		{RequestID: "a", APIKey: "secret-a"},
		{RequestID: "b", APIKey: "secret-b"},
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if len(slice) != 2 {
		t.Fatalf("len = %d, want 2", len(slice))
	}
	first := slice[0].(map[string]any)
	if first["api_key"] != audit.Placeholder {
		t.Errorf("[0].api_key = %v, want %s", first["api_key"], audit.Placeholder)
	}
}

func TestReflective_ArrayType(t *testing.T) {
	driver := patterns.New()
	var in [2]string
	in[0] = "hello"
	in[1] = "Bearer abc.def.ghi"
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact array: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if slice[0] != "hello" {
		t.Errorf("array[0] = %v", slice[0])
	}
	got1 := slice[1].(string)
	if got1 == "Bearer abc.def.ghi" {
		t.Errorf("Bearer credential not redacted in array element")
	}
}

func TestReflective_ReplaceStringsViaStruct(t *testing.T) {
	// Exercise reflectiveReplaceStrings via a struct payload that
	// contains an embedded Bearer credential — forces the reflect
	// path of walkReplaceStrings.
	type Probe struct {
		Note string `json:"note"`
	}
	driver := patterns.New()
	in := Probe{Note: "header value: Bearer abc.def.ghi end"}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["note"].(string)
	if got == "header value: Bearer abc.def.ghi end" {
		t.Errorf("Bearer-in-value rule did not run on struct field: %q", got)
	}
}

// TestRedactor_TopLevelArtifactRefPassesThrough verifies that an
// ArtifactRef supplied as the top-level payload is returned
// unchanged by every rule.
func TestRedactor_TopLevelArtifactRefPassesThrough(t *testing.T) {
	driver := patterns.New()
	ref := audit.ArtifactRef{Ref: "art://abc", MIME: "image/png", SizeBytes: 1024}
	out, err := driver.Redact(context.Background(), ref)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	got, ok := out.(audit.ArtifactRef)
	if !ok {
		t.Fatalf("ArtifactRef type changed: %T", out)
	}
	if got != ref {
		t.Errorf("ArtifactRef value changed: got %+v, want %+v", got, ref)
	}
}

// failingDeepRule returns an error mid-walk to exercise the
// reflective error-propagation path.
type failingDeepRule struct{}

func (failingDeepRule) Name() string { return "failing-deep" }
func (failingDeepRule) Apply(_ context.Context, _ any) (any, error) {
	return nil, errors.New("deep rule failed")
}

func TestDriver_FailingDeepRule_WrapsSentinel(t *testing.T) {
	d := patterns.NewWithRules([]audit.Rule{failingDeepRule{}})
	out, err := d.Redact(context.Background(), map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("expected error")
	}
	if out != nil {
		t.Errorf("expected nil out, got %v", out)
	}
	if !errors.Is(err, audit.ErrRedactionFailed) {
		t.Errorf("err=%v, want errors.Is ErrRedactionFailed", err)
	}
}

// TestReflectiveReplace_TypedSliceOfStrings exercises the slice
// branch of reflectiveReplaceStrings — a `[]string` falls out of
// the type switch in walkReplaceStrings into the reflect path.
func TestReflectiveReplace_TypedSliceOfStrings(t *testing.T) {
	driver := patterns.New()
	in := []string{
		"plain",
		"header: Bearer abc.def.ghi",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if slice[0] != "plain" {
		t.Errorf("[0] = %v, want plain", slice[0])
	}
	if got := slice[1].(string); got != "header: Bearer "+audit.Placeholder {
		t.Errorf("[1] = %q, want bearer-redacted", got)
	}
}

// TestReflectiveReplace_TypedStringMap exercises the map branch
// of reflectiveReplaceStrings via a `map[string]string`.
func TestReflectiveReplace_TypedStringMap(t *testing.T) {
	driver := patterns.New()
	in := map[string]string{
		"clean":   "plain text",
		"tagged":  "Bearer xyz",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if m["clean"] != "plain text" {
		t.Errorf("clean = %v", m["clean"])
	}
	got := m["tagged"].(string)
	if got != "Bearer "+audit.Placeholder {
		t.Errorf("tagged = %q, want bearer-redacted", got)
	}
}

// TestReflectiveReplace_NonStringKeyedMapPassesThrough exercises the
// "non-string key" branch of reflectiveReplaceStrings.
func TestReflectiveReplace_NonStringKeyedMapPassesThrough(t *testing.T) {
	driver := patterns.New()
	in := map[int]string{
		1: "Bearer xxx",
		2: "plain",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	got, ok := out.(map[int]string)
	if !ok {
		t.Fatalf("non-string-keyed map type changed: %T", out)
	}
	if got[1] != "Bearer xxx" || got[2] != "plain" {
		t.Errorf("non-string-keyed map values changed: %v", got)
	}
}

// TestReflectiveReplace_PointerToStruct exercises the Pointer +
// Struct branches of reflectiveReplaceStrings.
func TestReflectiveReplace_PointerToStruct(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
	}
	driver := patterns.New()
	in := &Probe{Note: "audit log: Bearer pqr.stu.vwx received"}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact(*Probe): %v", err)
	}
	m := out.(map[string]any)
	got := m["note"].(string)
	if got == "audit log: Bearer pqr.stu.vwx received" {
		t.Errorf("Bearer credential not redacted in *struct field: %q", got)
	}
}

// TestReflectiveReplace_NilPointerThroughMultimodalRule exercises
// the Pointer-isNil branch of reflectiveReplaceStrings.
func TestReflectiveReplace_NilPointerThroughMultimodalRule(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
	}
	driver := patterns.New()
	var in *Probe
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact(nil-pointer): %v", err)
	}
	if out != nil {
		t.Errorf("Redact(nil-pointer) = %v, want nil", out)
	}
}

// TestReflectiveReplace_TypedArray exercises the Array branch of
// reflectiveReplaceStrings.
func TestReflectiveReplace_TypedArray(t *testing.T) {
	driver := patterns.New()
	var in [2]string
	in[0] = "Bearer aaa"
	in[1] = "no token here"
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact array: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if slice[0].(string) != "Bearer "+audit.Placeholder {
		t.Errorf("[0] = %v", slice[0])
	}
	if slice[1] != "no token here" {
		t.Errorf("[1] = %v", slice[1])
	}
}

// TestReflectiveReplace_PassesThroughUnsupportedKind exercises the
// final default-return branch where the redactor doesn't know how
// to descend (e.g. a chan).
func TestReflectiveReplace_PassesThroughUnsupportedKind(t *testing.T) {
	driver := patterns.New()
	ch := make(chan int, 1)
	in := map[string]any{"channel": ch}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["channel"] != ch {
		t.Errorf("channel value changed: got %v want %v", m["channel"], ch)
	}
}

// TestRedact_PointerToArtifactRef exercises the *ArtifactRef branch
// of isArtifactRef.
func TestRedact_PointerToArtifactRef(t *testing.T) {
	driver := patterns.New()
	ref := &audit.ArtifactRef{Ref: "art://ptr", MIME: "image/png", SizeBytes: 100}
	in := map[string]any{"api_key": ref}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["api_key"] != ref {
		t.Errorf("*ArtifactRef did not pass through: got %+v", m["api_key"])
	}
}

// bearerOnly returns a driver with ONLY the bearer-in-value rule
// engaged, so reflective-walk branches of walkReplaceStrings are
// reached without earlier rules first converting structs to maps.
func bearerOnly() *patterns.Driver {
	all := audit.CanonicalRules()
	for _, r := range all {
		if r.Name() == "bearer_in_value" {
			return patterns.NewWithRules([]audit.Rule{r})
		}
	}
	return nil
}

func TestReflectiveReplace_TypedSliceDirect(t *testing.T) {
	d := bearerOnly()
	in := []string{"hello", "Bearer abc.def.ghi"}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if slice[0] != "hello" {
		t.Errorf("[0] = %v", slice[0])
	}
	if got := slice[1].(string); got != "Bearer "+audit.Placeholder {
		t.Errorf("[1] = %q", got)
	}
}

func TestReflectiveReplace_TypedMapDirect(t *testing.T) {
	d := bearerOnly()
	in := map[string]string{"k": "Bearer xyz"}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if got := m["k"].(string); got != "Bearer "+audit.Placeholder {
		t.Errorf("k = %q", got)
	}
}

func TestReflectiveReplace_StructDirect(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
	}
	d := bearerOnly()
	in := Probe{Note: "header: Bearer abc"}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if got := m["note"].(string); got == "header: Bearer abc" {
		t.Errorf("not redacted: %q", got)
	}
}

func TestReflectiveReplace_PointerDirect(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
	}
	d := bearerOnly()
	in := &Probe{Note: "Bearer ptr"}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact(*Probe): %v", err)
	}
	m := out.(map[string]any)
	if got := m["note"].(string); got == "Bearer ptr" {
		t.Errorf("not redacted: %q", got)
	}
}

func TestReflectiveReplace_NilPointerDirect(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
	}
	d := bearerOnly()
	var in *Probe
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact(nil-pointer): %v", err)
	}
	if out != nil {
		t.Errorf("got %v, want nil", out)
	}
}

func TestReflectiveReplace_TypedArrayDirect(t *testing.T) {
	d := bearerOnly()
	var in [2]string
	in[0] = "Bearer top"
	in[1] = "no token"
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact array: %v", err)
	}
	slice, ok := out.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", out)
	}
	if got := slice[0].(string); got != "Bearer "+audit.Placeholder {
		t.Errorf("[0] = %v", slice[0])
	}
}

func TestReflectiveReplace_NonStringMapPassesThrough(t *testing.T) {
	d := bearerOnly()
	in := map[int]string{1: "Bearer hidden"}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	got, ok := out.(map[int]string)
	if !ok {
		t.Fatalf("non-string-keyed map type changed: %T", out)
	}
	if got[1] != "Bearer hidden" {
		t.Errorf("non-string-keyed map mutated: %v", got)
	}
}

func TestReflectiveReplace_ArtifactRefPassesThrough(t *testing.T) {
	d := bearerOnly()
	ref := audit.ArtifactRef{Ref: "art://r", MIME: "image/png"}
	in := map[string]any{"data": ref}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["data"] != ref {
		t.Errorf("ArtifactRef changed: %+v", m["data"])
	}
}

func TestRedact_DataURL_NonBase64_UsesRawLength(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"image": "data:image/png,raw-percent-encoded-content-not-base64",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["image"].(string)
	if got == "data:image/png,raw-percent-encoded-content-not-base64" {
		t.Errorf("non-base64 DataURL was not redacted")
	}
	if !strings.HasPrefix(got, "[redacted: image/png of ") {
		t.Errorf("DataURL placeholder shape wrong: %q", got)
	}
}

func TestRedact_DataURL_InvalidBase64_FallsBackToEstimation(t *testing.T) {
	driver := patterns.New()
	// `_-.` are allowed by the regex's char class but are NOT in the
	// standard base64 alphabet; the StdEncoding decode fails and the
	// length-estimation fallback in decodedSize fires.
	in := map[string]any{
		"image": "data:image/png;base64,___---...___---xxx",
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["image"].(string)
	if !strings.HasPrefix(got, "[redacted: image/png of ") {
		t.Errorf("invalid-base64 DataURL not redacted: %q", got)
	}
}

// TestRedact_TimeTimePassesThrough pins the opaque-passthrough
// behavior added for the audit reflective walker. Without it,
// time.Time (a struct with no exported fields) collapses to an
// empty map[string]any after redaction, silently losing timestamps.
func TestRedact_TimeTimePassesThrough(t *testing.T) {
	type Probe struct {
		Note string `json:"note"`
		At   time.Time `json:"at"`
		Dur  time.Duration `json:"dur"`
	}
	driver := patterns.New()
	want := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	in := Probe{Note: "ok", At: want, Dur: 5 * time.Second}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	gotAt, ok := m["at"].(time.Time)
	if !ok {
		t.Fatalf("at field type=%T, want time.Time (got %v)", m["at"], m["at"])
	}
	if !gotAt.Equal(want) {
		t.Errorf("time.Time round-trip changed value: got %v, want %v", gotAt, want)
	}
	gotDur, ok := m["dur"].(time.Duration)
	if !ok {
		t.Fatalf("dur field type=%T, want time.Duration", m["dur"])
	}
	if gotDur != 5*time.Second {
		t.Errorf("time.Duration round-trip changed: got %v, want 5s", gotDur)
	}
}

func TestRedact_NilValueUnderSecretKey(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{"api_key": nil}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	if m["api_key"] != audit.Placeholder {
		t.Errorf("api_key=nil should still redact to placeholder, got %v", m["api_key"])
	}
}

// TestRedact_DataURLWithoutMIMEFallsBackToOctetStream stays the
// last test in the original block.
func TestRedact_DataURLWithoutMIMEFallsBackToOctetStream(t *testing.T) {
	driver := patterns.New()
	in := map[string]any{
		"image": "data:;base64,SGVsbG8gV29ybGQ=", // "Hello World"
	}
	out, err := driver.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := out.(map[string]any)
	got := m["image"].(string)
	if got == "data:;base64,SGVsbG8gV29ybGQ=" {
		t.Errorf("DataURL without MIME was not redacted")
	}
	if got == "" {
		t.Errorf("got empty redaction")
	}
}
