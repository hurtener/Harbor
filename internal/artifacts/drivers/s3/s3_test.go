package s3_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	awsmw "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	s3driver "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
	"github.com/hurtener/Harbor/internal/config"
)

const (
	envDSN          = "HARBOR_TEST_S3_DSN"
	envBucket       = "HARBOR_TEST_S3_BUCKET"
	envEndpoint     = "HARBOR_TEST_S3_ENDPOINT"
	envRegion       = "HARBOR_TEST_S3_REGION"
	envAccessKey    = "HARBOR_TEST_S3_ACCESS_KEY"
	envSecretKey    = "HARBOR_TEST_S3_SECRET_KEY"
	envPathStyle    = "HARBOR_TEST_S3_USE_PATH_STYLE"
	skipNoEndpoint  = "HARBOR_TEST_S3_DSN (or HARBOR_TEST_S3_*) not set; skipping s3 conformance — see docs/plans/phase-19-artifacts-s3.md"
	defaultS3Region = "us-east-1"
)

// s3TestConfig is the parsed test-time configuration. We accept either
// a flat DSN-style env var (`HARBOR_TEST_S3_DSN`) or individual
// `HARBOR_TEST_S3_*` vars. Unset → tests skip.
type s3TestConfig struct {
	bucket       string
	endpoint     string
	region       string
	accessKey    string
	secretKey    string
	usePathStyle bool
}

// loadS3TestConfig reads the test-time S3 configuration from env. It
// preferentially parses the `HARBOR_TEST_S3_DSN` umbrella var (a
// newline / semicolon / comma-separated `key=value` blob — easy to
// stuff into a single secret) and falls through to the individual
// `HARBOR_TEST_S3_*` vars for any field the umbrella doesn't set.
//
// Returns (nil, "skip-reason") when neither shape is provided, so
// callers t.Skip uniformly.
func loadS3TestConfig() (*s3TestConfig, string) {
	cfg := &s3TestConfig{
		region: defaultS3Region,
	}

	if dsn := os.Getenv(envDSN); dsn != "" {
		// Parse `key=value` pairs separated by newlines, semicolons, or
		// commas. We allow either form so CI YAML can use a multi-line
		// scalar OR a single-line comma-separated string.
		for _, raw := range strings.FieldsFunc(dsn, func(r rune) bool {
			return r == '\n' || r == ';' || r == ','
		}) {
			line := strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:eq])
			val := strings.TrimSpace(line[eq+1:])
			switch key {
			case "bucket":
				cfg.bucket = val
			case "endpoint":
				cfg.endpoint = val
			case "region":
				cfg.region = val
			case "access_key":
				cfg.accessKey = val
			case "secret_key":
				cfg.secretKey = val
			case "use_path_style":
				cfg.usePathStyle = val == "true" || val == "1"
			}
		}
	}

	// Fall through to individual vars for any field still unset.
	if v := os.Getenv(envBucket); v != "" {
		cfg.bucket = v
	}
	if v := os.Getenv(envEndpoint); v != "" {
		cfg.endpoint = v
	}
	if v := os.Getenv(envRegion); v != "" {
		cfg.region = v
	}
	if v := os.Getenv(envAccessKey); v != "" {
		cfg.accessKey = v
	}
	if v := os.Getenv(envSecretKey); v != "" {
		cfg.secretKey = v
	}
	if v := os.Getenv(envPathStyle); v != "" {
		cfg.usePathStyle = v == "true" || v == "1"
	}

	if cfg.bucket == "" {
		return nil, skipNoEndpoint
	}
	return cfg, ""
}

// requireS3 returns the parsed test config or skips the test cleanly.
// Used by every integration test in this file + presign_test.go +
// concurrent_test.go.
func requireS3(t *testing.T) *s3TestConfig {
	t.Helper()
	cfg, skipReason := loadS3TestConfig()
	if cfg == nil {
		t.Skip(skipReason)
	}
	return cfg
}

// uniquePrefix returns a per-test object-key prefix so concurrent
// runs against the same bucket cannot collide.
func uniquePrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "harbor_test_" + hex.EncodeToString(b[:])
}

// rawClient builds a low-level *s3.Client that matches the driver's
// effective configuration. Used by cleanupPrefix to delete every
// object under the test prefix between subtests.
func rawClient(t *testing.T, tc *s3TestConfig) *awss3.Client {
	t.Helper()
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(tc.region),
	}
	if tc.accessKey != "" && tc.secretKey != "" {
		loadOpts = append(loadOpts,
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(tc.accessKey, tc.secretKey, ""),
			),
		)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		t.Fatalf("awsconfig.LoadDefaultConfig: %v", err)
	}
	clientOpts := []func(*awss3.Options){}
	if tc.endpoint != "" {
		ep := tc.endpoint
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.BaseEndpoint = awsmw.String(ep)
		})
	}
	if tc.usePathStyle {
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.UsePathStyle = true
		})
	}
	return awss3.NewFromConfig(awsCfg, clientOpts...)
}

// cleanupPrefix lists every object under prefix and deletes them
// one at a time. We use per-key DeleteObject (rather than the
// batched DeleteObjects) for the same MinIO compat reason the driver
// itself avoids the batch API: older S3-compat backends require a
// Content-MD5 header on multi-object delete that the AWS SDK v2
// doesn't send by default. Best-effort: errors are logged but not
// fatal — the next test run uses a fresh prefix.
func cleanupPrefix(t *testing.T, tc *s3TestConfig, prefix string) {
	t.Helper()
	client := rawClient(t, tc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var continuation *string
	for {
		page, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            awsmw.String(tc.bucket),
			Prefix:            awsmw.String(prefix + "/"),
			ContinuationToken: continuation,
		})
		if err != nil {
			t.Logf("cleanupPrefix list (prefix=%q): %v", prefix, err)
			return
		}
		for _, obj := range page.Contents {
			if _, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: awsmw.String(tc.bucket),
				Key:    obj.Key,
			}); err != nil {
				t.Logf("cleanupPrefix delete %q: %v", awsmw.ToString(obj.Key), err)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuation = page.NextContinuationToken
	}
}

// driverConfig builds an artifacts ArtifactsConfig for the given test
// config + per-subtest prefix.
func driverConfig(tc *s3TestConfig, prefix string) config.ArtifactsConfig {
	return config.ArtifactsConfig{
		Driver:            "s3",
		S3Bucket:          tc.bucket,
		S3Endpoint:        tc.endpoint,
		S3Region:          tc.region,
		S3Prefix:          prefix,
		S3AccessKeyID:     tc.accessKey,
		S3SecretAccessKey: tc.secretKey,
		S3UsePathStyle:    tc.usePathStyle,
	}
}

// TestS3_Conformance drives the canonical conformance suite against
// the S3 driver. Gates on `HARBOR_TEST_S3_DSN` (or the individual
// `HARBOR_TEST_S3_*` vars). CI provides a MinIO service container;
// local runs without a DSN skip cleanly.
//
// Each conformance subtest receives a fresh per-subtest prefix so
// state from one subtest cannot bleed into another. Cleanup deletes
// every object under the prefix.
func TestS3_Conformance(t *testing.T) {
	tc := requireS3(t)
	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		prefix := uniquePrefix(t)
		s, err := s3driver.New(driverConfig(tc, prefix))
		if err != nil {
			t.Fatalf("s3.New: %v", err)
		}
		cleanup := func() {
			_ = s.Close(context.Background())
			cleanupPrefix(t, tc, prefix)
		}
		return s, cleanup
	})
}

// TestS3_DriverRegistered verifies the init() side-effect — the
// driver self-registers under "s3" so OpenDriver can resolve. This is
// a registry-only check; it does not open a real connection (which
// would require S3 availability).
func TestS3_DriverRegistered(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "s3"} // intentionally blank S3Bucket
	_, err := artifacts.OpenDriver("s3", cfg)
	if err == nil {
		t.Fatalf("OpenDriver: expected S3Bucket error, got nil")
	}
	if errors.Is(err, artifacts.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
	if !strings.Contains(err.Error(), "S3Bucket") {
		t.Errorf("error should mention S3Bucket; got: %v", err)
	}
}

// TestS3_New_RequiresBucket pins the explicit-bucket-required
// contract. Empty S3Bucket must surface a clear error rather than
// panic inside the SDK.
func TestS3_New_RequiresBucket(t *testing.T) {
	_, err := s3driver.New(config.ArtifactsConfig{Driver: "s3", S3Bucket: ""})
	if err == nil {
		t.Fatalf("expected error on empty S3Bucket")
	}
	if !strings.Contains(err.Error(), "S3Bucket") {
		t.Errorf("error should mention S3Bucket; got: %v", err)
	}
}

// TestS3_New_RejectsNonExistentBucket exercises the HeadBucket-on-
// construction check: a bucket that doesn't exist at the configured
// endpoint returns a clear error rather than a deferred per-call
// failure.
func TestS3_New_RejectsNonExistentBucket(t *testing.T) {
	tc := requireS3(t)
	cfg := driverConfig(tc, "harbor-test-prefix")
	cfg.S3Bucket = "harbor-nonexistent-bucket-" + fmt.Sprintf("%x", randBytes(t, 6))
	_, err := s3driver.New(cfg)
	if err == nil {
		t.Fatalf("expected HeadBucket error on bogus bucket, got nil")
	}
	// The exact error message depends on the backend (AWS vs MinIO).
	// We assert only that it's clearly a bucket error rather than
	// e.g. a network failure.
	if !strings.Contains(err.Error(), cfg.S3Bucket) && !strings.Contains(err.Error(), "HeadBucket") {
		t.Errorf("error should reference the bucket or HeadBucket; got: %v", err)
	}
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}
