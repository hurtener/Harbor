package artifacts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
)

// TestPresigner_InMemDoesNotImplement pins the negative type-assertion:
// the InMem driver explicitly does NOT implement `Presigner`. This is
// the contract guarding callers against accidental "default to nil URL"
// silent-fallback behavior.
func TestPresigner_InMemDoesNotImplement(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if _, ok := s.(artifacts.Presigner); ok {
		t.Errorf("inmem driver must NOT implement Presigner (the optional capability is reserved for S3-compat drivers)")
	}
}

// TestPresigner_FSDoesNotImplement pins the negative type-assertion
// for the FS driver. Same rationale as InMem: the optional capability
// is reserved for backends with native presigned-URL support.
func TestPresigner_FSDoesNotImplement(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if _, ok := s.(artifacts.Presigner); ok {
		t.Errorf("fs driver must NOT implement Presigner (the optional capability is reserved for S3-compat drivers)")
	}
}

// TestPresigner_ErrPresignUnsupported_IsSentinel pins the sentinel
// error so callers can `errors.Is` against it without depending on
// driver-specific wrapping.
func TestPresigner_ErrPresignUnsupported_IsSentinel(t *testing.T) {
	wrapped := errors.New("driver foo: something happened: " + artifacts.ErrPresignUnsupported.Error())
	if errors.Is(wrapped, artifacts.ErrPresignUnsupported) {
		t.Errorf("plain string concat must NOT match errors.Is — that would be a false positive")
	}
	// Real wrapping with %w must match.
	wrapped2 := wrapErr(artifacts.ErrPresignUnsupported, "driver foo: something happened")
	if !errors.Is(wrapped2, artifacts.ErrPresignUnsupported) {
		t.Errorf("errors.Is on wrapped sentinel returned false; wrapped err: %v", wrapped2)
	}
}

// wrapErr is a tiny helper so the test reads cleanly.
func wrapErr(sentinel error, ctx string) error {
	return &errWrap{sentinel: sentinel, ctx: ctx}
}

type errWrap struct {
	sentinel error
	ctx      string
}

func (e *errWrap) Error() string { return e.ctx + ": " + e.sentinel.Error() }
func (e *errWrap) Unwrap() error { return e.sentinel }
