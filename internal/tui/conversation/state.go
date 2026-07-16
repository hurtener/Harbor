package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

const (
	stateVersion     = 1
	maxStateEntries  = 64
	maxDraftBytes    = 64 << 10
	maxListBytes     = 256 << 10
	maxCollapsedIDs  = 512
	maxStateKeyBytes = 1024
	stateTTL         = 30 * 24 * time.Hour
)

// InteractionState is bounded local-only UI state. It must never contain a
// Runtime entity row, event payload, credential, or transcript content.
type InteractionState struct {
	Identity           types.IdentityScope `json:"identity"`
	RuntimeFingerprint string              `json:"runtime_fingerprint"`
	Draft              string              `json:"draft,omitempty"`
	History            []string            `json:"history,omitempty"`
	Stash              []string            `json:"stash,omitempty"`
	ScrollBlockID      string              `json:"scroll_block_id,omitempty"`
	// ExpandedReasoning / ExpandedTools hold the block IDs the operator has
	// explicitly expanded. Reasoning and tool detail are COLLAPSED by default,
	// so these record deliberate exceptions rather than the common case.
	ExpandedReasoning []string  `json:"expanded_reasoning,omitempty"`
	ExpandedTools     []string  `json:"expanded_tools,omitempty"`
	SidebarWidth      int       `json:"sidebar_width,omitempty"`
	SidebarOpen       bool      `json:"sidebar_open,omitempty"`
	Theme             string    `json:"theme,omitempty"`
	ReducedMotion     bool      `json:"reduced_motion,omitempty"`
	Compact           bool      `json:"compact,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type stateFile struct {
	Version     int                         `json:"version"`
	LastSession map[string]string           `json:"last_session,omitempty"`
	Entries     map[string]InteractionState `json:"entries"`
}

// Store atomically persists bounded interaction state.
type Store struct {
	path string
	now  func() time.Time
	mu   *sync.Mutex
}

// NewStore constructs a store at path.
func NewStore(path string) Store { return Store{path: path, now: time.Now, mu: &sync.Mutex{}} }

// Load reads, validates, expires, and returns state for the full identity and Runtime fingerprint.
func (s Store) Load(identity types.IdentityScope, fingerprint string) (InteractionState, bool, error) {
	s.lock()
	defer s.unlock()
	file, err := s.read()
	if err != nil {
		return InteractionState{}, false, err
	}
	value, ok := file.Entries[stateKey(identity, fingerprint)]
	if !ok {
		return InteractionState{}, false, nil
	}
	if err := validateInteractionState(value); err != nil {
		return InteractionState{}, false, fmt.Errorf("tui state: malformed entry: %w", err)
	}
	if value.UpdatedAt.IsZero() {
		return InteractionState{}, false, errors.New("tui state: malformed entry update time")
	}
	if scopeKey(value.Identity) != scopeKey(identity) || value.RuntimeFingerprint != fingerprint {
		return InteractionState{}, false, errors.New("tui state: malformed entry identity or Runtime fingerprint")
	}
	if s.now().Sub(value.UpdatedAt) > stateTTL {
		delete(file.Entries, stateKey(identity, fingerprint))
		s.evict(&file)
		if err := s.write(file); err != nil {
			return InteractionState{}, false, err
		}
		return InteractionState{}, false, nil
	}
	return value, true, nil
}

// LastSession returns the durable reference for a tenant/user and Runtime fingerprint.
func (s Store) LastSession(tenant, user, fingerprint string) (string, error) {
	s.lock()
	defer s.unlock()
	file, err := s.read()
	if err != nil {
		return "", err
	}
	beforeEntries, beforeLast := len(file.Entries), len(file.LastSession)
	s.evict(&file)
	if beforeEntries != len(file.Entries) || beforeLast != len(file.LastSession) {
		if err := s.write(file); err != nil {
			return "", err
		}
	}
	return file.LastSession[tenant+"/"+user+"@"+fingerprint], nil
}

// Save validates and atomically replaces one interaction entry.
func (s Store) Save(value InteractionState) error {
	s.lock()
	defer s.unlock()
	if err := validateInteractionState(value); err != nil {
		return err
	}
	value.UpdatedAt = s.now().UTC()
	file, err := s.read()
	if err != nil {
		return err
	}
	if file.Entries == nil {
		file.Entries = map[string]InteractionState{}
	}
	if file.LastSession == nil {
		file.LastSession = map[string]string{}
	}
	file.Entries[stateKey(value.Identity, value.RuntimeFingerprint)] = value
	file.LastSession[value.Identity.Tenant+"/"+value.Identity.User+"@"+value.RuntimeFingerprint] = value.Identity.Session
	s.evict(&file)
	return s.write(file)
}

func validateInteractionState(value InteractionState) error {
	if err := validateScope(value.Identity); err != nil {
		return err
	}
	if value.RuntimeFingerprint == "" || len(value.RuntimeFingerprint) > maxStateKeyBytes {
		return errors.New("tui state: Runtime fingerprint missing or exceeds bound")
	}
	if len(value.Draft) > maxDraftBytes {
		return errors.New("tui state: draft exceeds bound")
	}
	if len(value.History) > 50 || len(value.Stash) > 50 || stringsBytes(value.History) > maxListBytes || stringsBytes(value.Stash) > maxListBytes {
		return errors.New("tui state: history or stash exceeds bound")
	}
	if len(value.ExpandedReasoning) > maxCollapsedIDs || len(value.ExpandedTools) > maxCollapsedIDs || stringsBytes(value.ExpandedReasoning) > maxListBytes || stringsBytes(value.ExpandedTools) > maxListBytes {
		return errors.New("tui state: collapsed detail state exceeds bound")
	}
	if len(value.ScrollBlockID) > maxStateKeyBytes {
		return errors.New("tui state: scroll anchor exceeds bound")
	}
	return nil
}

func stringsBytes(values []string) int {
	total := 0
	for _, value := range values {
		total += len(value)
	}
	return total
}

func (s Store) lock() {
	if s.mu != nil {
		s.mu.Lock()
	}
}
func (s Store) unlock() {
	if s.mu != nil {
		s.mu.Unlock()
	}
}

func (s Store) read() (stateFile, error) {
	file := stateFile{Version: stateVersion, Entries: map[string]InteractionState{}, LastSession: map[string]string{}}
	info, statErr := os.Lstat(s.path)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return stateFile{}, errors.New("tui state: symbolic-link state file rejected")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return stateFile{}, fmt.Errorf("tui state: permissions %04o are too permissive; require 0600", info.Mode().Perm())
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return stateFile{}, fmt.Errorf("tui state: inspect: %w", statErr)
	}
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return stateFile{}, fmt.Errorf("tui state: read: %w", err)
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return stateFile{}, fmt.Errorf("tui state: malformed: %w", err)
	}
	if file.Version == 0 {
		file.Version = stateVersion
		if file.Entries == nil {
			file.Entries = map[string]InteractionState{}
		}
		if file.LastSession == nil {
			file.LastSession = map[string]string{}
		}
	}
	if file.Version != stateVersion {
		return stateFile{}, fmt.Errorf("tui state: unsupported version %d", file.Version)
	}
	if file.Entries == nil || file.LastSession == nil {
		return stateFile{}, errors.New("tui state: malformed maps")
	}
	return file, nil
}
func (s Store) evict(file *stateFile) {
	now := s.now()
	for key, value := range file.Entries {
		if now.Sub(value.UpdatedAt) > stateTTL {
			delete(file.Entries, key)
		}
	}
	if len(file.Entries) > maxStateEntries {
		type row struct {
			key string
			at  time.Time
		}
		rows := make([]row, 0, len(file.Entries))
		for key, value := range file.Entries {
			rows = append(rows, row{key, value.UpdatedAt})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
		for _, row := range rows[:len(rows)-maxStateEntries] {
			delete(file.Entries, row.key)
		}
	}
	for lastKey, session := range file.LastSession {
		retained := false
		for _, value := range file.Entries {
			key := value.Identity.Tenant + "/" + value.Identity.User + "@" + value.RuntimeFingerprint
			if key == lastKey && value.Identity.Session == session {
				retained = true
				break
			}
		}
		if !retained {
			delete(file.LastSession, lastKey)
		}
	}
}
func (s Store) write(file stateFile) error {
	parent := filepath.Dir(s.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("tui state: create directory: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("tui state: inspect directory: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("tui state: symbolic-link state directory rejected")
	}
	if err = os.Chmod(parent, 0o700); err != nil {
		return fmt.Errorf("tui state: secure directory: %w", err)
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("tui state: encode: %w", err)
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(parent, ".tui-state-*")
	if err != nil {
		return fmt.Errorf("tui state: create temporary: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(body)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("tui state: write temporary: %w", err)
	}
	if err = os.Rename(name, s.path); err != nil {
		return fmt.Errorf("tui state: replace: %w", err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("tui state: open parent for sync: %w", err)
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil {
		return fmt.Errorf("tui state: sync parent: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("tui state: close parent: %w", closeErr)
	}
	return nil
}
func stateKey(id types.IdentityScope, fingerprint string) string {
	return scopeKey(id) + "@" + fingerprint
}
