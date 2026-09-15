package wopi

import (
	"context"
	"sync"
	"time"
)

// LockStore persists the WOPI lock currently held (if any) for a given
// file, keyed by its opaque fileID. The default MemoryLockStore is
// process-local, which is fine for a single instance; a horizontally
// scaled deployment should back this with something shared (Redis, etc.)
// without touching the Service or API layer above it.
type LockStore interface {
	Get(ctx context.Context, fileID string) (lockID string, ok bool, err error)
	Set(ctx context.Context, fileID, lockID string, ttl time.Duration) error
	Delete(ctx context.Context, fileID string) error
}

type lockEntry struct {
	lockID    string
	expiresAt time.Time
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

func (m *MemoryLockStore) Set(ctx context.Context, fileID, lockID string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.locks[fileID] = lockEntry{lockID: lockID, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *MemoryLockStore) Delete(ctx context.Context, fileID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, fileID)
	return nil
}
