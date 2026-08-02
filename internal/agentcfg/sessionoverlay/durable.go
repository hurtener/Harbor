package sessionoverlay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// MaxSessionSkillReadAttempts bounds a stable session-skill read. A writer
// that continually changes lifecycle or erasure fences must not yield a
// partially stable result.
const MaxSessionSkillReadAttempts = 3

const (
	// MaxSessionPersonalRecordBytes bounds one durable personal-skill record
	// before storage and after retrieval.
	MaxSessionPersonalRecordBytes = 256 * 1024
	// MaxLegacySessionOverlayRecordBytes bounds one compatibility envelope
	// before strict decoding at the migration authority boundary.
	MaxLegacySessionOverlayRecordBytes = 256 * 1024
	// MaxAgentLifecycleFenceBytes bounds the active-pointer compatibility
	// envelope before it can establish session-owned authority.
	MaxAgentLifecycleFenceBytes = 64 * 1024
	// MaxSessionPersonalCopyEpochBytes bounds the operator-declared copy epoch
	// stamped onto records copied from schema-1 overlays.
	MaxSessionPersonalCopyEpochBytes = 128
	// MaxSessionPersonalCutoverRecordBytes bounds the durable controller
	// checkpoint, including its opaque canonical tenant-scan continuation.
	MaxSessionPersonalCutoverRecordBytes = 4 * 1024
	// MaxSessionPersonalCutoverCounter bounds copied-row and checkpoint
	// generations so corrupt durable values cannot overflow later increments.
	MaxSessionPersonalCutoverCounter = 1<<31 - 1
)

const (
	personalKindPrefix = "agentcfg.session_personal.v1."
	cutoverKindPrefix  = "agentcfg.session_personal.cutover."
	legacyKindPrefix   = "agentcfg.session_overlay."
	cutoverUser        = "__agentcfg__"
	cutoverSession     = "__session_personal_cutover__"
)

var (
	// ErrSessionSkillReadUnstable means all bounded before/after fence reads
	// observed concurrent lifecycle or erasure changes.
	ErrSessionSkillReadUnstable = errors.New("agentcfg/sessionoverlay: session skill read unstable")
	// ErrSessionErased means a pending or terminal session-erasure fence is
	// present and refuses an overlay or personal-skill operation.
	ErrSessionErased = errors.New("agentcfg/sessionoverlay: session is being erased or was erased")
	// ErrAgentLifecycleInactive means the durable lifecycle slot is absent or
	// not the active compatibility envelope. Session-owned state must never
	// create authority for an unresolvable agent.
	ErrAgentLifecycleInactive = errors.New("agentcfg/sessionoverlay: agent lifecycle is not active")
	// ErrPersonalRecordInvalid means a stored personal record did not prove the
	// identity encoded by its key and is therefore unsafe to use.
	ErrPersonalRecordInvalid = errors.New("agentcfg/sessionoverlay: personal skill record invalid")
	// ErrCutoverRecordInvalid means a durable cutover record is malformed or
	// does not exactly match the operator declaration. Callers stay dual-read.
	ErrCutoverRecordInvalid = errors.New("agentcfg/sessionoverlay: cutover record invalid")
	// ErrCutoverPending means a tenant has not completed the declared durable
	// migration and session-personal mutation must remain refused.
	ErrCutoverPending = errors.New("agentcfg/sessionoverlay: session skill cutover pending")
	// ErrLegacyOverlayInvalid means a row returned by the schema-1 legacy scan
	// does not prove the exact session-scoped envelope it claims to represent.
	// Cutover must remain dual-read rather than silently skipping the row.
	ErrLegacyOverlayInvalid = errors.New("agentcfg/sessionoverlay: legacy overlay invalid")
)

// PersonalSkillRecord is one agent-owned durable session-personal body. A
// tombstone is authoritative and suppresses legacy fallback for its name.
type PersonalSkillRecord struct {
	Schema            int          `json:"schema"`
	AgentID           string       `json:"agent_id"`
	CanonicalName     string       `json:"canonical_name"`
	ContentHash       string       `json:"content_hash"`
	Deleted           bool         `json:"deleted,omitempty"`
	Skill             skills.Skill `json:"skill"`
	CopyEpoch         string       `json:"copy_epoch,omitempty"`
	LegacyContentHash string       `json:"legacy_content_hash,omitempty"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

// DurableStore persists per-session overlays and agent-owned personal records
// through StateStore.SaveIf. It has no process-local correctness lock: two
// processes contend only through the mandatory storage CAS.
type DurableStore struct {
	state state.StateStore
	clock func() time.Time
}

// NewDurableStore builds the durable session-personal persistence surface.
func NewDurableStore(st state.StateStore, clock func() time.Time) (*DurableStore, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if clock == nil {
		clock = time.Now
	}
	return &DurableStore{state: st, clock: clock}, nil
}

// LegacyOverlayKind returns the schema-1 overlay kind. It intentionally uses
// the historic raw suffix; maintenance callers must scan the common prefix and
// compare exact Kind equality, never use an agent-specific prefix.
func LegacyOverlayKind(agentID string) string { return legacyKindPrefix + agentID }

// LegacyOverlayPrefix returns the tenant-scan prefix shared by all schema-1
// overlays.
func LegacyOverlayPrefix() string { return legacyKindPrefix }

// PersonalSkillKind returns a collision-resistant key for one agent/name pair.
// The payload independently repeats both values and is verified on every read.
func PersonalSkillKind(agentID, canonicalName string) (string, error) {
	if strings.TrimSpace(agentID) == "" || canonicalNameFor(canonicalName) == "" {
		return "", fmt.Errorf("%w: agent and canonical name are required", ErrInvalidInput)
	}
	encodedAgent := base64.RawURLEncoding.EncodeToString([]byte(agentID))
	digest := sha256.Sum256([]byte(canonicalNameFor(canonicalName)))
	return personalKindPrefix + encodedAgent + "." + hex.EncodeToString(digest[:]), nil
}

// PersonalSkillPrefix returns the collision-safe exact per-agent prefix used
// by retirement maintenance after lifecycle tombstoning.
func PersonalSkillPrefix(agentID string) (string, error) {
	if strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("%w: agent id is required", ErrInvalidInput)
	}
	return personalKindPrefix + base64.RawURLEncoding.EncodeToString([]byte(agentID)) + ".", nil
}

// LoadPersonal returns a stable personal record for id/agent/name.
func (s *DurableStore) LoadPersonal(ctx context.Context, id identity.Quadruple, agentID, name string) (PersonalSkillRecord, bool, error) {
	kind, err := PersonalSkillKind(agentID, name)
	if err != nil {
		return PersonalSkillRecord{}, false, err
	}
	for range MaxSessionSkillReadAttempts {
		if err := ctx.Err(); err != nil {
			return PersonalSkillRecord{}, false, err
		}
		fences, err := loadFences(ctx, s.state, id, agentID)
		if err != nil {
			return PersonalSkillRecord{}, false, err
		}
		if fences.erased() {
			return PersonalSkillRecord{}, false, ErrSessionErased
		}
		if !fences.active {
			return PersonalSkillRecord{}, false, ErrAgentLifecycleInactive
		}
		record, found, err := s.loadPersonal(ctx, id, kind, agentID, name)
		if err != nil {
			return PersonalSkillRecord{}, false, err
		}
		stable, err := fences.stable(ctx, s.state)
		if err != nil {
			return PersonalSkillRecord{}, false, err
		}
		if stable {
			return record, found, nil
		}
		if err := ctx.Err(); err != nil {
			return PersonalSkillRecord{}, false, err
		}
	}
	return PersonalSkillRecord{}, false, ErrSessionSkillReadUnstable
}

// SavePersonal validates and conditionally writes one complete owned record.
// It writes only the personal record; schema-1 Overlay.PersonalSkills remains
// read-only legacy migration input.
func (s *DurableStore) SavePersonal(ctx context.Context, id identity.Quadruple, agentID string, skill skills.Skill, copyEpoch, legacyContentHash string) (PersonalSkillRecord, error) {
	if err := validateSessionInput(id, agentID); err != nil {
		return PersonalSkillRecord{}, err
	}
	if err := skill.Validate(); err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if skill.Scope != skills.ScopeSession {
		return PersonalSkillRecord{}, fmt.Errorf("%w: session personal skill scope must be %q", ErrInvalidInput, skills.ScopeSession)
	}
	if err := validateCopyMarkers(copyEpoch, legacyContentHash); err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	canonicalName := canonicalNameFor(skill.Name)
	kind, err := PersonalSkillKind(agentID, canonicalName)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	fences, err := loadFences(ctx, s.state, id, agentID)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	if fences.erased() {
		return PersonalSkillRecord{}, ErrSessionErased
	}
	if !fences.active {
		return PersonalSkillRecord{}, ErrAgentLifecycleInactive
	}
	target, err := slotExpectation(ctx, s.state, durableSessionQuad(id), kind)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	contentHash := skills.CanonicalContentHash(skill)
	record := PersonalSkillRecord{Schema: 1, AgentID: agentID, CanonicalName: canonicalName, ContentHash: contentHash, Skill: skill, CopyEpoch: copyEpoch, LegacyContentHash: legacyContentHash, UpdatedAt: s.clock().UTC()}
	bytes, err := json.Marshal(record)
	if err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("agentcfg/sessionoverlay: marshal personal skill: %w", err)
	}
	if len(bytes) > MaxSessionPersonalRecordBytes {
		return PersonalSkillRecord{}, fmt.Errorf("%w: personal skill record is %d bytes, maximum is %d", ErrInvalidInput, len(bytes), MaxSessionPersonalRecordBytes)
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: durableSessionQuad(id), Kind: kind, Bytes: bytes, UpdatedAt: record.UpdatedAt}
	expectations := append([]state.SlotExpectation{target}, fences.expectations()...)
	if err := s.state.SaveIf(ctx, expectations, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return PersonalSkillRecord{}, err
		}
		converged, ok, reconcileErr := s.exactPersonal(ctx, id, kind, next, agentID, canonicalName, fences)
		if reconcileErr == nil && ok {
			return converged, nil
		}
		if reconcileErr != nil {
			return PersonalSkillRecord{}, fmt.Errorf("%w: conditional personal save: %w; exact reread: %w", ErrStateUnavailable, err, reconcileErr)
		}
		return PersonalSkillRecord{}, fmt.Errorf("%w: conditional personal save outcome uncertain: %w", ErrStateUnavailable, err)
	}
	return record, nil
}

// DeletePersonal writes a logical tombstone. The tombstone, not an absent
// record, prevents a deleted legacy name from reappearing after cutover.
func (s *DurableStore) DeletePersonal(ctx context.Context, id identity.Quadruple, agentID, name string) (PersonalSkillRecord, error) {
	if err := validateSessionInput(id, agentID); err != nil {
		return PersonalSkillRecord{}, err
	}
	canonicalName := canonicalNameFor(name)
	kind, err := PersonalSkillKind(agentID, canonicalName)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	fences, err := loadFences(ctx, s.state, id, agentID)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	if fences.erased() {
		return PersonalSkillRecord{}, ErrSessionErased
	}
	if !fences.active {
		return PersonalSkillRecord{}, ErrAgentLifecycleInactive
	}
	target, err := slotExpectation(ctx, s.state, durableSessionQuad(id), kind)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	record := PersonalSkillRecord{Schema: 1, AgentID: agentID, CanonicalName: canonicalName, Deleted: true, UpdatedAt: s.clock().UTC()}
	bytes, err := json.Marshal(record)
	if err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("agentcfg/sessionoverlay: marshal personal tombstone: %w", err)
	}
	if len(bytes) > MaxSessionPersonalRecordBytes {
		return PersonalSkillRecord{}, fmt.Errorf("%w: personal tombstone is %d bytes, maximum is %d", ErrInvalidInput, len(bytes), MaxSessionPersonalRecordBytes)
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: durableSessionQuad(id), Kind: kind, Bytes: bytes, UpdatedAt: record.UpdatedAt}
	expectations := append([]state.SlotExpectation{target}, fences.expectations()...)
	if err := s.state.SaveIf(ctx, expectations, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return PersonalSkillRecord{}, err
		}
		converged, ok, reconcileErr := s.exactPersonal(ctx, id, kind, next, agentID, canonicalName, fences)
		if reconcileErr == nil && ok {
			return converged, nil
		}
		if reconcileErr != nil {
			return PersonalSkillRecord{}, fmt.Errorf("%w: conditional personal delete: %w; exact reread: %w", ErrStateUnavailable, err, reconcileErr)
		}
		return PersonalSkillRecord{}, fmt.Errorf("%w: conditional personal delete outcome uncertain: %w", ErrStateUnavailable, err)
	}
	return record, nil
}

func (s *DurableStore) exactPersonal(ctx context.Context, id identity.Quadruple, kind string, expected state.StateRecord, agentID, canonicalName string, before fences) (PersonalSkillRecord, bool, error) {
	rec, err := s.state.Load(ctx, durableSessionQuad(id), kind)
	if err != nil {
		return PersonalSkillRecord{}, false, err
	}
	if rec.ID != expected.ID || string(rec.Bytes) != string(expected.Bytes) {
		return PersonalSkillRecord{}, false, nil
	}
	after, err := loadFences(ctx, s.state, id, agentID)
	if err != nil {
		return PersonalSkillRecord{}, false, err
	}
	if !after.active || after.erased() || !before.equal(after) {
		return PersonalSkillRecord{}, false, nil
	}
	return decodePersonal(rec.Bytes, agentID, canonicalName)
}

func (s *DurableStore) loadPersonal(ctx context.Context, id identity.Quadruple, kind, agentID, name string) (PersonalSkillRecord, bool, error) {
	rec, err := s.state.Load(ctx, durableSessionQuad(id), kind)
	if errors.Is(err, state.ErrNotFound) {
		return PersonalSkillRecord{}, false, nil
	}
	if err != nil {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: load personal skill: %w", ErrStateUnavailable, err)
	}
	decoded, found, err := decodePersonal(rec.Bytes, agentID, canonicalNameFor(name))
	return decoded, found, err
}

func decodePersonal(bytes []byte, agentID, canonicalName string) (PersonalSkillRecord, bool, error) {
	if len(bytes) == 0 || len(bytes) > MaxSessionPersonalRecordBytes {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: record size %d is outside 1..%d", ErrPersonalRecordInvalid, len(bytes), MaxSessionPersonalRecordBytes)
	}
	var envelope struct {
		Schema            *int            `json:"schema"`
		AgentID           *string         `json:"agent_id"`
		CanonicalName     *string         `json:"canonical_name"`
		ContentHash       *string         `json:"content_hash"`
		Deleted           *bool           `json:"deleted"`
		Skill             json.RawMessage `json:"skill"`
		CopyEpoch         *string         `json:"copy_epoch"`
		LegacyContentHash *string         `json:"legacy_content_hash"`
		UpdatedAt         *time.Time      `json:"updated_at"`
	}
	if err := decodeStrictJSON(bytes, &envelope); err != nil {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: decode: %w", ErrPersonalRecordInvalid, err)
	}
	if envelope.Schema == nil || envelope.AgentID == nil || envelope.CanonicalName == nil || envelope.ContentHash == nil || envelope.UpdatedAt == nil || len(envelope.Skill) == 0 || string(envelope.Skill) == "null" {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: required field is absent", ErrPersonalRecordInvalid)
	}
	var skill skills.Skill
	if err := decodeStrictJSON(envelope.Skill, &skill); err != nil {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: decode skill body: %w", ErrPersonalRecordInvalid, err)
	}
	record := PersonalSkillRecord{
		Schema:        *envelope.Schema,
		AgentID:       *envelope.AgentID,
		CanonicalName: *envelope.CanonicalName,
		ContentHash:   *envelope.ContentHash,
		Skill:         skill,
		UpdatedAt:     *envelope.UpdatedAt,
	}
	if envelope.CopyEpoch != nil {
		record.CopyEpoch = *envelope.CopyEpoch
	}
	if envelope.LegacyContentHash != nil {
		record.LegacyContentHash = *envelope.LegacyContentHash
	}
	if envelope.Deleted != nil {
		record.Deleted = *envelope.Deleted
	}
	if record.Schema != 1 || record.AgentID != agentID || record.CanonicalName != canonicalName || record.UpdatedAt.IsZero() {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: key/payload mismatch", ErrPersonalRecordInvalid)
	}
	if envelope.Deleted != nil {
		if !record.Deleted || record.ContentHash != "" || envelope.CopyEpoch != nil || envelope.LegacyContentHash != nil || !reflect.DeepEqual(record.Skill, skills.Skill{}) {
			return PersonalSkillRecord{}, false, fmt.Errorf("%w: tombstone carries live body or copy metadata", ErrPersonalRecordInvalid)
		}
		return record, true, nil
	}
	if (envelope.CopyEpoch == nil) != (envelope.LegacyContentHash == nil) {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: copy marker field presence mismatch", ErrPersonalRecordInvalid)
	}
	if envelope.CopyEpoch != nil && (*envelope.CopyEpoch == "" || *envelope.LegacyContentHash == "") {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: present copy markers must be non-empty", ErrPersonalRecordInvalid)
	}
	if err := validateCopyMarkers(record.CopyEpoch, record.LegacyContentHash); err != nil {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: %w", ErrPersonalRecordInvalid, err)
	}
	if err := record.Skill.Validate(); err != nil || record.Skill.Scope != skills.ScopeSession || canonicalNameFor(record.Skill.Name) != canonicalName || !validCanonicalSHA256(record.ContentHash) || record.ContentHash != skills.CanonicalContentHash(record.Skill) {
		return PersonalSkillRecord{}, false, fmt.Errorf("%w: body validation failed", ErrPersonalRecordInvalid)
	}
	return record, true, nil
}

func validateCopyMarkers(copyEpoch, legacyContentHash string) error {
	if (copyEpoch == "") != (legacyContentHash == "") {
		return errors.New("copy epoch and legacy content hash must both be present or both be absent")
	}
	if copyEpoch != "" && !validCutoverToken(copyEpoch, MaxSessionPersonalCopyEpochBytes) {
		return fmt.Errorf("copy epoch must be canonical printable ASCII bounded to %d bytes", MaxSessionPersonalCopyEpochBytes)
	}
	if legacyContentHash != "" && !validCanonicalSHA256(legacyContentHash) {
		return errors.New("legacy content hash must be canonical lowercase SHA-256 hex")
	}
	return nil
}

func validCanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type fences struct {
	lifecycle state.SlotExpectation
	pending   state.SlotExpectation
	tombstone state.SlotExpectation
	active    bool
}

func loadFences(ctx context.Context, st state.StateStore, id identity.Quadruple, agentID string) (fences, error) {
	if err := ctx.Err(); err != nil {
		return fences{}, err
	}
	if err := validateSessionInput(id, agentID); err != nil {
		return fences{}, err
	}
	lifecycleQ, lifecycleKind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
	if err != nil {
		return fences{}, err
	}
	pendingQ, pendingKind, err := sessionfence.PendingSlot(durableSessionQuad(id))
	if err != nil {
		return fences{}, err
	}
	tombstoneQ, tombstoneKind, err := sessionfence.TombstoneSlot(durableSessionQuad(id))
	if err != nil {
		return fences{}, err
	}
	lifecycle, lifecycleBytes, err := slotExpectationWithBytes(ctx, st, lifecycleQ, lifecycleKind)
	if err != nil {
		return fences{}, err
	}
	pending, err := slotExpectation(ctx, st, pendingQ, pendingKind)
	if err != nil {
		return fences{}, err
	}
	tombstone, err := slotExpectation(ctx, st, tombstoneQ, tombstoneKind)
	if err != nil {
		return fences{}, err
	}
	return fences{lifecycle: lifecycle, pending: pending, tombstone: tombstone, active: activeLifecycleEnvelope(lifecycleBytes)}, nil
}

func (f fences) expectations() []state.SlotExpectation {
	return []state.SlotExpectation{f.lifecycle, f.pending, f.tombstone}
}
func (f fences) erased() bool {
	return f.pending.ExpectedEventID != "" || f.tombstone.ExpectedEventID != ""
}
func (f fences) equal(other fences) bool {
	return f.active == other.active && f.lifecycle.ExpectedEventID == other.lifecycle.ExpectedEventID && f.pending.ExpectedEventID == other.pending.ExpectedEventID && f.tombstone.ExpectedEventID == other.tombstone.ExpectedEventID
}
func (f fences) stable(ctx context.Context, st state.StateStore) (bool, error) {
	for _, before := range f.expectations() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		after, err := slotExpectation(ctx, st, before.Identity, before.Kind)
		if err != nil {
			return false, err
		}
		if after.ExpectedEventID != before.ExpectedEventID {
			return false, nil
		}
	}
	return true, nil
}

func slotExpectation(ctx context.Context, st state.StateStore, q identity.Quadruple, kind string) (state.SlotExpectation, error) {
	expectation, _, err := slotExpectationWithBytes(ctx, st, q, kind)
	return expectation, err
}

func slotExpectationWithBytes(ctx context.Context, st state.StateStore, q identity.Quadruple, kind string) (state.SlotExpectation, []byte, error) {
	if err := ctx.Err(); err != nil {
		return state.SlotExpectation{}, nil, err
	}
	rec, err := st.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return state.SlotExpectation{Identity: q, Kind: kind}, nil, nil
	}
	if err != nil {
		return state.SlotExpectation{}, nil, fmt.Errorf("%w: load fence: %w", ErrStateUnavailable, err)
	}
	return state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: rec.ID}, rec.Bytes, nil
}

type lifecycleEnvelopeState uint8

const (
	lifecycleEnvelopeInvalid lifecycleEnvelopeState = iota
	lifecycleEnvelopeActive
	lifecycleEnvelopeTerminal
)

// activeLifecycleEnvelope is the compatibility reader for the established
// active-pointer envelope. Only one bounded JSON document containing the
// known schema/revision/timestamp fields can establish active authority.
// Empty compatible pointers are terminal; malformed, retired-shaped, future,
// unknown-field, oversized, and trailing-document records are invalid. Both
// terminal and invalid states fail closed here.
func activeLifecycleEnvelope(bytes []byte) bool {
	return decodeLifecycleEnvelope(bytes) == lifecycleEnvelopeActive
}

func decodeLifecycleEnvelope(data []byte) lifecycleEnvelopeState {
	if len(data) == 0 || len(data) > MaxAgentLifecycleFenceBytes {
		return lifecycleEnvelopeInvalid
	}
	var envelope struct {
		Schema     int       `json:"schema"`
		RevisionID string    `json:"revision_id"`
		UpdatedAt  time.Time `json:"updated_at"`
	}
	if err := decodeStrictJSON(data, &envelope); err != nil || (envelope.Schema != 0 && envelope.Schema != 1) {
		return lifecycleEnvelopeInvalid
	}
	if envelope.Schema == 1 && envelope.UpdatedAt.IsZero() {
		return lifecycleEnvelopeInvalid
	}
	if envelope.RevisionID == "" {
		return lifecycleEnvelopeTerminal
	}
	if envelope.RevisionID != strings.TrimSpace(envelope.RevisionID) {
		return lifecycleEnvelopeInvalid
	}
	return lifecycleEnvelopeActive
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON document")
	}
	return nil
}

func durableSessionQuad(id identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: id.Identity}
}
func validateSessionInput(id identity.Quadruple, agentID string) error {
	if err := identity.Validate(id.Identity); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: agent id is empty", ErrIdentityRequired)
	}
	return nil
}
func canonicalNameFor(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// CutoverScope returns the exact reserved StateStore identity for a tenant's
// cutover control record.
func CutoverScope(tenantID string) (identity.Quadruple, error) {
	if strings.TrimSpace(tenantID) == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: tenant id is empty", ErrInvalidInput)
	}
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenantID, UserID: cutoverUser, SessionID: cutoverSession}}, nil
}

// CutoverKind names one epoch's durable control record.
func CutoverKind(epoch string) (string, error) {
	if strings.TrimSpace(epoch) == "" {
		return "", fmt.Errorf("%w: epoch is empty", ErrInvalidInput)
	}
	return cutoverKindPrefix + base64.RawURLEncoding.EncodeToString([]byte(epoch)), nil
}

// CutoverMode declares whether session-personal records are still non-authoritative copies.
type CutoverMode string

const (
	CutoverDualRead  CutoverMode = "dual_read"
	CutoverStateOnly CutoverMode = "state_only"
)

// CutoverRecord is the bounded durable controller checkpoint. It deliberately
// contains no per-overlay classification; copied records carry their markers.
type CutoverRecord struct {
	Schema       int         `json:"schema"`
	Mode         CutoverMode `json:"mode"`
	Epoch        string      `json:"epoch"`
	RosterDigest string      `json:"roster_digest"`
	Continuation string      `json:"continuation,omitempty"`
	Copied       int         `json:"copied"`
	Generation   int         `json:"generation"`
}

// CutoverController advances only the finite operator-declared tenant set.
// It is safe for concurrent processes because every checkpoint uses SaveIf.
type CutoverController struct {
	state        state.StateStore
	declarations map[string]config.SessionPersonalCutoverTenant
}

// LegacyMigrator owns the bridge from a schema-1 overlay reference to an
// owned personal record. The controller deliberately does not import a
// SkillStore: the caller performs each identity-exact copy with the four
// session fences, while the controller owns only ordered scan progress.
type LegacyMigrator interface {
	CopyLegacyOverlay(ctx context.Context, record state.StateRecord, declaration config.SessionPersonalCutoverTenant) (int, error)
	VerifyLegacyOverlay(ctx context.Context, record state.StateRecord, declaration config.SessionPersonalCutoverTenant) (bool, error)
}

// NewCutoverController validates the supplied static declarations again so a
// direct Go embedding cannot bypass the loader's boot validation.
func NewCutoverController(st state.StateStore, declarations []config.SessionPersonalCutoverTenant) (*CutoverController, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if len(declarations) > 256 {
		return nil, fmt.Errorf("%w: too many tenant declarations", ErrInvalidInput)
	}
	result := make(map[string]config.SessionPersonalCutoverTenant, len(declarations))
	for _, declaration := range declarations {
		if !validCutoverToken(declaration.TenantID, 128) || !validCutoverToken(declaration.Epoch, 128) || !validCutoverToken(declaration.RosterDigest, 256) {
			return nil, fmt.Errorf("%w: empty static cutover declaration", ErrInvalidInput)
		}
		if _, exists := result[declaration.TenantID]; exists {
			return nil, fmt.Errorf("%w: duplicate tenant %q", ErrInvalidInput, declaration.TenantID)
		}
		result[declaration.TenantID] = declaration
	}
	return &CutoverController{state: st, declarations: result}, nil
}

// Mode returns dual-read for unlisted or undrained tenants. A malformed or
// mismatched durable record never grants state-only authority.
func (c *CutoverController) Mode(ctx context.Context, tenantID string) (CutoverMode, error) {
	if err := ctx.Err(); err != nil {
		return CutoverDualRead, err
	}
	declaration, listed := c.declarations[tenantID]
	if !listed || !declaration.LegacyWritersDrained {
		return CutoverDualRead, nil
	}
	record, found, err := c.load(ctx, declaration)
	if err != nil {
		return CutoverDualRead, err
	}
	if !found {
		return CutoverDualRead, nil
	}
	if err := validateCutoverRecord(record, declaration); err != nil {
		return CutoverDualRead, err
	}
	return record.Mode, nil
}

// Ensure records the initial dual-read checkpoint for every drained declared
// tenant. It never discovers or scans unlisted tenants.
func (c *CutoverController) Ensure(ctx context.Context) error {
	for _, declaration := range c.declarations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !declaration.LegacyWritersDrained {
			continue
		}
		if _, found, err := c.load(ctx, declaration); err != nil {
			return err
		} else if found {
			continue
		}
		q, err := CutoverScope(declaration.TenantID)
		if err != nil {
			return err
		}
		kind, err := CutoverKind(declaration.Epoch)
		if err != nil {
			return err
		}
		record := CutoverRecord{Schema: 1, Mode: CutoverDualRead, Epoch: declaration.Epoch, RosterDigest: declaration.RosterDigest}
		bytes, err := json.Marshal(record)
		if err != nil {
			return err
		}
		next := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: bytes}
		err = c.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind}}, next)
		if err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				winner, found, loadErr := c.load(ctx, declaration)
				if loadErr != nil {
					return fmt.Errorf("%w: initialize cutover concurrent winner: %w", ErrCutoverRecordInvalid, loadErr)
				}
				if !found {
					return fmt.Errorf("%w: initialize cutover condition failed without a durable winner", ErrCutoverRecordInvalid)
				}
				if err := validateCutoverRecord(winner, declaration); err != nil {
					return fmt.Errorf("%w: initialize cutover concurrent winner: %w", ErrCutoverRecordInvalid, err)
				}
				continue
			}
			ok, reconcileErr := c.exactCutover(ctx, next, declaration)
			if reconcileErr == nil && ok {
				continue
			}
			if reconcileErr != nil {
				return fmt.Errorf("%w: initialize cutover: %w; exact reread: %w", ErrStateUnavailable, err, reconcileErr)
			}
			return fmt.Errorf("%w: initialize cutover outcome uncertain: %w", ErrStateUnavailable, err)
		}
	}
	return nil
}

// Advance copies at most one bounded scan page for tenant and checkpoints its
// opaque continuation. Once the first pass reaches the end, it performs a
// fresh verification scan from the beginning; only that fresh pass may set
// state_only. The underlying StateStore scan is deliberately not a snapshot.
func (c *CutoverController) Advance(ctx context.Context, tenantID string, limit int, migrator LegacyMigrator) (CutoverMode, error) {
	if err := ctx.Err(); err != nil {
		return CutoverDualRead, err
	}
	if migrator == nil {
		return CutoverDualRead, fmt.Errorf("%w: legacy migrator is required", ErrInvalidConfig)
	}
	declaration, listed := c.declarations[tenantID]
	if !listed || !declaration.LegacyWritersDrained {
		return CutoverDualRead, nil
	}
	if err := c.Ensure(ctx); err != nil {
		return CutoverDualRead, err
	}
	record, slot, err := c.loadWithSlot(ctx, declaration)
	if err != nil {
		return CutoverDualRead, err
	}
	if record.Mode == CutoverStateOnly {
		return CutoverStateOnly, nil
	}
	q, err := CutoverScope(declaration.TenantID)
	if err != nil {
		return CutoverDualRead, err
	}
	page, err := c.state.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, declaration.TenantID, LegacyOverlayPrefix(), limit, record.Continuation)
	if err != nil {
		return CutoverDualRead, fmt.Errorf("%w: scan legacy overlays: %w", ErrStateUnavailable, err)
	}
	copied := 0
	for _, candidate := range page.Records {
		if err := validateLegacyOverlayCandidate(candidate, declaration.TenantID); err != nil {
			return CutoverDualRead, err
		}
		n, err := migrator.CopyLegacyOverlay(ctx, candidate, declaration)
		if err != nil {
			return CutoverDualRead, err
		}
		if n < 0 || n > MaxSessionPersonalCutoverCounter-copied {
			return CutoverDualRead, fmt.Errorf("%w: copied-row delta is outside the bounded counter", ErrCutoverRecordInvalid)
		}
		copied += n
	}
	next := record
	next.Continuation = page.Continuation
	if copied > MaxSessionPersonalCutoverCounter-next.Copied || next.Generation == MaxSessionPersonalCutoverCounter {
		return CutoverDualRead, fmt.Errorf("%w: checkpoint counters exhausted", ErrCutoverRecordInvalid)
	}
	next.Copied += copied
	next.Generation++
	if err := c.saveRecord(ctx, q, slot, next); err != nil {
		return CutoverDualRead, err
	}
	if next.Continuation != "" {
		return CutoverDualRead, nil
	}
	// A completed first pass is not authority. Verify from a fresh cursor so a
	// legacy writer that raced the first ordered scan remains visible.
	continuation := ""
	for {
		if err := ctx.Err(); err != nil {
			return CutoverDualRead, err
		}
		page, err := c.state.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, declaration.TenantID, LegacyOverlayPrefix(), limit, continuation)
		if err != nil {
			return CutoverDualRead, fmt.Errorf("%w: verify legacy overlays: %w", ErrStateUnavailable, err)
		}
		for _, candidate := range page.Records {
			if err := validateLegacyOverlayCandidate(candidate, declaration.TenantID); err != nil {
				return CutoverDualRead, err
			}
			complete, err := migrator.VerifyLegacyOverlay(ctx, candidate, declaration)
			if err != nil {
				return CutoverDualRead, err
			}
			if !complete {
				return CutoverDualRead, nil
			}
		}
		if page.Continuation == "" {
			break
		}
		continuation = page.Continuation
	}
	// Reload so an independent process's checkpoint cannot be overwritten.
	fresh, freshSlot, err := c.loadWithSlot(ctx, declaration)
	if err != nil {
		return CutoverDualRead, err
	}
	if fresh.Mode == CutoverStateOnly {
		return CutoverStateOnly, nil
	}
	if fresh.Continuation != "" {
		return CutoverDualRead, nil
	}
	if fresh.Generation == MaxSessionPersonalCutoverCounter {
		return CutoverDualRead, fmt.Errorf("%w: checkpoint generation exhausted", ErrCutoverRecordInvalid)
	}
	fresh.Mode = CutoverStateOnly
	fresh.Generation++
	if err := c.saveRecord(ctx, q, freshSlot, fresh); err != nil {
		return CutoverDualRead, err
	}
	return CutoverStateOnly, nil
}

func validateLegacyOverlayCandidate(candidate state.StateRecord, tenantID string) error {
	if err := state.ValidateRecord(candidate); err != nil {
		return fmt.Errorf("%w: record shape: %w", ErrLegacyOverlayInvalid, err)
	}
	if candidate.Identity.TenantID != tenantID || candidate.Identity.RunID != "" || candidate.Identity.UserID == cutoverUser || candidate.Identity.SessionID == cutoverSession {
		return fmt.Errorf("%w: legacy identity must be the exact tenant session scope with empty run id", ErrLegacyOverlayInvalid)
	}
	if len(candidate.Bytes) == 0 || len(candidate.Bytes) > MaxLegacySessionOverlayRecordBytes {
		return fmt.Errorf("%w: record size %d is outside 1..%d", ErrLegacyOverlayInvalid, len(candidate.Bytes), MaxLegacySessionOverlayRecordBytes)
	}
	if !strings.HasPrefix(candidate.Kind, legacyKindPrefix) {
		return fmt.Errorf("%w: kind %q is outside the legacy prefix", ErrLegacyOverlayInvalid, candidate.Kind)
	}
	agentID := strings.TrimPrefix(candidate.Kind, legacyKindPrefix)
	if !validCutoverToken(agentID, 128) || candidate.Kind != LegacyOverlayKind(agentID) {
		return fmt.Errorf("%w: invalid raw agent suffix", ErrLegacyOverlayInvalid)
	}
	var envelope struct {
		Schema    int             `json:"schema"`
		Overlay   json.RawMessage `json:"overlay"`
		UpdatedAt *time.Time      `json:"updated_at"`
	}
	if err := decodeStrictJSON(candidate.Bytes, &envelope); err != nil {
		return fmt.Errorf("%w: decode schema-1 envelope: %w", ErrLegacyOverlayInvalid, err)
	}
	if envelope.Schema != recordSchema || len(envelope.Overlay) == 0 || string(envelope.Overlay) == "null" || envelope.UpdatedAt == nil || envelope.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: incompatible schema-1 envelope", ErrLegacyOverlayInvalid)
	}
	var overlay Overlay
	if err := decodeStrictJSON(envelope.Overlay, &overlay); err != nil {
		return fmt.Errorf("%w: decode overlay body: %w", ErrLegacyOverlayInvalid, err)
	}
	for _, values := range [][]string{overlay.DisabledServers, overlay.DisabledTools, overlay.PersonalSkills} {
		if !sort.StringsAreSorted(values) {
			return fmt.Errorf("%w: overlay lists must be canonical sorted sets", ErrLegacyOverlayInvalid)
		}
		for i, value := range values {
			if value == "" || (i > 0 && values[i-1] == value) {
				return fmt.Errorf("%w: overlay lists must contain unique non-empty values", ErrLegacyOverlayInvalid)
			}
		}
	}
	return nil
}

func (c *CutoverController) loadWithSlot(ctx context.Context, declaration config.SessionPersonalCutoverTenant) (CutoverRecord, state.SlotExpectation, error) {
	q, err := CutoverScope(declaration.TenantID)
	if err != nil {
		return CutoverRecord{}, state.SlotExpectation{}, err
	}
	kind, err := CutoverKind(declaration.Epoch)
	if err != nil {
		return CutoverRecord{}, state.SlotExpectation{}, err
	}
	rec, err := c.state.Load(ctx, q, kind)
	if err != nil {
		return CutoverRecord{}, state.SlotExpectation{}, fmt.Errorf("%w: expected initialized cutover record: %w", ErrCutoverRecordInvalid, err)
	}
	record, err := decodeCutoverRecord(rec.Bytes, declaration)
	if err != nil {
		return CutoverRecord{}, state.SlotExpectation{}, err
	}
	return record, state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: rec.ID}, nil
}

func (c *CutoverController) saveRecord(ctx context.Context, q identity.Quadruple, slot state.SlotExpectation, record CutoverRecord) error {
	declaration, found := c.declarations[q.TenantID]
	if !found {
		return fmt.Errorf("%w: no static declaration for checkpoint tenant", ErrCutoverRecordInvalid)
	}
	if err := validateCutoverRecord(record, declaration); err != nil {
		return err
	}
	bytes, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("agentcfg/sessionoverlay: marshal cutover: %w", err)
	}
	if len(bytes) > MaxSessionPersonalCutoverRecordBytes {
		return fmt.Errorf("%w: encoded checkpoint is %d bytes, maximum is %d", ErrCutoverRecordInvalid, len(bytes), MaxSessionPersonalCutoverRecordBytes)
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: slot.Kind, Bytes: bytes}
	err = c.state.SaveIf(ctx, []state.SlotExpectation{slot}, next)
	if err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return err
		}
		ok, reconcileErr := c.exactCutover(ctx, next, declaration)
		if reconcileErr == nil && ok {
			return nil
		}
		if reconcileErr != nil {
			return fmt.Errorf("%w: save cutover checkpoint: %w; exact reread: %w", ErrStateUnavailable, err, reconcileErr)
		}
		return fmt.Errorf("%w: save cutover checkpoint outcome uncertain: %w", ErrStateUnavailable, err)
	}
	return nil
}

func (c *CutoverController) exactCutover(ctx context.Context, expected state.StateRecord, declaration config.SessionPersonalCutoverTenant) (bool, error) {
	actual, err := c.state.Load(ctx, expected.Identity, expected.Kind)
	if err != nil {
		return false, err
	}
	if actual.ID != expected.ID || string(actual.Bytes) != string(expected.Bytes) {
		return false, nil
	}
	if _, err := decodeCutoverRecord(actual.Bytes, declaration); err != nil {
		return false, err
	}
	return true, nil
}

func (c *CutoverController) load(ctx context.Context, declaration config.SessionPersonalCutoverTenant) (CutoverRecord, bool, error) {
	q, err := CutoverScope(declaration.TenantID)
	if err != nil {
		return CutoverRecord{}, false, err
	}
	kind, err := CutoverKind(declaration.Epoch)
	if err != nil {
		return CutoverRecord{}, false, err
	}
	rec, err := c.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return CutoverRecord{}, false, nil
	}
	if err != nil {
		return CutoverRecord{}, false, fmt.Errorf("%w: load cutover: %w", ErrStateUnavailable, err)
	}
	record, err := decodeCutoverRecord(rec.Bytes, declaration)
	if err != nil {
		return CutoverRecord{}, false, err
	}
	return record, true, nil
}

func decodeCutoverRecord(data []byte, declaration config.SessionPersonalCutoverTenant) (CutoverRecord, error) {
	if len(data) == 0 || len(data) > MaxSessionPersonalCutoverRecordBytes {
		return CutoverRecord{}, fmt.Errorf("%w: checkpoint size %d is outside 1..%d", ErrCutoverRecordInvalid, len(data), MaxSessionPersonalCutoverRecordBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record CutoverRecord
	if err := decoder.Decode(&record); err != nil {
		return CutoverRecord{}, fmt.Errorf("%w: decode: %w", ErrCutoverRecordInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CutoverRecord{}, fmt.Errorf("%w: trailing JSON document", ErrCutoverRecordInvalid)
	}
	if err := validateCutoverRecord(record, declaration); err != nil {
		return CutoverRecord{}, err
	}
	return record, nil
}

func validateCutoverRecord(record CutoverRecord, declaration config.SessionPersonalCutoverTenant) error {
	if record.Schema != 1 || (record.Mode != CutoverDualRead && record.Mode != CutoverStateOnly) || record.Epoch != declaration.Epoch || record.RosterDigest != declaration.RosterDigest || record.Generation < 0 || record.Generation > MaxSessionPersonalCutoverCounter || record.Copied < 0 || record.Copied > MaxSessionPersonalCutoverCounter {
		return fmt.Errorf("%w: declaration mismatch", ErrCutoverRecordInvalid)
	}
	if record.Generation == 0 && (record.Copied != 0 || record.Continuation != "") {
		return fmt.Errorf("%w: initial checkpoint cannot carry progress", ErrCutoverRecordInvalid)
	}
	if record.Mode == CutoverStateOnly {
		if record.Continuation != "" || record.Generation < 2 {
			return fmt.Errorf("%w: state-only checkpoint must be terminal", ErrCutoverRecordInvalid)
		}
		return nil
	}
	if record.Continuation != "" {
		scope := state.ListScope{MaintenanceScoped: true}
		if _, err := state.DecodeStateScanContinuation(record.Continuation, declaration.TenantID, LegacyOverlayPrefix(), scope); err != nil {
			return fmt.Errorf("%w: continuation is not bound to the declared legacy scan: %w", ErrCutoverRecordInvalid, err)
		}
	}
	return nil
}

func validCutoverToken(value string, maximum int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
