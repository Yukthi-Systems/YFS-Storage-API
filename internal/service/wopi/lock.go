package wopi

import (
	"context"
	"sync"
	"time"
)

// LockStore persists the WOPI lock currently held (if any) for a given
// file, keyed by its opaque fileID, plus the versioning state scoped to
// that same lock's lifetime (see GetVersionState/SetVersionState). The
// default MemoryLockStore is process-local, which is fine for a single
// instance; a horizontally scaled deployment should back this with
// something shared (Redis, etc.) without touching the Service or API
// layer above it.
type LockStore interface {
	Get(ctx context.Context, fileID string) (lockID string, ok bool, err error)
	Set(ctx context.Context, fileID, lockID string, ttl time.Duration) error
	Delete(ctx context.Context, fileID string) error
	// GetVersionState reports whether a new version file has already
	// been created during fileID's current lock lifetime, and the
	// storage path it was written to if so.
	GetVersionState(ctx context.Context, fileID string) (versionPath string, created bool, err error)
	// SetVersionState records that a new version file has been created
	// at versionPath for fileID. It upserts: if no lock entry exists yet
	// (e.g. a versioned PutFile arrived before any WOPI Lock call), one
	// is created with the given ttl so the state still survives long
	// enough to be reused by the next PutFile.
	SetVersionState(ctx context.Context, fileID, versionPath string, ttl time.Duration) error
}

type lockEntry struct {
	lockID    string
	expiresAt time.Time
	// versionCreated and versionPath track, for the file currently
	// identified by this entry's key, whether a new version file has
	// already been created during this lock's lifetime. They live here
	// (rather than in a separate map) because the lock's lifetime —
	// acquired on first edit, released on Unlock/expiry — is exactly the
	// "one edit session" boundary versioning needs to key off.
	versionCreated bool
	versionPath    string
}

// MemoryLockStore is an in-memory, mutex-guarded LockStore.
type MemoryLockStore struct {
	mu    sync.Mutex
	locks map[string]lockEntry
}

// NewMemoryLockStore builds an empty MemoryLockStore.
func NewMemoryLockStore() *MemoryLockStore {
	return &MemoryLockStore{locks: make(map[string]lockEntry)}
}

func (m *MemoryLockStore) Get(ctx context.Context, fileID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.locks[fileID]
	if !ok {
		return "", false, nil
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.locks, fileID)
		return "", false, nil
	}
	return entry.lockID, true, nil
}

// Set installs fileID's lock, preserving any versioning state already
// recorded against a still-live entry (a repeat LOCK/REFRESH_LOCK call
// continuing the same edit session) rather than clobbering it. An
// expired or absent entry starts fresh, which is what makes "close and
// reopen the file" produce a new version on the next edit.
func (m *MemoryLockStore) Set(ctx context.Context, fileID, lockID string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := lockEntry{lockID: lockID, expiresAt: time.Now().Add(ttl)}
	if existing, ok := m.locks[fileID]; ok && !time.Now().After(existing.expiresAt) {
		entry.versionCreated = existing.versionCreated
		entry.versionPath = existing.versionPath
	}
	m.locks[fileID] = entry
	return nil
}

func (m *MemoryLockStore) Delete(ctx context.Context, fileID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, fileID)
	return nil
}

func (m *MemoryLockStore) GetVersionState(ctx context.Context, fileID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.locks[fileID]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false, nil
	}
	return entry.versionPath, entry.versionCreated, nil
}

func (m *MemoryLockStore) SetVersionState(ctx context.Context, fileID, versionPath string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.locks[fileID]
	if !ok || time.Now().After(entry.expiresAt) {
		entry = lockEntry{expiresAt: time.Now().Add(ttl)}
	}
	entry.versionCreated = true
	entry.versionPath = versionPath
	m.locks[fileID] = entry
	return nil
}
