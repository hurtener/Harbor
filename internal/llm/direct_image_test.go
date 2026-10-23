package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
)

func TestDirectImageInputBoundsAndRole(t *testing.T) {
	const threshold = 32 << 10
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", threshold)))
	image := &ImagePart{DataURL: dataURL, MIME: "image/png", DirectInput: true}
	request := func(role Role, part *ImagePart) CompleteRequest {
		return CompleteRequest{Model: "m", Messages: []ChatMessage{{Role: role, Content: Content{Parts: []ContentPart{{Type: PartImage, Image: part}}}}}}
	}
	if site, _, found := findContextLeak(request(RoleUser, image), threshold); found {
		t.Fatalf("bounded current-user image treated as context leak: %s", site)
	}
	if site, _, found := findContextLeak(request(RoleTool, image), threshold); !found || !strings.Contains(site, "Image.DataURL") {
		t.Fatalf("tool-role marker bypassed leak check: %q, %t", site, found)
	}
	for _, tc := range []struct {
		name  string
		image *ImagePart
	}{
		{"MIME mismatch", &ImagePart{DataURL: dataURL, MIME: "image/jpeg", DirectInput: true}},
		{"unsupported MIME", &ImagePart{DataURL: "data:image/svg+xml;base64,PHN2Zz4=", MIME: "image/svg+xml", DirectInput: true}},
		{"not base64", &ImagePart{DataURL: "data:image/png,raw", MIME: "image/png", DirectInput: true}},
		{"over 3 MiB", &ImagePart{DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, maxDirectImageBytes+1)), MIME: "image/png", DirectInput: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := materializeRequest(context.Background(), request(RoleUser, tc.image), nil, nil, threshold, identity.Quadruple{}, ModelProfile{})
			if !errors.Is(err, ErrInvalidContent) {
				t.Fatalf("marked invalid input error = %v, want ErrInvalidContent", err)
			}
			if site, _, found := findContextLeak(request(RoleUser, tc.image), threshold); !found && len(tc.image.DataURL) >= threshold {
				t.Fatalf("invalid marker bypassed leak check at %q", site)
			}
		})
	}
}

// The safety pass is per-attempt. A routed retry may use the same assembled
// request concurrently with a vision and a text-only model.
func TestDirectImageSharedRequestConcurrentProfiles(t *testing.T) {
	store, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	const threshold = 256 << 10
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", threshold+1)))
	image := &ImagePart{DataURL: dataURL, MIME: "image/png", DirectInput: true}
	req := CompleteRequest{Model: "m", Messages: []ChatMessage{{Role: RoleUser, Content: Content{Parts: []ContentPart{{Type: PartImage, Image: image}}}}}}
	supports, unsupported := true, false
	profiles := [2]ModelProfile{{ImageInput: &supports}, {ImageInput: &unsupported}}
	id := identity.Quadruple{TenantID: "tenant", UserID: "user", SessionID: "session"}
	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := materializeRequest(context.Background(), req, store, nil, threshold, id, profiles[i%2])
			if err != nil {
				errs <- err
				return
			}
			part := got.Messages[0].Content.Parts[0]
			if i%2 == 0 && (part.Type != PartImage || part.Image == nil || part.Image.DataURL != dataURL) {
				errs <- fmt.Errorf("vision attempt lost image input")
			}
			if i%2 == 1 && (part.Type != PartText || !strings.Contains(part.Text, "does not support image input")) {
				errs <- fmt.Errorf("nonvision attempt received image input")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if original := req.Messages[0].Content.Parts[0]; original.Type != PartImage || original.Image != image || original.Image.DataURL != dataURL {
		t.Fatal("concurrent safety pass mutated the shared input request")
	}
}

func TestNonvisionWithholdsOtherImageForms(t *testing.T) {
	unsupported := false
	for _, image := range []*ImagePart{
		{URL: "https://example.invalid/photo.png", MIME: "image/png"},
		{URL: "https://example.invalid/photo.png", MIME: "image/png", ProviderNative: true},
		{Artifact: &ArtifactStub{Ref: "image-ref", MIME: "image/png"}, MIME: "image/png"},
	} {
		req := CompleteRequest{Messages: []ChatMessage{{Role: RoleUser, Content: Content{Parts: []ContentPart{{Type: PartImage, Image: image}}}}}}
		got, err := materializeRequest(context.Background(), req, nil, nil, 256<<10, identity.Quadruple{}, ModelProfile{ImageInput: &unsupported})
		if err != nil {
			t.Fatal(err)
		}
		part := got.Messages[0].Content.Parts[0]
		if part.Type != PartText || part.Image != nil || !strings.Contains(part.Text, "does not support image input") {
			t.Fatalf("nonvision received image form %#v", part)
		}
		if req.Messages[0].Content.Parts[0].Image != image {
			t.Fatal("withholding mutated caller's image part")
		}
	}
}

func TestRouteImageInputOverridesStaticProfile(t *testing.T) {
	staticSupports, routeUnsupported := true, false
	cfg := ConfigSnapshot{ModelProfiles: map[string]ModelProfile{
		"model": {ContextWindowTokens: 8192, ImageInput: &staticSupports},
	}}
	req := withTrustedModelProfile(CompleteRequest{Model: "model"}, "model", ModelProfile{
		ContextWindowTokens: 8192, ImageInput: &routeUnsupported,
	})
	profile, ok := EffectiveModelProfile(req, cfg)
	if !ok || profile.ImageInput == nil || *profile.ImageInput {
		t.Fatalf("route nonvision fact lost: %+v", profile)
	}
	*profile.ImageInput = true
	if *req.modelProfile.ImageInput || !*cfg.ModelProfiles["model"].ImageInput {
		t.Fatal("effective profile aliases route or static capability")
	}
}
