// Package wopi implements the WOPI *host* operations Collabora Online
// needs to check out, edit, and lock a file: CheckFileInfo, GetFile,
// PutFile, Lock, Unlock, and RefreshLock. It does not implement Collabora
// itself, and it does not decide who is allowed to edit what — the Rust
// API bakes that decision into the WOPI access token (see
// pkg/types.Claims.CanWrite) before a client ever reaches these
// endpoints.
package wopi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
)

// ErrLockMismatch is returned when a lock operation's lock ID does not
// match the lock currently held on the file. Callers should map this
// to a 409 Conflict with an X-WOPI-Lock header carrying ConflictLockID.
var ErrLockMismatch = errors.New("wopi: lock id mismatch")

// ErrNotLocked is returned by Unlock/RefreshLock when the file has no
// active lock. Callers should map this to a 409 Conflict with an empty
// X-WOPI-Lock header.
var ErrNotLocked = errors.New("wopi: file is not locked")

// LockConflictError carries the lock ID actually held on the file, so
// the API layer can echo it back via X-WOPI-Lock as the WOPI spec
// requires.
type LockConflictError struct {
	Err            error
	ConflictLockID string
}

func (e *LockConflictError) Error() string { return e.Err.Error() }
func (e *LockConflictError) Unwrap() error { return e.Err }

// FileInfo is the subset of the WOPI CheckFileInfo response the Storage
// API can answer on its own (storage-level facts). The Rust API's WOPI
// facade is expected to layer in everything else CheckFileInfo can
// return (ownership, user permissions, breadcrumb, sharing, ...); this
// Service only covers what genuinely lives in object storage.
type FileInfo struct {
	BaseFileName     string `json:"BaseFileName"`
	Size             int64  `json:"Size"`
	Version          string `json:"Version"`
	UserCanWrite     bool   `json:"UserCanWrite"`
	LastModifiedTime string `json:"LastModifiedTime,omitempty"`
	OwnerID          string `json:"OwnerId,omitempty"`
	UserID           string `json:"UserId,omitempty"`
	UserFriendlyName string `json:"UserFriendlyName,omitempty"`
}

// Service implements the WOPI host operations against a storage.Storage
// backend and a LockStore.
type Service struct {
	store   storage.Storage
	locks   LockStore
	lockTTL time.Duration
}

// New builds a WOPI Service.
func New(store storage.Storage, locks LockStore, lockTTL time.Duration) *Service {
	return &Service{store: store, locks: locks, lockTTL: lockTTL}
}

// CheckFileInfo answers WOPI's CheckFileInfo for the file at path.
// ownerID, userID and userName are the identity fields the Rust API
// baked into the session's claims at grant time (see
// session.GrantInput) — CheckFileInfo only ever echoes them back, it
// never decides ownership or identity itself.
func (s *Service) CheckFileInfo(ctx context.Context, path, fileID, filename string, canWrite bool, ownerID, userID, userName string) (FileInfo, error) {
	meta, err := s.store.Stat(ctx, path)
	if err != nil {
		return FileInfo{}, fmt.Errorf("wopi: stat: %w", err)
	}

	if filename == "" {
		filename = fileID
	}

	return FileInfo{
		BaseFileName:     filename,
		Size:             meta.Size,
		Version:          meta.ETag,
		UserCanWrite:     canWrite,
		LastModifiedTime: meta.ModTime.UTC().Format(time.RFC3339),
		OwnerID:          ownerID,
		UserID:           userID,
		UserFriendlyName: userName,
	}, nil
}

// GetFile streams the content at path for Collabora to load.
func (s *Service) GetFile(ctx context.Context, path string) (io.ReadCloser, storage.FileMeta, error) {
	return s.store.Get(ctx, path)
}

// PutFile overwrites the content at path with what Collabora saved,
// provided the caller holds the current lock on fileID (or the file is
// currently unlocked, which WOPI also permits for zero-byte/new files).
func (s *Service) PutFile(ctx context.Context, path, fileID, lockID string, r io.Reader, size int64, contentType string) error {
	current, locked, err := s.locks.Get(ctx, fileID)
	if err != nil {
		return err
	}
	if locked && current != lockID {
		return &LockConflictError{Err: ErrLockMismatch, ConflictLockID: current}
	}

	_, err = s.store.Put(ctx, path, r, size, contentType)
	return err
}

// Lock acquires a new WOPI lock on fileID, or refreshes it if lockID
// already matches the current holder (WOPI treats a repeat LOCK with the
// same ID as a refresh).
func (s *Service) Lock(ctx context.Context, fileID, lockID string) error {
	current, locked, err := s.locks.Get(ctx, fileID)
	if err != nil {
		return err
	}
	if locked && current != lockID {
		return &LockConflictError{Err: ErrLockMismatch, ConflictLockID: current}
	}
	return s.locks.Set(ctx, fileID, lockID, s.lockTTL)
}

// Unlock releases fileID's lock, provided lockID matches the current
// holder.
func (s *Service) Unlock(ctx context.Context, fileID, lockID string) error {
	current, locked, err := s.locks.Get(ctx, fileID)
	if err != nil {
		return err
	}
	if !locked {
		return &LockConflictError{Err: ErrNotLocked, ConflictLockID: ""}
	}
	if current != lockID {
		return &LockConflictError{Err: ErrLockMismatch, ConflictLockID: current}
	}
	return s.locks.Delete(ctx, fileID)
}

// RefreshLock extends the TTL of fileID's existing lock, provided
// lockID matches the current holder.
func (s *Service) RefreshLock(ctx context.Context, fileID, lockID string) error {
	current, locked, err := s.locks.Get(ctx, fileID)
	if err != nil {
		return err
	}
	if !locked {
		return &LockConflictError{Err: ErrNotLocked, ConflictLockID: ""}
	}
	if current != lockID {
		return &LockConflictError{Err: ErrLockMismatch, ConflictLockID: current}
	}
	return s.locks.Set(ctx, fileID, lockID, s.lockTTL)
}
