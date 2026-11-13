package artifacts

import "github.com/hurtener/Harbor/internal/events"

// EventTypeArtifactNoteSet is published when an artifact's note is added,
// replaced or cleared. It is the audit-visible record of the change.
const EventTypeArtifactNoteSet events.EventType = "artifacts.note_set"

func init() {
	events.RegisterEventType(EventTypeArtifactNoteSet)
}

// ArtifactNoteSetPayload is the typed payload of artifacts.note_set.
// SafePayload: the content-addressed id and the note's size only, never the
// note text (it is free text the model or a caller wrote).
type ArtifactNoteSetPayload struct {
	events.SafeSealed
	// ArtifactID is the content-addressed identifier of the artifact.
	ArtifactID string `json:"artifact_id"`
	// NoteBytes is the new note's length; zero when the note was cleared.
	NoteBytes int `json:"note_bytes"`
	// Cleared reports that the note was removed.
	Cleared bool `json:"cleared"`
}
