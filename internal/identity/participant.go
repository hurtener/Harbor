package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSpeakerBytes bounds a participant's display label.
const MaxSpeakerBytes = 128

// ErrParticipantInvalid rejects a participant whose user is empty or whose
// speaker label is not a bounded printable single-line string.
var ErrParticipantInvalid = errors.New("identity: participant invalid")

// Participant is the person who wrote the current turn of a session.
//
// A session belongs to exactly one (tenant, user) partition: storage,
// transcript, events and artifacts stay keyed by that partition's triple.
// When an issuer signs a bearer for another user of the same tenant that
// acts inside someone else's session, the working identity stays the
// session's partition and the Participant names the writer. Every
// user-keyed outbound resolution (credential exchange subject, per-user
// capability reconcile) reads the acting identity (ActingFrom), never the
// partition user.
//
// Participant is not an isolation principal and never appears in a storage
// filter. It is immutable request/run metadata.
type Participant struct {
	// UserID is the verified writer. It equals the partition user when the
	// partition owner writes in a labelled session.
	UserID string `json:"user_id"`
	// Speaker is an optional issuer-signed display label for the writer.
	Speaker string `json:"speaker,omitempty"`
}

// ValidateParticipant checks the participant shape.
func ValidateParticipant(p Participant) error {
	if strings.TrimSpace(p.UserID) == "" || len(p.UserID) > 256 {
		return fmt.Errorf("%w: user_id", ErrParticipantInvalid)
	}
	return ValidateSpeaker(p.Speaker)
}

// ValidateSpeaker accepts an empty label or a bounded printable single-line
// UTF-8 string with no leading or trailing whitespace.
func ValidateSpeaker(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > MaxSpeakerBytes || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return fmt.Errorf("%w: speaker", ErrParticipantInvalid)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: speaker", ErrParticipantInvalid)
		}
	}
	return nil
}

type participantKey struct{}

// WithParticipant seats the writer of the current request or run on ctx.
func WithParticipant(ctx context.Context, p Participant) (context.Context, error) {
	if err := ValidateParticipant(p); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, participantKey{}, p), nil
}

// ParticipantFrom returns the writer seated on ctx.
func ParticipantFrom(ctx context.Context) (Participant, bool) {
	p, ok := ctx.Value(participantKey{}).(Participant)
	return p, ok
}

// IsGuest reports whether p acts inside another user's partition.
func (p Participant) IsGuest(partition Identity) bool {
	return p.UserID != "" && p.UserID != partition.UserID
}

// ActingFrom returns the identity outbound user-keyed calls act as: the
// working identity with its user replaced by the participant's when one is
// seated. The bool reports whether a working identity is present. The
// second identity is the partition (working) identity, unchanged.
func ActingFrom(ctx context.Context) (acting Identity, partition Identity, ok bool) {
	partition, ok = From(ctx)
	if !ok {
		return Identity{}, Identity{}, false
	}
	acting = partition
	if p, has := ParticipantFrom(ctx); has && p.UserID != "" {
		acting.UserID = p.UserID
	}
	return acting, partition, true
}
