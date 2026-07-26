package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	awsmw "github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hurtener/Harbor/internal/artifacts"
	s3driver "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
)

// emptyTaskSentinel is the `_` segment the pre-reconciliation object-key
// layout used to stand in for an empty TaskID. The driver no longer
// writes it; these tests reproduce the old layout by hand so the
// resolution fallback that reads it is exercised against a real backend.
const emptyTaskSentinel = "_"

// legacyBlobKey builds the PRE-RECONCILIATION object key —
// `<prefix>/<tenant>/<user>/<session>/<task>/<namespace>/<id>` — which
// the driver stopped writing when the read key became the isolation
// triple, and which it must still resolve.
func legacyBlobKey(prefix string, scope artifacts.ArtifactScope, namespace, id string) string {
	task := scope.TaskID
	if task == "" {
		task = emptyTaskSentinel
	}
	parts := []string{}
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, scope.TenantID, scope.UserID, scope.SessionID, task, namespace, id)
	return strings.Join(parts, "/")
}

// seedLegacyObject writes a blob + sibling meta at the legacy key,
// returning the content-addressed id.
func seedLegacyObject(
	t *testing.T,
	tc *s3TestConfig,
	prefix string,
	scope artifacts.ArtifactScope,
	namespace string,
	payload []byte,
) string {
	t.Helper()
	client := rawClient(t, tc)
	digest := sha256.Sum256(payload)
	hexDigest := hex.EncodeToString(digest[:])
	id := fmt.Sprintf("%s_%s", namespace, hexDigest[:12])
	key := legacyBlobKey(prefix, scope, namespace, id)

	ref := artifacts.ArtifactRef{
		ID:        id,
		MimeType:  "application/json",
		SizeBytes: int64(len(payload)),
		Filename:  "legacy.json",
		SHA256:    hexDigest,
		Scope:     scope,
		Namespace: namespace,
	}
	meta, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal legacy meta: %v", err)
	}
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: awsmw.String(tc.bucket),
		Key:    awsmw.String(key),
		Body:   bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("seed legacy blob %q: %v", key, err)
	}
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: awsmw.String(tc.bucket),
		Key:    awsmw.String(key + ".meta.json"),
		Body:   bytes.NewReader(meta),
	}); err != nil {
		t.Fatalf("seed legacy meta %q: %v", key, err)
	}
	return id
}

// objectExists reports whether a raw object key is present.
func objectExists(t *testing.T, tc *s3TestConfig, key string) bool {
	t.Helper()
	client := rawClient(t, tc)
	_, err := client.HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: awsmw.String(tc.bucket),
		Key:    awsmw.String(key),
	})
	return err == nil
}

// TestS3_LegacyTaskNestedObject_ResolvesOnTheTriple pins the migration
// path that has no migration: a bucket written before the read key was
// reconciled holds objects under `.../<session>/<task>/<ns>/<id>`, and
// nothing rewrites them. They must stay readable through the ordinary
// interface, keyed on the triple, whatever task the caller names.
//
// This is the only way to exercise the resolution fallback — the driver
// cannot produce its input any more.
func TestS3_LegacyTaskNestedObject_ResolvesOnTheTriple(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, tc, prefix)

	producer := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "legacy-run",
	}
	payload := []byte(`{"written":"by an earlier build"}`)
	id := seedLegacyObject(t, tc, prefix, producer, "ns", payload)

	store, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	for _, reader := range []artifacts.ArtifactScope{
		producer,
		{TenantID: "T", UserID: "U", SessionID: "S"},
		{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "some-other-run"},
	} {
		got, found, gerr := store.Get(ctx, reader, id)
		if gerr != nil {
			t.Fatalf("Get(task=%q): %v", reader.TaskID, gerr)
		}
		if !found {
			t.Fatalf("Get(task=%q) found=false — the legacy object is unreachable", reader.TaskID)
		}
		if string(got) != string(payload) {
			t.Errorf("Get(task=%q) bytes=%q, want %q", reader.TaskID, got, payload)
		}
		ref, found, rerr := store.GetRef(ctx, reader, id)
		if rerr != nil || !found {
			t.Fatalf("GetRef(task=%q): found=%v err=%v", reader.TaskID, found, rerr)
		}
		if ref.Scope.TaskID != "legacy-run" {
			t.Errorf("GetRef(task=%q) stamp=%q, want the seeded %q",
				reader.TaskID, ref.Scope.TaskID, "legacy-run")
		}
		exists, eerr := store.Exists(ctx, reader, id)
		if eerr != nil || !exists {
			t.Errorf("Exists(task=%q)=%v err=%v", reader.TaskID, exists, eerr)
		}
	}

	// A cross-session read still misses: the fallback scan is anchored on
	// the session prefix, so widening the key never widened the boundary.
	if _, found, gerr := store.Get(ctx,
		artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "other"}, id); gerr != nil || found {
		t.Errorf("cross-session Get on a legacy object: found=%v err=%v, want (false, nil)", found, gerr)
	}
}

// TestS3_LegacyTaskNestedObject_DedupAndDeleteSweep pins the two
// operations a leftover copy would otherwise corrupt: a Put of the same
// bytes must find the legacy copy rather than store a second one, and a
// Delete must remove EVERY copy so a later Get cannot resolve a
// survivor. A Delete that reported success while leaving one behind is
// the silent degradation CLAUDE.md §13 names.
func TestS3_LegacyTaskNestedObject_DedupAndDeleteSweep(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, tc, prefix)

	producer := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "legacy-run",
	}
	payload := []byte(`{"written":"by an earlier build"}`)
	id := seedLegacyObject(t, tc, prefix, producer, "ns", payload)
	legacyKey := legacyBlobKey(prefix, producer, "ns", id)

	store, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	ctx := context.Background()

	// A later run stores the same bytes: it must dedup onto the legacy
	// copy, keep the legacy stamp, and leave the session holding ONE row.
	later := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "current-run",
	}
	ref, err := store.PutBytes(ctx, later, payload, artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutBytes over a legacy copy: %v", err)
	}
	if ref.ID != id {
		t.Errorf("re-Put id=%q, want the legacy %q", ref.ID, id)
	}
	if ref.Scope.TaskID != "legacy-run" {
		t.Errorf("re-Put stamp=%q, want the first writer's %q", ref.Scope.TaskID, "legacy-run")
	}
	rows, err := store.List(ctx, artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("session holds %d artifacts after a dedup onto a legacy copy, want 1", len(rows))
	}

	// Delete on the triple sweeps the legacy key too.
	existed, err := store.Delete(ctx,
		artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}, id)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Errorf("Delete returned existed=false for a present artifact")
	}
	if objectExists(t, tc, legacyKey) {
		t.Errorf("Delete left the legacy object %q behind", legacyKey)
	}
	if _, found, gerr := store.Get(ctx,
		artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}, id); gerr != nil || found {
		t.Errorf("after Delete: found=%v err=%v, want (false, nil)", found, gerr)
	}
}
