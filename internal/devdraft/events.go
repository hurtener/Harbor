package devdraft

import (
	"github.com/hurtener/Harbor/internal/events"
)

// Draft lifecycle event types. Each is registered with the events
// package's exhaustive registry via init() so Publish accepts them
// without ErrUnknownEventType. Identity lives on the Event itself,
// intentionally not duplicated on the payload.
const (
	// EventTypeDraftCreated — emitted by Store.Create after the
	// draft tree has been materialised under the identity-scoped
	// path. Payload is DraftCreatedPayload (SafePayload).
	EventTypeDraftCreated events.EventType = "dev.draft.created"
	// EventTypeDraftUpdated — emitted by Store.WriteFile after a
	// successful PATCH. Payload is DraftUpdatedPayload (SafePayload).
	EventTypeDraftUpdated events.EventType = "dev.draft.updated"
	// EventTypeDraftPreviewed — emitted by Store.Preview after a
	// preview run terminates. Payload is DraftPreviewedPayload
	// (SafePayload).
	EventTypeDraftPreviewed events.EventType = "dev.draft.previewed"
	// EventTypeDraftSaved — emitted by Store.Save after a successful
	// promotion to the scaffold output dir. Payload is
	// DraftSavedPayload (SafePayload).
	EventTypeDraftSaved events.EventType = "dev.draft.saved"
	// EventTypeDraftDiscarded — emitted by Store.Discard after the
	// draft tree has been removed. Payload is DraftDiscardedPayload
	// (SafePayload).
	EventTypeDraftDiscarded events.EventType = "dev.draft.discarded"
)

func init() {
	events.RegisterEventType(EventTypeDraftCreated)
	events.RegisterEventType(EventTypeDraftUpdated)
	events.RegisterEventType(EventTypeDraftPreviewed)
	events.RegisterEventType(EventTypeDraftSaved)
	events.RegisterEventType(EventTypeDraftDiscarded)
}

// DraftCreatedPayload reports a successful Store.Create. DraftID is
// the opaque ULID minted by the Store; FileCount is the number of
// files seeded from the chosen template. SafePayload by construction.
type DraftCreatedPayload struct {
	events.SafeSealed
	DraftID   string
	Template  string
	FileCount int
}

// DraftUpdatedPayload reports a successful Store.WriteFile. Path is
// the slash-separated rel-path the operator submitted (already
// path-traversal-checked at the boundary). Size is the number of
// bytes written. SafePayload by construction — neither field carries
// secret-shaped material; file contents themselves NEVER reach the
// bus.
type DraftUpdatedPayload struct {
	events.SafeSealed
	DraftID string
	Path    string
	Size    int
}

// DraftPreviewedPayload reports a Store.Preview call. The payload is
// deliberately small — Phase 66's preview path is a stub that
// validates the draft tree (specifically the rendered harbor.yaml)
// and reports whether it would boot. Concrete dry-run execution
// lands in a later phase; the event shape is stable across that
// upgrade. SafePayload by construction.
type DraftPreviewedPayload struct {
	events.SafeSealed
	DraftID string
	OK      bool
}

// DraftSavedPayload reports a successful Store.Save promotion to the
// scaffold output dir. OutputDir is the absolute path Save wrote to;
// FileCount is the number of files materialised. SafePayload by
// construction — OutputDir is the operator's own working-dir, not
// secret-shaped.
type DraftSavedPayload struct {
	events.SafeSealed
	DraftID   string
	OutputDir string
	FileCount int
}

// DraftDiscardedPayload reports a successful Store.Discard. The
// draft tree was removed from the operator's working dir.
// SafePayload by construction.
type DraftDiscardedPayload struct {
	events.SafeSealed
	DraftID string
}
