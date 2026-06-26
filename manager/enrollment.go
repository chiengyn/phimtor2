package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// verifyStatus is the outcome of checking a streamer's presented control token
// against its enrollment record. The register handler maps each to an HTTP
// response (see manager/server.go).
type verifyStatus int

const (
	// verifyPending: the instance is known but not yet approved (or was just
	// created on first contact). The streamer should keep retrying.
	verifyPending verifyStatus = iota
	// verifyApproved: approved and the presented control token matches the pinned
	// fingerprint. The register may proceed.
	verifyApproved
	// verifyMismatch: approved, but the presented control token does NOT match the
	// pinned fingerprint — a different machine claiming an approved ID.
	verifyMismatch
)

// Enrollment is one streamer's allow-list entry. It is the only manager state
// persisted to disk: the live instance set and session tokens stay in memory and
// rebuild on re-register. FingerprintHash is the sha256 (hex) of the streamer's
// self-generated control token, pinned at approval time (trust-on-first-use).
type Enrollment struct {
	ID              string    `json:"id"`
	FingerprintHash string    `json:"fingerprintHash"`
	Approved        bool      `json:"approved"`
	FirstSeen       time.Time `json:"firstSeen"`
	ApprovedAt      time.Time `json:"approvedAt,omitempty"`
	LastInternalURL string    `json:"lastInternalURL"`
	LastPublicURL   string    `json:"lastPublicURL"`
}

// EnrollmentStore is the persisted streamer allow-list. Reads/writes are
// mutex-guarded and every mutation flushes the whole map to disk atomically
// (the set is tiny — a handful of streamers).
type EnrollmentStore struct {
	path string

	mu  sync.RWMutex
	set map[string]*Enrollment
}

// NewEnrollmentStore loads the allow-list from <stateDir>/enrollments.json,
// creating the directory if needed. A missing file is not an error (first run).
func NewEnrollmentStore(stateDir string) (*EnrollmentStore, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	s := &EnrollmentStore{
		path: filepath.Join(stateDir, "enrollments.json"),
		set:  make(map[string]*Enrollment),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *EnrollmentStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var list []*Enrollment
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, e := range list {
		s.set[e.ID] = e
	}
	log.Printf("loaded %d streamer enrollment(s) from %s", len(s.set), s.path)
	return nil
}

// flush writes the whole map to disk via a temp file + atomic rename. The caller
// must hold at least a read lock.
func (s *EnrollmentStore) flush() error {
	list := make([]*Enrollment, 0, len(s.set))
	for _, e := range s.set {
		list = append(list, e)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// verify checks a presented control token against the enrollment for id. An
// unknown id is registered as pending (trust-on-first-use) before returning
// verifyPending. URLs are refreshed on every call so the dashboard shows where a
// still-pending streamer is advertising from.
func (s *EnrollmentStore) verify(id, controlToken, internalURL, publicURL string) verifyStatus {
	fp := fingerprint(controlToken)

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.set[id]
	if !ok {
		s.set[id] = &Enrollment{
			ID:              id,
			FingerprintHash: fp,
			Approved:        false,
			FirstSeen:       time.Now(),
			LastInternalURL: internalURL,
			LastPublicURL:   publicURL,
		}
		_ = s.flush()
		return verifyPending
	}

	e.LastInternalURL = internalURL
	e.LastPublicURL = publicURL
	if !e.Approved {
		// Not yet approved: keep re-pinning the latest fingerprint so the operator
		// approves whatever the streamer is currently presenting.
		e.FingerprintHash = fp
		_ = s.flush()
		return verifyPending
	}
	if subtle.ConstantTimeCompare([]byte(e.FingerprintHash), []byte(fp)) == 1 {
		return verifyApproved
	}
	return verifyMismatch
}

// approve marks a pending enrollment approved, pinning its current fingerprint.
// Returns false if the id is unknown.
func (s *EnrollmentStore) approve(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.set[id]
	if !ok {
		return false
	}
	e.Approved = true
	e.ApprovedAt = time.Now()
	_ = s.flush()
	return true
}

// revoke removes an enrollment entirely. Returns false if the id is unknown.
func (s *EnrollmentStore) revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; !ok {
		return false
	}
	delete(s.set, id)
	_ = s.flush()
	return true
}

// list returns a snapshot of all enrollments (pending and approved).
func (s *EnrollmentStore) list() []Enrollment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Enrollment, 0, len(s.set))
	for _, e := range s.set {
		out = append(out, *e)
	}
	return out
}

// fingerprint is the sha256 hex of a control token — what the store persists, so
// a leaked enrollments.json never exposes a usable token.
func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
