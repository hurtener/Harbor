package llm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// materializeRequest walks the assembled CompleteRequest and rewrites
// any inline DataURL content whose decoded byte length is ≥
// threshold into an ArtifactStub backed by the configured
// ArtifactStore. The function is idempotent: a part that already
// carries an Artifact is a no-op; a URL part is a no-op; a sub-
// threshold DataURL is a no-op.
//
// Materialization is the FIRST step of the safety pass — it rewrites
// the request, then the leak-detection step asserts no raw heavy
// content survived (D-026). The order matters: a producer that
// fails to materialize gets one more chance here; a producer that
// emits raw bytes in a text field (not a DataURL) is caught by the
// leak step.
//
// The store-side write uses the calling identity for scope; the
// safety pass requires identity in ctx before reaching this function
// (enforced upstream by safetyClient.Complete).
//
// Emits `llm.image.materialized` per rewritten part. Errors from the
// store propagate — fail-loudly is the runtime principle (AGENTS.md
// §5 + §13); the LLM call MUST NOT proceed with a half-materialized
// request.
func materializeRequest(
	ctx context.Context,
	req CompleteRequest,
	store artifacts.ArtifactStore,
	bus events.EventBus,
	threshold int,
	id identity.Quadruple,
) (CompleteRequest, error) {
	for mi := range req.Messages {
		if req.Messages[mi].Content.Parts == nil {
			continue
		}
		for pi := range req.Messages[mi].Content.Parts {
			part := &req.Messages[mi].Content.Parts[pi]
			switch part.Type {
			case PartImage:
				if part.Image == nil {
					return req, fmt.Errorf("%w: Messages[%d].Parts[%d].Type=image but Image is nil",
						ErrInvalidContent, mi, pi)
				}
				stub, materialized, err := materializeDataURL(
					ctx, store, threshold, id, part.Image.DataURL, part.Image.MIME, "", part.Image.Artifact,
				)
				if err != nil {
					return req, err
				}
				if materialized {
					part.Image.DataURL = ""
					part.Image.Artifact = stub
					emitMaterialized(ctx, bus, id, req.Model, stub)
				}
			case PartAudio:
				if part.Audio == nil {
					return req, fmt.Errorf("%w: Messages[%d].Parts[%d].Type=audio but Audio is nil",
						ErrInvalidContent, mi, pi)
				}
				stub, materialized, err := materializeDataURL(
					ctx, store, threshold, id, part.Audio.DataURL, part.Audio.MIME, "", part.Audio.Artifact,
				)
				if err != nil {
					return req, err
				}
				if materialized {
					part.Audio.DataURL = ""
					part.Audio.Artifact = stub
					emitMaterialized(ctx, bus, id, req.Model, stub)
				}
			case PartFile:
				if part.File == nil {
					return req, fmt.Errorf("%w: Messages[%d].Parts[%d].Type=file but File is nil",
						ErrInvalidContent, mi, pi)
				}
				stub, materialized, err := materializeDataURL(
					ctx, store, threshold, id, part.File.DataURL, part.File.MIME, part.File.Filename, part.File.Artifact,
				)
				if err != nil {
					return req, err
				}
				if materialized {
					part.File.DataURL = ""
					part.File.Artifact = stub
					emitMaterialized(ctx, bus, id, req.Model, stub)
				}
			case PartText:
				// Text parts pass through. The leak-detection step
				// catches raw heavy content in Text after this.
			}
		}
	}
	return req, nil
}

// materializeDataURL handles ONE part's worth of materialize work.
// Returns the new ArtifactStub (when materialization fires), a
// bool indicating whether the caller should adopt the stub, and any
// error.
//
// Cases:
//   - Existing Artifact set → no-op (`(existing, false, nil)`).
//   - DataURL empty → no-op (`(nil, false, nil)`).
//   - DataURL set but undecode-able → fail loudly with ErrInvalidContent.
//   - Decoded bytes < threshold → no-op (sub-threshold inline allowed).
//   - Decoded bytes ≥ threshold → write to ArtifactStore, return stub.
func materializeDataURL(
	ctx context.Context,
	store artifacts.ArtifactStore,
	threshold int,
	id identity.Quadruple,
	dataURL, mime, filename string,
	existing *ArtifactStub,
) (*ArtifactStub, bool, error) {
	if existing != nil {
		return existing, false, nil
	}
	if dataURL == "" {
		return nil, false, nil
	}
	bytes, declaredMIME, err := decodeDataURL(dataURL)
	if err != nil {
		return nil, false, fmt.Errorf("%w: data URL: %v", ErrInvalidContent, err)
	}
	if len(bytes) < threshold {
		return nil, false, nil
	}
	// Use the declared MIME from the data URL when the part-level
	// MIME is empty. Declared MIME wins on conflict — operators who
	// set Image.MIME=image/png but ship a data:image/jpeg payload
	// have a real bug they should see in the error path; defer
	// surfacing that to Phase 33 (the driver will reject the
	// translation), Phase 32 normalises to what's on the wire.
	effectiveMIME := mime
	if effectiveMIME == "" {
		effectiveMIME = declaredMIME
	}
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
		TaskID:    id.RunID, // RunID maps to TaskID for foreground (artifacts.ArtifactScope godoc)
	}
	opts := artifacts.PutOpts{
		MimeType:  effectiveMIME,
		Filename:  filename,
		Namespace: "llm.materialized",
		Source:    map[string]any{"phase": "32", "stage": "auto_materialize"},
	}
	ref, err := store.PutBytes(ctx, scope, bytes, opts)
	if err != nil {
		return nil, false, fmt.Errorf("llm: materialize PutBytes: %w", err)
	}
	sum := sha256.Sum256(bytes)
	stub := &ArtifactStub{
		Ref:       ref.ID,
		MIME:      effectiveMIME,
		SizeBytes: int64(len(bytes)),
		Hash:      "sha256:" + hex.EncodeToString(sum[:]),
		Fetch: &StubFetch{
			Tool: "artifact.fetch",
			ID:   ref.ID,
		},
	}
	return stub, true, nil
}

// decodeDataURL parses a `data:<mime>[;base64],<payload>` URI and
// returns the decoded bytes + declared MIME. Errors are
// short-message; the caller wraps with ErrInvalidContent.
//
// Non-base64 data URLs (i.e. URL-encoded payloads) are rare for
// multimodal; we accept the form but treat undecoded ASCII as the
// payload bytes. The threshold check applies to the decoded length.
func decodeDataURL(uri string) ([]byte, string, error) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, "", errors.New("not a data URI")
	}
	body := uri[len("data:"):]
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return nil, "", errors.New("missing ','")
	}
	header := body[:comma]
	payload := body[comma+1:]
	mime := header
	isBase64 := false
	if i := strings.IndexByte(header, ';'); i >= 0 {
		mime = header[:i]
		params := header[i+1:]
		// Honour `base64` token; other ;params are operator-supplied
		// codec hints and ignored at the safety-net layer.
		for _, p := range strings.Split(params, ";") {
			if strings.TrimSpace(p) == "base64" {
				isBase64 = true
				break
			}
		}
	}
	if isBase64 {
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, mime, fmt.Errorf("base64 decode: %w", err)
		}
		return b, mime, nil
	}
	return []byte(payload), mime, nil
}

// emitMaterialized publishes the `llm.image.materialized` event.
// Errors from the bus are non-fatal — the request continues; an
// audit-pipeline failure should not block an LLM call (the bus's
// drop-oldest backpressure already covers ordinary congestion).
// A persistent bus failure would surface in operator-facing health
// elsewhere.
func emitMaterialized(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, stub *ArtifactStub) {
	if bus == nil || stub == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{
		Type:       EventTypeImageMaterialized,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: ImageMaterializedPayload{
			Identity:    id,
			Model:       model,
			ArtifactRef: stub.Ref,
			MIME:        stub.MIME,
			SizeBytes:   stub.SizeBytes,
			OccurredAt:  time.Now(),
		},
	})
}
