// Package publication implements Harbor's same-runtime organization skill
// publication contract (HA-68).
//
// A publication is an organization-owned, reviewed immutable skill revision.
// An agent receives only an exact reference to one revision; it never receives
// a mutable catalog pointer.  Metadata and mutation receipts are deliberately
// content-free.  Skill bodies can be obtained only through Resolve, which
// checks the runtime/deployment binding and the caller's target reference.
//
// The package has two implementations: MemoryStore is the reference
// implementation used by unit/conformance tests, and StateStoreStore stores
// the same aggregate through Harbor's mandatory StateStore.  The latter thus
// inherits the in-memory, SQLite, and PostgreSQL driver triad and restart
// semantics without inventing a publication-specific persistence interface.
package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	publicationStateActive  = "active"
	publicationStateRetired = "retired"
	publicationOrgUser      = "_harbor_publications"
	publicationOrgSession   = "organization"
	publicationOrgKind      = "skills.publications.organization.v1"
	publicationRefPrefix    = "skills.publications.reference.v1."
	publicationOriginPrefix = "skills.publications.origin.v1."
	publicationRefSession   = "references"
	maxPublications         = 1024
	maxRevisions            = 1024
)

// State values for a publication or reference.
type State string

const (
	StateActive  State = publicationStateActive
	StateRetired State = publicationStateRetired
)

// Publication is the immutable organization-owned revision. Skill is kept
// private to metadata projections; callers must use Resolve to obtain a body.
type Publication struct {
	PublicationID string       `json:"publication_id"`
	RevisionID    string       `json:"revision_id"`
	Name          string       `json:"name"`
	ContentHash   string       `json:"content_hash"`
	State         State        `json:"state"`
	Generation    uint64       `json:"generation"`
	RuntimeID     string       `json:"runtime_id"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Skill         skills.Skill `json:"skill"`
	// Revisions is the immutable body-bearing revision history. It never
	// appears in Metadata or mutation receipts.
	Revisions []Revision `json:"revisions,omitempty"`
}

// Revision is one immutable body-bearing publication revision. A successor
// appends a revision; it never mutates an earlier one.
type Revision struct {
	RevisionID  string       `json:"revision_id"`
	ContentHash string       `json:"content_hash"`
	Generation  uint64       `json:"generation"`
	RuntimeID   string       `json:"runtime_id"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Skill       skills.Skill `json:"skill"`
}

// Metadata is the content-free projection used by list/get/available. It
// intentionally contains no skill body, prompt text, or supporting bytes.
type Metadata struct {
	PublicationID string    `json:"publication_id"`
	RevisionID    string    `json:"revision_id"`
	Name          string    `json:"name"`
	ContentHash   string    `json:"content_hash"`
	State         State     `json:"state"`
	Generation    uint64    `json:"generation"`
	RuntimeID     string    `json:"runtime_id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Reference pins one exact publication revision to one target Agent. A
// reference is durable user state, not a copy of the skill body.
type Reference struct {
	AgentID       string    `json:"agent_id"`
	PublicationID string    `json:"publication_id"`
	RevisionID    string    `json:"revision_id"`
	ContentHash   string    `json:"content_hash"`
	Generation    uint64    `json:"generation"`
	RuntimeID     string    `json:"runtime_id"`
	State         State     `json:"state"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Receipt is a content-free mutation receipt. The operation key is safe to
// persist and replay; the skill body is never included.
type Receipt struct {
	OperationID   string    `json:"operation_id"`
	Operation     string    `json:"operation"`
	PublicationID string    `json:"publication_id,omitempty"`
	RevisionID    string    `json:"revision_id,omitempty"`
	Generation    uint64    `json:"generation"`
	State         State     `json:"state"`
	BeforeHash    string    `json:"before_hash,omitempty"`
	AfterHash     string    `json:"after_hash,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PublishRequest creates a new organization publication. Creation requires
// ExpectedAbsent=true; the expected hash is intentionally empty.
type PublishRequest struct {
	IdempotencyKey string
	Name           string
	Skill          skills.Skill
	ExpectedAbsent bool
}

// SuccessorRequest publishes an immutable successor revision for an existing
// publication. Both generation and hash are required exact CAS predicates.
type SuccessorRequest struct {
	IdempotencyKey      string
	PublicationID       string
	ExpectedGeneration  uint64
	ExpectedContentHash string
	Skill               skills.Skill
}

// RetireRequest terminally retires a publication. Retirement is not a grace
// period: future composition resolution fails closed.
type RetireRequest struct {
	IdempotencyKey      string
	PublicationID       string
	ExpectedGeneration  uint64
	ExpectedContentHash string
}

// InstallRequest installs an exact publication revision reference on one
// target Agent. The target is caller-selected but the publication is
// organization-owned and remains runtime-local.
type InstallRequest struct {
	IdempotencyKey string
	AgentID        string
	PublicationID  string
	RevisionID     string
	ExpectedAbsent bool
}

// UpdateRequest explicitly swaps a reference to an exact successor revision.
type UpdateRequest struct {
	IdempotencyKey      string
	AgentID             string
	PublicationID       string
	RevisionID          string
	ExpectedGeneration  uint64
	ExpectedContentHash string
}

// RemoveRequest removes a reference using exact CAS. It never retires or
// deletes the organization publication.
type RemoveRequest struct {
	IdempotencyKey      string
	AgentID             string
	ExpectedGeneration  uint64
	ExpectedContentHash string
}

// Store is the HA-68 service contract. Metadata methods never return bodies;
// Resolve is the only body-bearing method and requires the exact caller
// identity plus target agent.
type Store interface {
	Publish(ctx context.Context, caller identity.Quadruple, req PublishRequest) (Metadata, Receipt, error)
	List(ctx context.Context, caller identity.Quadruple) ([]Metadata, error)
	Get(ctx context.Context, caller identity.Quadruple, publicationID string) (Metadata, error)
	PublishSuccessor(ctx context.Context, caller identity.Quadruple, req SuccessorRequest) (Metadata, Receipt, error)
	Retire(ctx context.Context, caller identity.Quadruple, req RetireRequest) (Metadata, Receipt, error)
	ListAvailable(ctx context.Context, caller identity.Quadruple) ([]Metadata, error)
	Install(ctx context.Context, caller identity.Quadruple, req InstallRequest) (Reference, Receipt, error)
	Update(ctx context.Context, caller identity.Quadruple, req UpdateRequest) (Reference, Receipt, error)
	Remove(ctx context.Context, caller identity.Quadruple, req RemoveRequest) (Receipt, error)
	ListReferences(ctx context.Context, caller identity.Quadruple) ([]Reference, error)
	Resolve(ctx context.Context, caller identity.Quadruple, agentID string) (skills.Skill, Metadata, error)
	Close(ctx context.Context) error
}

// Sentinel errors. Callers should use errors.Is and never parse messages.
var (
	ErrIdentityRequired    = errors.New("skills/publication: identity required")
	ErrInvalidRequest      = errors.New("skills/publication: invalid request")
	ErrNotFound            = errors.New("skills/publication: publication not found")
	ErrReferenceNotFound   = errors.New("skills/publication: reference not found")
	ErrConflict            = errors.New("skills/publication: compare-and-swap conflict")
	ErrIdempotencyConflict = errors.New("skills/publication: idempotency conflict")
	ErrRetired             = errors.New("skills/publication: publication retired")
	ErrRuntimeMismatch     = errors.New("skills/publication: runtime mismatch")
	ErrContentHashMismatch = errors.New("skills/publication: content hash mismatch")
	ErrStoreClosed         = errors.New("skills/publication: store closed")
)

// RuntimeIDFromContext returns the runtime/deployment binding. Production
// assembly sets it at construction; tests may use WithRuntimeID.
type runtimeIDKey struct{}

// WithRuntimeID binds a request to one Harbor runtime/deployment.
func WithRuntimeID(ctx context.Context, runtimeID string) context.Context {
	return context.WithValue(ctx, runtimeIDKey{}, strings.TrimSpace(runtimeID))
}

func runtimeID(ctx context.Context, fallback string) string {
	if value, ok := ctx.Value(runtimeIDKey{}).(string); ok && value != "" {
		return value
	}
	return fallback
}

// RuntimeID returns the request's explicit runtime binding when one exists.
func RuntimeID(ctx context.Context) string {
	return runtimeID(ctx, "")
}

// NewRuntimeID produces a deterministic opaque runtime identifier from a
// deployment name. It is intentionally not a network/export identifier.
func NewRuntimeID(deployment string) string {
	h := sha256.Sum256([]byte("harbor-runtime-v1\x00" + strings.TrimSpace(deployment)))
	return "hrt_" + hex.EncodeToString(h[:16])
}

func validateCaller(caller identity.Quadruple) error {
	if caller.TenantID == "" || caller.UserID == "" || caller.SessionID == "" {
		return ErrIdentityRequired
	}
	return nil
}

func validateKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len([]rune(key)) > 128 {
		return fmt.Errorf("%w: idempotency key", ErrInvalidRequest)
	}
	return nil
}

func canonicalName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func validateSkill(skill skills.Skill, expectedName string) (skills.Skill, error) {
	if expectedName != "" && canonicalName(skill.Name) != canonicalName(expectedName) {
		return skills.Skill{}, fmt.Errorf("%w: skill name must match publication name", ErrInvalidRequest)
	}
	for key, value := range skill.Extra {
		if _, ok := value.(string); !ok {
			return skills.Skill{}, fmt.Errorf("%w: skill extra[%q] must be a string", ErrInvalidRequest, key)
		}
	}
	item := skills.PackItemFromSkill(skill)
	item.Name = canonicalName(item.Name)
	item.OriginRef = ""
	if err := item.Validate(); err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill: %w", ErrInvalidRequest, err)
	}
	canonical, err := item.Skill()
	if err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill: %w", ErrInvalidRequest, err)
	}
	canonical.AgentID = ""
	canonical.OriginRef = ""
	return canonical, nil
}

func metadata(p Publication) Metadata {
	return Metadata{PublicationID: p.PublicationID, RevisionID: p.RevisionID, Name: p.Name, ContentHash: p.ContentHash, State: p.State, Generation: p.Generation, RuntimeID: p.RuntimeID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func cloneSkill(s skills.Skill) skills.Skill {
	s.Tags = append([]string(nil), s.Tags...)
	s.Steps = append([]string(nil), s.Steps...)
	s.Preconditions = append([]string(nil), s.Preconditions...)
	s.FailureModes = append([]string(nil), s.FailureModes...)
	s.RequiredTools = append([]string(nil), s.RequiredTools...)
	s.RequiredNS = append([]string(nil), s.RequiredNS...)
	s.RequiredTags = append([]string(nil), s.RequiredTags...)
	if s.Extra != nil {
		s.Extra = cloneExtra(s.Extra)
	}
	return s
}

func cloneExtra(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		switch nested := value.(type) {
		case map[string]any:
			out[key] = cloneExtra(nested)
		case map[string]string:
			copyMap := make(map[string]string, len(nested))
			for nestedKey, nestedValue := range nested {
				copyMap[nestedKey] = nestedValue
			}
			out[key] = copyMap
		case []any:
			copySlice := make([]any, len(nested))
			for i, item := range nested {
				if itemMap, ok := item.(map[string]any); ok {
					copySlice[i] = cloneExtra(itemMap)
				} else {
					copySlice[i] = item
				}
			}
			out[key] = copySlice
		case []string:
			out[key] = append([]string(nil), nested...)
		default:
			out[key] = value
		}
	}
	return out
}

func stampOriginRef(skill skills.Skill, publicationID, revisionID string) skills.Skill {
	skill.OriginRef = publicationOriginPrefix + publicationID + "." + revisionID
	return skill
}

func clonePublication(p Publication) Publication {
	p.Skill = cloneSkill(p.Skill)
	if len(p.Revisions) > 0 {
		p.Revisions = append([]Revision(nil), p.Revisions...)
		for i := range p.Revisions {
			p.Revisions[i].Skill = cloneSkill(p.Revisions[i].Skill)
		}
	}
	return p
}

func currentRevision(p Publication) Revision {
	return Revision{RevisionID: p.RevisionID, ContentHash: p.ContentHash, Generation: p.Generation, RuntimeID: p.RuntimeID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Skill: cloneSkill(p.Skill)}
}

func revisionFor(p Publication, revisionID string) (Revision, error) {
	if p.RevisionID == revisionID {
		return currentRevision(p), nil
	}
	for _, rev := range p.Revisions {
		if rev.RevisionID == revisionID {
			rev.Skill = cloneSkill(rev.Skill)
			return rev, nil
		}
	}
	return Revision{}, ErrNotFound
}

func revisionMetadata(p Publication, rev Revision) Metadata {
	return Metadata{PublicationID: p.PublicationID, RevisionID: rev.RevisionID, Name: p.Name, ContentHash: rev.ContentHash, State: p.State, Generation: rev.Generation, RuntimeID: rev.RuntimeID, CreatedAt: rev.CreatedAt, UpdatedAt: rev.UpdatedAt}
}

func cloneReference(r Reference) Reference { return r }

func newOpaqueID(prefix string, key string, generation uint64) string {
	h := sha256.Sum256([]byte(prefix + "\x00" + key + "\x00" + fmt.Sprint(generation) + "\x00" + fmt.Sprint(time.Now().UnixNano())))
	return prefix + "_" + hex.EncodeToString(h[:16])
}

// MemoryStore is a concurrency-safe reference implementation. It is useful
// for unit tests and is intentionally behaviorally equivalent to StateStore.
type MemoryStore struct {
	mu      sync.RWMutex
	runtime string
	closed  bool
	clock   func() time.Time
	pubs    map[string][]Publication
	refs    map[string]Reference
	ops     map[string]operation
}

type operation struct {
	Fingerprint     string     `json:"fingerprint"`
	Receipt         Receipt    `json:"receipt"`
	ResultMetadata  Metadata   `json:"result_metadata,omitempty"`
	ResultReference *Reference `json:"result_reference,omitempty"`
}

func replayOperationMetadata(op operation, fallback Metadata) Metadata {
	if op.ResultMetadata.PublicationID != "" {
		return op.ResultMetadata
	}
	return fallback
}

// NewMemoryStore constructs an HA-68 store bound to one runtime/deployment.
func NewMemoryStore(runtimeID string) *MemoryStore {
	return &MemoryStore{runtime: strings.TrimSpace(runtimeID), clock: time.Now, pubs: map[string][]Publication{}, refs: map[string]Reference{}, ops: map[string]operation{}}
}

func (m *MemoryStore) begin(ctx context.Context, caller identity.Quadruple, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCaller(caller); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrStoreClosed
	}
	return nil
}

func (m *MemoryStore) finish() { m.mu.Unlock() }

func (m *MemoryStore) checkOp(key, fingerprint string) (Receipt, bool, error) {
	if prior, ok := m.ops[key]; ok {
		if prior.Fingerprint != fingerprint {
			return Receipt{}, true, ErrIdempotencyConflict
		}
		return prior.Receipt, true, nil
	}
	return Receipt{}, false, nil
}

func (m *MemoryStore) recordOp(key, fingerprint string, receipt Receipt) {
	m.ops[key] = operation{Fingerprint: fingerprint, Receipt: receipt}
}

func (m *MemoryStore) recordPublicationOp(key, fingerprint string, receipt Receipt, result Metadata) {
	m.ops[key] = operation{Fingerprint: fingerprint, Receipt: receipt, ResultMetadata: result}
}

func (m *MemoryStore) recordReferenceOp(key, fingerprint string, receipt Receipt, result Reference) {
	resultCopy := cloneReference(result)
	m.ops[key] = operation{Fingerprint: fingerprint, Receipt: receipt, ResultReference: &resultCopy}
}

func (m *MemoryStore) replayMetadata(key string, fallback Metadata) Metadata {
	if result := m.ops[key].ResultMetadata; result.PublicationID != "" {
		return result
	}
	return fallback
}

func (m *MemoryStore) replayReference(key string, fallback Reference) Reference {
	if result := m.ops[key].ResultReference; result != nil {
		return cloneReference(*result)
	}
	return fallback
}

func refKey(caller identity.Quadruple, agentID string) string {
	return caller.TenantID + "\x00" + caller.UserID + "\x00" + agentID
}
func opKey(caller identity.Quadruple, key string) string {
	return caller.TenantID + "\x00" + caller.UserID + "\x00" + caller.SessionID + "\x00" + key
}

func (m *MemoryStore) publication(caller identity.Quadruple, id string) (Publication, int, error) {
	for i, p := range m.pubs[caller.TenantID] {
		if p.PublicationID == id {
			return clonePublication(p), i, nil
		}
	}
	return Publication{}, -1, ErrNotFound
}

func (m *MemoryStore) Publish(ctx context.Context, caller identity.Quadruple, req PublishRequest) (Metadata, Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	defer m.finish()
	if !req.ExpectedAbsent || canonicalName(req.Name) == "" {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: create requires expected absence and name", ErrInvalidRequest)
	}
	skill, err := validateSkill(req.Skill, req.Name)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	fingerprint := "publish\x00" + canonicalName(req.Name) + "\x00" + skill.ContentHash
	opKey := opKey(caller, req.IdempotencyKey)
	if prior, ok, err := m.checkOp(opKey, fingerprint); err != nil || ok {
		if err != nil {
			return Metadata{}, prior, err
		}
		for _, p := range m.pubs[caller.TenantID] {
			if p.PublicationID == prior.PublicationID {
				return m.replayMetadata(opKey, metadata(p)), prior, nil
			}
		}
		return m.replayMetadata(opKey, Metadata{}), prior, nil
	}
	for _, p := range m.pubs[caller.TenantID] {
		if p.Name == canonicalName(req.Name) {
			return Metadata{}, Receipt{}, ErrConflict
		}
	}
	if len(m.pubs[caller.TenantID]) >= maxPublications {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: publication limit reached", ErrConflict)
	}
	now := m.clock().UTC()
	publicationID := newOpaqueID("pub", caller.TenantID+canonicalName(req.Name), uint64(len(m.pubs[caller.TenantID])+1))
	revisionID := newOpaqueID("rev", skill.ContentHash, 1)
	skill = stampOriginRef(skill, publicationID, revisionID)
	p := Publication{PublicationID: publicationID, RevisionID: revisionID, Name: canonicalName(req.Name), ContentHash: skill.ContentHash, State: StateActive, Generation: 1, RuntimeID: m.runtime, CreatedAt: now, UpdatedAt: now, Skill: cloneSkill(skill)}
	p.Revisions = []Revision{currentRevision(p)}
	m.pubs[caller.TenantID] = append(m.pubs[caller.TenantID], p)
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "publish", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	m.recordPublicationOp(opKey, fingerprint, receipt, result)
	return result, receipt, nil
}

func (m *MemoryStore) List(ctx context.Context, caller identity.Quadruple) ([]Metadata, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrStoreClosed
	}
	out := make([]Metadata, 0, len(m.pubs[caller.TenantID]))
	for _, p := range m.pubs[caller.TenantID] {
		out = append(out, metadata(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) Get(ctx context.Context, caller identity.Quadruple, publicationID string) (Metadata, error) {
	if err := validateCaller(caller); err != nil {
		return Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(publicationID) == "" {
		return Metadata{}, ErrInvalidRequest
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Metadata{}, ErrStoreClosed
	}
	p, _, err := m.publication(caller, publicationID)
	if err != nil {
		return Metadata{}, err
	}
	return metadata(p), nil
}

func (m *MemoryStore) PublishSuccessor(ctx context.Context, caller identity.Quadruple, req SuccessorRequest) (Metadata, Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	defer m.finish()
	if req.ExpectedGeneration == 0 || req.ExpectedContentHash == "" {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: successor requires generation and hash", ErrInvalidRequest)
	}
	skill, err := validateSkill(req.Skill, "")
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	fingerprint := fmt.Sprintf("successor\x00%s\x00%d\x00%s\x00%s", req.PublicationID, req.ExpectedGeneration, req.ExpectedContentHash, skill.ContentHash)
	opKey := opKey(caller, req.IdempotencyKey)
	if prior, ok, err := m.checkOp(opKey, fingerprint); err != nil || ok {
		if err != nil {
			return Metadata{}, prior, err
		}
		return m.replayMetadata(opKey, Metadata{}), prior, nil
	}
	p, idx, err := m.publication(caller, req.PublicationID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if canonicalName(skill.Name) != p.Name {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: successor skill name must match publication name", ErrInvalidRequest)
	}
	if p.State == StateRetired {
		return Metadata{}, Receipt{}, ErrRetired
	}
	if p.Generation != req.ExpectedGeneration || p.ContentHash != req.ExpectedContentHash {
		return Metadata{}, Receipt{}, ErrConflict
	}
	if len(p.Revisions) >= maxRevisions {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: revision limit reached", ErrConflict)
	}
	now := m.clock().UTC()
	revisionID := newOpaqueID("rev", skill.ContentHash, p.Generation+1)
	skill = stampOriginRef(skill, p.PublicationID, revisionID)
	p.Revisions = append(p.Revisions, currentRevision(p))
	p.RevisionID = revisionID
	p.ContentHash = skill.ContentHash
	p.Generation++
	p.UpdatedAt = now
	p.Skill = cloneSkill(skill)
	m.pubs[caller.TenantID][idx] = p
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "publish_successor", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, BeforeHash: req.ExpectedContentHash, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	m.recordPublicationOp(opKey, fingerprint, receipt, result)
	return result, receipt, nil
}

func (m *MemoryStore) Retire(ctx context.Context, caller identity.Quadruple, req RetireRequest) (Metadata, Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	defer m.finish()
	if req.ExpectedGeneration == 0 || req.ExpectedContentHash == "" {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: retire requires generation and hash", ErrInvalidRequest)
	}
	fingerprint := fmt.Sprintf("retire\x00%s\x00%d\x00%s", req.PublicationID, req.ExpectedGeneration, req.ExpectedContentHash)
	opKey := opKey(caller, req.IdempotencyKey)
	if prior, ok, err := m.checkOp(opKey, fingerprint); err != nil || ok {
		if err != nil {
			return Metadata{}, prior, err
		}
		return m.replayMetadata(opKey, Metadata{}), prior, nil
	}
	p, idx, err := m.publication(caller, req.PublicationID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if p.Generation != req.ExpectedGeneration || p.ContentHash != req.ExpectedContentHash {
		return Metadata{}, Receipt{}, ErrConflict
	}
	if p.State == StateRetired {
		return Metadata{}, Receipt{}, ErrRetired
	}
	now := m.clock().UTC()
	p.State = StateRetired
	p.Generation++
	p.UpdatedAt = now
	m.pubs[caller.TenantID][idx] = p
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "retire", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, BeforeHash: req.ExpectedContentHash, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	m.recordPublicationOp(opKey, fingerprint, receipt, result)
	return result, receipt, nil
}

func (m *MemoryStore) ListAvailable(ctx context.Context, caller identity.Quadruple) ([]Metadata, error) {
	all, err := m.List(ctx, caller)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if p.State == StateActive {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MemoryStore) findRevision(caller identity.Quadruple, publicationID, revisionID string) (Publication, Revision, error) {
	p, _, err := m.publication(caller, publicationID)
	if err != nil {
		return Publication{}, Revision{}, err
	}
	rev, err := revisionFor(p, revisionID)
	if err != nil {
		return Publication{}, Revision{}, err
	}
	return p, rev, nil
}

func (m *MemoryStore) Install(ctx context.Context, caller identity.Quadruple, req InstallRequest) (Reference, Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Reference{}, Receipt{}, err
	}
	defer m.finish()
	if !req.ExpectedAbsent || strings.TrimSpace(req.AgentID) == "" {
		return Reference{}, Receipt{}, fmt.Errorf("%w: install requires expected absence and agent", ErrInvalidRequest)
	}
	fp := fmt.Sprintf("install\x00%s\x00%s\x00%s", req.AgentID, req.PublicationID, req.RevisionID)
	opKey := opKey(caller, req.IdempotencyKey)
	if prior, ok, err := m.checkOp(opKey, fp); err != nil || ok {
		if err != nil {
			return Reference{}, prior, err
		}
		return m.replayReference(opKey, m.refs[refKey(caller, req.AgentID)]), prior, nil
	}
	p, rev, err := m.findRevision(caller, req.PublicationID, req.RevisionID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	if p.State != StateActive {
		return Reference{}, Receipt{}, ErrRetired
	}
	key := refKey(caller, req.AgentID)
	if _, ok := m.refs[key]; ok {
		return Reference{}, Receipt{}, ErrConflict
	}
	now := m.clock().UTC()
	r := Reference{AgentID: req.AgentID, PublicationID: p.PublicationID, RevisionID: rev.RevisionID, ContentHash: rev.ContentHash, Generation: rev.Generation, RuntimeID: rev.RuntimeID, State: p.State, UpdatedAt: now}
	m.refs[key] = r
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "install", PublicationID: r.PublicationID, RevisionID: r.RevisionID, Generation: r.Generation, State: r.State, AfterHash: r.ContentHash, UpdatedAt: now}
	m.recordReferenceOp(opKey, fp, receipt, r)
	return cloneReference(r), receipt, nil
}

func (m *MemoryStore) Update(ctx context.Context, caller identity.Quadruple, req UpdateRequest) (Reference, Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Reference{}, Receipt{}, err
	}
	defer m.finish()
	if req.ExpectedGeneration == 0 || req.ExpectedContentHash == "" || strings.TrimSpace(req.AgentID) == "" {
		return Reference{}, Receipt{}, fmt.Errorf("%w: update requires generation/hash/agent", ErrInvalidRequest)
	}
	fp := fmt.Sprintf("update\x00%s\x00%s\x00%s\x00%d\x00%s", req.AgentID, req.PublicationID, req.RevisionID, req.ExpectedGeneration, req.ExpectedContentHash)
	opKey := opKey(caller, req.IdempotencyKey)
	if prior, ok, err := m.checkOp(opKey, fp); err != nil || ok {
		if err != nil {
			return Reference{}, prior, err
		}
		return m.replayReference(opKey, m.refs[refKey(caller, req.AgentID)]), prior, nil
	}
	p, rev, err := m.findRevision(caller, req.PublicationID, req.RevisionID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	if p.State != StateActive {
		return Reference{}, Receipt{}, ErrRetired
	}
	key := refKey(caller, req.AgentID)
	r, ok := m.refs[key]
	if !ok {
		return Reference{}, Receipt{}, ErrReferenceNotFound
	}
	if r.Generation != req.ExpectedGeneration || r.ContentHash != req.ExpectedContentHash {
		return Reference{}, Receipt{}, ErrConflict
	}
	now := m.clock().UTC()
	r.PublicationID = p.PublicationID
	r.RevisionID = rev.RevisionID
	r.ContentHash = rev.ContentHash
	r.Generation = rev.Generation
	r.RuntimeID = rev.RuntimeID
	r.State = p.State
	r.UpdatedAt = now
	m.refs[key] = r
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "update", PublicationID: r.PublicationID, RevisionID: r.RevisionID, Generation: r.Generation, State: r.State, BeforeHash: req.ExpectedContentHash, AfterHash: r.ContentHash, UpdatedAt: now}
	m.recordReferenceOp(opKey, fp, receipt, r)
	return cloneReference(r), receipt, nil
}

func (m *MemoryStore) Remove(ctx context.Context, caller identity.Quadruple, req RemoveRequest) (Receipt, error) {
	if err := m.begin(ctx, caller, req.IdempotencyKey); err != nil {
		return Receipt{}, err
	}
	defer m.finish()
	if req.ExpectedGeneration == 0 || req.ExpectedContentHash == "" || strings.TrimSpace(req.AgentID) == "" {
		return Receipt{}, fmt.Errorf("%w: remove requires generation/hash/agent", ErrInvalidRequest)
	}
	fp := fmt.Sprintf("remove\x00%s\x00%d\x00%s", req.AgentID, req.ExpectedGeneration, req.ExpectedContentHash)
	if prior, ok, err := m.checkOp(opKey(caller, req.IdempotencyKey), fp); err != nil || ok {
		return prior, err
	}
	key := refKey(caller, req.AgentID)
	r, ok := m.refs[key]
	if !ok {
		return Receipt{}, ErrReferenceNotFound
	}
	if r.Generation != req.ExpectedGeneration || r.ContentHash != req.ExpectedContentHash {
		return Receipt{}, ErrConflict
	}
	now := m.clock().UTC()
	delete(m.refs, key)
	receipt := Receipt{OperationID: req.IdempotencyKey, Operation: "remove", PublicationID: r.PublicationID, RevisionID: r.RevisionID, Generation: r.Generation, State: StateRetired, BeforeHash: r.ContentHash, UpdatedAt: now}
	m.recordOp(opKey(caller, req.IdempotencyKey), fp, receipt)
	return receipt, nil
}

func (m *MemoryStore) ListReferences(ctx context.Context, caller identity.Quadruple) ([]Reference, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrStoreClosed
	}
	prefix := caller.TenantID + "\x00" + caller.UserID + "\x00"
	out := []Reference{}
	for k, r := range m.refs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, cloneReference(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

func (m *MemoryStore) Resolve(ctx context.Context, caller identity.Quadruple, agentID string) (skills.Skill, Metadata, error) {
	if err := validateCaller(caller); err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return skills.Skill{}, Metadata{}, ErrStoreClosed
	}
	r, ok := m.refs[refKey(caller, agentID)]
	if !ok {
		return skills.Skill{}, Metadata{}, ErrReferenceNotFound
	}
	if r.RuntimeID != runtimeID(ctx, m.runtime) {
		return skills.Skill{}, Metadata{}, ErrRuntimeMismatch
	}
	p, rev, err := m.findRevision(caller, r.PublicationID, r.RevisionID)
	if err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	if p.State != StateActive || r.State != StateActive {
		return skills.Skill{}, Metadata{}, ErrRetired
	}
	if rev.ContentHash != r.ContentHash || rev.Generation != r.Generation {
		return skills.Skill{}, Metadata{}, ErrContentHashMismatch
	}
	return cloneSkill(rev.Skill), revisionMetadata(p, rev), nil
}

func (m *MemoryStore) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// StateStoreStore persists the publication aggregate and user references in
// Harbor's StateStore. It is a durable implementation for all three shipped
// StateStore drivers. The StateStore ID is the exact CAS generation, while
// the domain generation/hash remain explicit in every request.
type StateStoreStore struct {
	state   state.StateStore
	runtime string
	clock   func() time.Time
	mu      sync.Mutex
	closed  bool
}

// NewStateStore constructs a durable HA-68 store over an existing StateStore.
func NewStateStore(st state.StateStore, runtimeID string) (*StateStoreStore, error) {
	if st == nil {
		return nil, ErrStoreClosed
	}
	return &StateStoreStore{state: st, runtime: strings.TrimSpace(runtimeID), clock: time.Now}, nil
}

type durableAggregate struct {
	Publications []Publication        `json:"publications"`
	Operations   map[string]operation `json:"operations,omitempty"`
}
type durableReference struct {
	Reference  Reference            `json:"reference"`
	Operations map[string]operation `json:"operations,omitempty"`
}

func orgIdentity(tenant string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: publicationOrgUser, SessionID: publicationOrgSession}}
}

func referenceIdentity(c identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: c.TenantID, UserID: c.UserID, SessionID: publicationRefSession}}
}

func refKind(agent string) string { return publicationRefPrefix + agent }
func durableOpKey(c identity.Quadruple, key string) string {
	return c.UserID + "\x00" + c.SessionID + "\x00" + key
}
func decodeAggregate(r state.StateRecord) (durableAggregate, error) {
	var a durableAggregate
	if len(r.Bytes) == 0 {
		return a, nil
	}
	if err := json.Unmarshal(r.Bytes, &a); err != nil {
		return a, fmt.Errorf("%w: aggregate: %v", ErrInvalidRequest, err)
	}
	if a.Operations == nil {
		a.Operations = map[string]operation{}
	}
	return a, nil
}
func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrInvalidRequest, err)
	}
	return b, nil
}
func (d *StateStoreStore) loadAggregate(ctx context.Context, tenant string) (durableAggregate, state.StateRecord, bool, error) {
	r, err := d.state.Load(ctx, orgIdentity(tenant), publicationOrgKind)
	if errors.Is(err, state.ErrNotFound) {
		return durableAggregate{Operations: map[string]operation{}}, state.StateRecord{}, false, nil
	}
	if err != nil {
		return durableAggregate{}, state.StateRecord{}, false, err
	}
	a, err := decodeAggregate(r)
	return a, r, true, err
}
func (d *StateStoreStore) saveAggregate(ctx context.Context, tenant string, old state.StateRecord, present bool, a durableAggregate) error {
	b, err := encode(a)
	if err != nil {
		return err
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: orgIdentity(tenant), Kind: publicationOrgKind, Bytes: b}
	if present {
		return d.state.SaveIf(ctx, []state.SlotExpectation{{Identity: next.Identity, Kind: next.Kind, ExpectedEventID: old.ID}}, next)
	}
	return d.state.SaveIf(ctx, []state.SlotExpectation{{Identity: next.Identity, Kind: next.Kind}}, next)
}

// StateStore-backed methods use the same behavior as MemoryStore. The helper
// methods below intentionally keep bodies outside all metadata projections.
func (d *StateStoreStore) Publish(ctx context.Context, c identity.Quadruple, r PublishRequest) (Metadata, Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Metadata{}, Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	skill, err := validateSkill(r.Skill, r.Name)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if !r.ExpectedAbsent {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: expected absence", ErrInvalidRequest)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Metadata{}, Receipt{}, err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	fp := "publish\x00" + canonicalName(r.Name) + "\x00" + skill.ContentHash
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := a.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Metadata{}, Receipt{}, ErrIdempotencyConflict
		}
		for _, p := range a.Publications {
			if p.PublicationID == prior.Receipt.PublicationID {
				return replayOperationMetadata(prior, metadata(p)), prior.Receipt, nil
			}
		}
		return replayOperationMetadata(prior, Metadata{}), prior.Receipt, nil
	}
	for _, p := range a.Publications {
		if p.Name == canonicalName(r.Name) {
			return Metadata{}, Receipt{}, ErrConflict
		}
	}
	if len(a.Publications) >= maxPublications {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: publication limit reached", ErrConflict)
	}
	now := d.clock().UTC()
	publicationID := newOpaqueID("pub", c.TenantID+r.Name, uint64(len(a.Publications)+1))
	revisionID := newOpaqueID("rev", skill.ContentHash, 1)
	skill = stampOriginRef(skill, publicationID, revisionID)
	p := Publication{PublicationID: publicationID, RevisionID: revisionID, Name: canonicalName(r.Name), ContentHash: skill.ContentHash, State: StateActive, Generation: 1, RuntimeID: d.runtime, CreatedAt: now, UpdatedAt: now, Skill: cloneSkill(skill)}
	p.Revisions = []Revision{currentRevision(p)}
	a.Publications = append(a.Publications, p)
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "publish", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	a.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt, ResultMetadata: result}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Metadata{}, Receipt{}, ErrConflict
		}
		return Metadata{}, Receipt{}, err
	}
	return result, receipt, nil
}
func (d *StateStoreStore) loadPubs(ctx context.Context, c identity.Quadruple) (durableAggregate, state.StateRecord, bool, error) {
	return d.loadAggregate(ctx, c.TenantID)
}
func (d *StateStoreStore) List(ctx context.Context, c identity.Quadruple) ([]Metadata, error) {
	if err := validateCaller(c); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return nil, err
	}
	a, _, _, err := d.loadPubs(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, 0, len(a.Publications))
	for _, p := range a.Publications {
		out = append(out, metadata(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (d *StateStoreStore) Get(ctx context.Context, c identity.Quadruple, id string) (Metadata, error) {
	if err := validateCaller(c); err != nil {
		return Metadata{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Metadata{}, err
	}
	a, _, _, err := d.loadPubs(ctx, c)
	if err != nil {
		return Metadata{}, err
	}
	for _, p := range a.Publications {
		if p.PublicationID == id {
			return metadata(p), nil
		}
	}
	return Metadata{}, ErrNotFound
}

func (d *StateStoreStore) checkOpen(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return ErrStoreClosed
	}
	return nil
}

func (d *StateStoreStore) findPublication(a durableAggregate, publicationID string) (Publication, int, error) {
	for i, p := range a.Publications {
		if p.PublicationID == publicationID {
			return clonePublication(p), i, nil
		}
	}
	return Publication{}, -1, ErrNotFound
}

func (d *StateStoreStore) findPublicationRevision(a durableAggregate, publicationID, revisionID string) (Publication, Revision, error) {
	p, _, err := d.findPublication(a, publicationID)
	if err != nil {
		return Publication{}, Revision{}, err
	}
	rev, err := revisionFor(p, revisionID)
	if err != nil {
		return Publication{}, Revision{}, err
	}
	return p, rev, nil
}

func (d *StateStoreStore) loadReference(ctx context.Context, caller identity.Quadruple, agentID string) (durableReference, state.StateRecord, bool, error) {
	refID := referenceIdentity(caller)
	r, err := d.state.Load(ctx, refID, refKind(agentID))
	if errors.Is(err, state.ErrNotFound) {
		return durableReference{Operations: map[string]operation{}}, state.StateRecord{}, false, nil
	}
	if err != nil {
		return durableReference{}, state.StateRecord{}, false, err
	}
	var ref durableReference
	if err := json.Unmarshal(r.Bytes, &ref); err != nil {
		return durableReference{}, state.StateRecord{}, false, fmt.Errorf("%w: reference: %v", ErrInvalidRequest, err)
	}
	if ref.Operations == nil {
		ref.Operations = map[string]operation{}
	}
	return ref, r, true, nil
}

func (d *StateStoreStore) saveReference(ctx context.Context, caller identity.Quadruple, agentID string, old state.StateRecord, present bool, ref durableReference) error {
	b, err := encode(ref)
	if err != nil {
		return err
	}
	refID := referenceIdentity(caller)
	next := state.StateRecord{ID: state.NewEventID(), Identity: refID, Kind: refKind(agentID), Bytes: b}
	if present {
		return d.state.SaveIf(ctx, []state.SlotExpectation{{Identity: refID, Kind: next.Kind, ExpectedEventID: old.ID}}, next)
	}
	return d.state.SaveIf(ctx, []state.SlotExpectation{{Identity: refID, Kind: next.Kind}}, next)
}

func (d *StateStoreStore) PublishSuccessor(ctx context.Context, c identity.Quadruple, r SuccessorRequest) (Metadata, Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Metadata{}, Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	if r.ExpectedGeneration == 0 || r.ExpectedContentHash == "" {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: successor requires generation and hash", ErrInvalidRequest)
	}
	skill, err := validateSkill(r.Skill, "")
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Metadata{}, Receipt{}, err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if !present {
		return Metadata{}, Receipt{}, ErrNotFound
	}
	fp := fmt.Sprintf("successor\x00%s\x00%d\x00%s\x00%s", r.PublicationID, r.ExpectedGeneration, r.ExpectedContentHash, skill.ContentHash)
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := a.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Metadata{}, Receipt{}, ErrIdempotencyConflict
		}
		p, _, ferr := d.findPublication(a, r.PublicationID)
		if ferr != nil {
			return Metadata{}, Receipt{}, ferr
		}
		return replayOperationMetadata(prior, metadata(p)), prior.Receipt, nil
	}
	p, idx, err := d.findPublication(a, r.PublicationID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if canonicalName(skill.Name) != p.Name {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: successor skill name must match publication name", ErrInvalidRequest)
	}
	if p.State == StateRetired {
		return Metadata{}, Receipt{}, ErrRetired
	}
	if p.Generation != r.ExpectedGeneration || p.ContentHash != r.ExpectedContentHash {
		return Metadata{}, Receipt{}, ErrConflict
	}
	if len(p.Revisions) >= maxRevisions {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: revision limit reached", ErrConflict)
	}
	now := d.clock().UTC()
	revisionID := newOpaqueID("rev", skill.ContentHash, p.Generation+1)
	skill = stampOriginRef(skill, p.PublicationID, revisionID)
	p.Revisions = append(p.Revisions, currentRevision(p))
	p.RevisionID = revisionID
	p.ContentHash = skill.ContentHash
	p.Generation++
	p.UpdatedAt = now
	p.Skill = cloneSkill(skill)
	a.Publications[idx] = p
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "publish_successor", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, BeforeHash: r.ExpectedContentHash, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	a.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt, ResultMetadata: result}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Metadata{}, Receipt{}, ErrConflict
		}
		return Metadata{}, Receipt{}, err
	}
	return result, receipt, nil
}

func (d *StateStoreStore) Retire(ctx context.Context, c identity.Quadruple, r RetireRequest) (Metadata, Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Metadata{}, Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Metadata{}, Receipt{}, err
	}
	if r.ExpectedGeneration == 0 || r.ExpectedContentHash == "" {
		return Metadata{}, Receipt{}, fmt.Errorf("%w: retire requires generation and hash", ErrInvalidRequest)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Metadata{}, Receipt{}, err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if !present {
		return Metadata{}, Receipt{}, ErrNotFound
	}
	fp := fmt.Sprintf("retire\x00%s\x00%d\x00%s", r.PublicationID, r.ExpectedGeneration, r.ExpectedContentHash)
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := a.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Metadata{}, Receipt{}, ErrIdempotencyConflict
		}
		p, _, ferr := d.findPublication(a, r.PublicationID)
		if ferr != nil {
			return Metadata{}, Receipt{}, ferr
		}
		return replayOperationMetadata(prior, metadata(p)), prior.Receipt, nil
	}
	p, idx, err := d.findPublication(a, r.PublicationID)
	if err != nil {
		return Metadata{}, Receipt{}, err
	}
	if p.Generation != r.ExpectedGeneration || p.ContentHash != r.ExpectedContentHash {
		return Metadata{}, Receipt{}, ErrConflict
	}
	if p.State == StateRetired {
		return Metadata{}, Receipt{}, ErrRetired
	}
	now := d.clock().UTC()
	p.State = StateRetired
	p.Generation++
	p.UpdatedAt = now
	a.Publications[idx] = p
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "retire", PublicationID: p.PublicationID, RevisionID: p.RevisionID, Generation: p.Generation, State: p.State, BeforeHash: r.ExpectedContentHash, AfterHash: p.ContentHash, UpdatedAt: now}
	result := metadata(p)
	a.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt, ResultMetadata: result}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Metadata{}, Receipt{}, ErrConflict
		}
		return Metadata{}, Receipt{}, err
	}
	return result, receipt, nil
}
func (d *StateStoreStore) ListAvailable(ctx context.Context, c identity.Quadruple) ([]Metadata, error) {
	all, err := d.List(ctx, c)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if p.State == StateActive {
			out = append(out, p)
		}
	}
	return out, nil
}
func (d *StateStoreStore) Install(ctx context.Context, c identity.Quadruple, r InstallRequest) (Reference, Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Reference{}, Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Reference{}, Receipt{}, err
	}
	if !r.ExpectedAbsent || strings.TrimSpace(r.AgentID) == "" {
		return Reference{}, Receipt{}, fmt.Errorf("%w: install requires expected absence and agent", ErrInvalidRequest)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Reference{}, Receipt{}, err
	}
	ref, old, present, err := d.loadReference(ctx, c, r.AgentID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	fp := fmt.Sprintf("install\x00%s\x00%s\x00%s", r.AgentID, r.PublicationID, r.RevisionID)
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := ref.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Reference{}, Receipt{}, ErrIdempotencyConflict
		}
		if prior.ResultReference != nil {
			return cloneReference(*prior.ResultReference), prior.Receipt, nil
		}
		return cloneReference(ref.Reference), prior.Receipt, nil
	}
	a, _, _, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	p, rev, err := d.findPublicationRevision(a, r.PublicationID, r.RevisionID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	if p.State != StateActive {
		return Reference{}, Receipt{}, ErrRetired
	}
	if present && ref.Reference.AgentID != "" {
		return Reference{}, Receipt{}, ErrConflict
	}
	now := d.clock().UTC()
	ref.Reference = Reference{AgentID: r.AgentID, PublicationID: p.PublicationID, RevisionID: rev.RevisionID, ContentHash: rev.ContentHash, Generation: rev.Generation, RuntimeID: rev.RuntimeID, State: p.State, UpdatedAt: now}
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "install", PublicationID: p.PublicationID, RevisionID: rev.RevisionID, Generation: rev.Generation, State: p.State, AfterHash: rev.ContentHash, UpdatedAt: now}
	result := cloneReference(ref.Reference)
	ref.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt, ResultReference: &result}
	if err := d.saveReference(ctx, c, r.AgentID, old, present, ref); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Reference{}, Receipt{}, ErrConflict
		}
		return Reference{}, Receipt{}, err
	}
	return result, receipt, nil
}
func (d *StateStoreStore) Update(ctx context.Context, c identity.Quadruple, r UpdateRequest) (Reference, Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Reference{}, Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Reference{}, Receipt{}, err
	}
	if r.ExpectedGeneration == 0 || r.ExpectedContentHash == "" || strings.TrimSpace(r.AgentID) == "" {
		return Reference{}, Receipt{}, fmt.Errorf("%w: update requires generation/hash/agent", ErrInvalidRequest)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Reference{}, Receipt{}, err
	}
	ref, old, present, err := d.loadReference(ctx, c, r.AgentID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	fp := fmt.Sprintf("update\x00%s\x00%s\x00%s\x00%d\x00%s", r.AgentID, r.PublicationID, r.RevisionID, r.ExpectedGeneration, r.ExpectedContentHash)
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := ref.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Reference{}, Receipt{}, ErrIdempotencyConflict
		}
		if prior.ResultReference != nil {
			return cloneReference(*prior.ResultReference), prior.Receipt, nil
		}
		return cloneReference(ref.Reference), prior.Receipt, nil
	}
	a, _, _, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	p, rev, err := d.findPublicationRevision(a, r.PublicationID, r.RevisionID)
	if err != nil {
		return Reference{}, Receipt{}, err
	}
	if p.State != StateActive {
		return Reference{}, Receipt{}, ErrRetired
	}
	if !present || ref.Reference.AgentID == "" {
		return Reference{}, Receipt{}, ErrReferenceNotFound
	}
	if ref.Reference.Generation != r.ExpectedGeneration || ref.Reference.ContentHash != r.ExpectedContentHash {
		return Reference{}, Receipt{}, ErrConflict
	}
	now := d.clock().UTC()
	ref.Reference.PublicationID = p.PublicationID
	ref.Reference.RevisionID = rev.RevisionID
	ref.Reference.ContentHash = rev.ContentHash
	ref.Reference.Generation = rev.Generation
	ref.Reference.RuntimeID = rev.RuntimeID
	ref.Reference.State = p.State
	ref.Reference.UpdatedAt = now
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "update", PublicationID: p.PublicationID, RevisionID: rev.RevisionID, Generation: rev.Generation, State: p.State, BeforeHash: r.ExpectedContentHash, AfterHash: rev.ContentHash, UpdatedAt: now}
	result := cloneReference(ref.Reference)
	ref.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt, ResultReference: &result}
	if err := d.saveReference(ctx, c, r.AgentID, old, present, ref); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Reference{}, Receipt{}, ErrConflict
		}
		return Reference{}, Receipt{}, err
	}
	return result, receipt, nil
}
func (d *StateStoreStore) Remove(ctx context.Context, c identity.Quadruple, r RemoveRequest) (Receipt, error) {
	if err := validateCaller(c); err != nil {
		return Receipt{}, err
	}
	if err := validateKey(r.IdempotencyKey); err != nil {
		return Receipt{}, err
	}
	if r.ExpectedGeneration == 0 || r.ExpectedContentHash == "" || strings.TrimSpace(r.AgentID) == "" {
		return Receipt{}, fmt.Errorf("%w: remove requires generation/hash/agent", ErrInvalidRequest)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return Receipt{}, err
	}
	ref, old, present, err := d.loadReference(ctx, c, r.AgentID)
	if err != nil {
		return Receipt{}, err
	}
	fp := fmt.Sprintf("remove\x00%s\x00%d\x00%s", r.AgentID, r.ExpectedGeneration, r.ExpectedContentHash)
	opKey := durableOpKey(c, r.IdempotencyKey)
	if prior, ok := ref.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return Receipt{}, ErrIdempotencyConflict
		}
		return prior.Receipt, nil
	}
	if !present || ref.Reference.AgentID == "" {
		return Receipt{}, ErrReferenceNotFound
	}
	if ref.Reference.Generation != r.ExpectedGeneration || ref.Reference.ContentHash != r.ExpectedContentHash {
		return Receipt{}, ErrConflict
	}
	now := d.clock().UTC()
	receipt := Receipt{OperationID: r.IdempotencyKey, Operation: "remove", PublicationID: ref.Reference.PublicationID, RevisionID: ref.Reference.RevisionID, Generation: ref.Reference.Generation, State: StateRetired, BeforeHash: ref.Reference.ContentHash, UpdatedAt: now}
	ref.Reference = Reference{}
	ref.Operations[opKey] = operation{Fingerprint: fp, Receipt: receipt}
	if err := d.saveReference(ctx, c, r.AgentID, old, present, ref); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Receipt{}, ErrConflict
		}
		return Receipt{}, err
	}
	return receipt, nil
}
func (d *StateStoreStore) ListReferences(ctx context.Context, c identity.Quadruple) ([]Reference, error) {
	if err := validateCaller(c); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return nil, err
	}
	rows, err := d.state.ListKindForIdentity(ctx, referenceIdentity(c), publicationRefPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Reference, 0, len(rows))
	for _, row := range rows {
		var ref durableReference
		if err := json.Unmarshal(row.Bytes, &ref); err != nil {
			return nil, fmt.Errorf("%w: reference decode: %v", ErrInvalidRequest, err)
		}
		if ref.Reference.AgentID != "" {
			out = append(out, ref.Reference)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}
func (d *StateStoreStore) Resolve(ctx context.Context, c identity.Quadruple, agentID string) (skills.Skill, Metadata, error) {
	if err := validateCaller(c); err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	ref, _, present, err := d.loadReference(ctx, c, agentID)
	if err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	if !present || ref.Reference.AgentID == "" {
		return skills.Skill{}, Metadata{}, ErrReferenceNotFound
	}
	if ref.Reference.RuntimeID != runtimeID(ctx, d.runtime) {
		return skills.Skill{}, Metadata{}, ErrRuntimeMismatch
	}
	a, _, _, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	p, rev, err := d.findPublicationRevision(a, ref.Reference.PublicationID, ref.Reference.RevisionID)
	if err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	if p.State != StateActive || ref.Reference.State != StateActive {
		return skills.Skill{}, Metadata{}, ErrRetired
	}
	if rev.ContentHash != ref.Reference.ContentHash || rev.Generation != ref.Reference.Generation {
		return skills.Skill{}, Metadata{}, ErrContentHashMismatch
	}
	return cloneSkill(rev.Skill), revisionMetadata(p, rev), nil
}
func (d *StateStoreStore) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return nil
}
