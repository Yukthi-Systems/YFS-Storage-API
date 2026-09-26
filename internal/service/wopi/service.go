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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
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

// VersionNotifier is implemented by whatever component tells the Rust
// API about a newly created file version, via
// POST /internal/callback/create.
type VersionNotifier interface {
	NotifyCreate(ctx context.Context, cb models.FileOpsCallback) error
}

// NoopVersionNotifier logs the callback and does nothing else. It is
// the default VersionNotifier until a real Rust API client is wired in.
type NoopVersionNotifier struct{}

func (NoopVersionNotifier) NotifyCreate(ctx context.Context, cb models.FileOpsCallback) error {
	slog.Info("version_callback", "file_id", cb.FileID, "file_version", cb.FileVersion, "folder_id", cb.FolderID, "owner_id", cb.OwnerID, "file_location", cb.FileLocation, "hosted_at", cb.HostedAt, "file_size", cb.FileSize, "file_hash", cb.FileHash)
	return nil
}

// Service implements the WOPI host operations against a storage.Storage
// backend and a LockStore.
type Service struct {
	store    storage.Storage
	locks    LockStore
	lockTTL  time.Duration
	notifier VersionNotifier
}

// New builds a WOPI Service. notifier reports newly created versions to
// the Rust API; a nil notifier falls back to NoopVersionNotifier.
func New(store storage.Storage, locks LockStore, lockTTL time.Duration, notifier VersionNotifier) *Service {
	if notifier == nil {
		notifier = NoopVersionNotifier{}
	}
	return &Service{store: store, locks: locks, lockTTL: lockTTL, notifier: notifier}
}

// effectivePath returns where fileID's bytes currently live: the
// version file created earlier in this lock's lifetime, if any,
// otherwise the caller-supplied path unchanged. CheckFileInfo and
// GetFile both need this so that, once a versioning-enabled session's
// first save has redirected writes to a new version file, subsequent
// reads within the same session see what was actually saved instead of
// the stale original.
func (s *Service) effectivePath(ctx context.Context, path, fileID string) string {
	versionPath, created, err := s.locks.GetVersionState(ctx, fileID)
	if err != nil || !created || versionPath == "" {
		return path
	}
	return versionPath
}

// CheckFileInfo answers WOPI's CheckFileInfo for the file at path.
// ownerID, userID and userName are the identity fields the Rust API
// baked into the session's claims at grant time (see
// session.GrantInput) — CheckFileInfo only ever echoes them back, it
// never decides ownership or identity itself.
func (s *Service) CheckFileInfo(ctx context.Context, path, fileID, filename string, canWrite bool, ownerID, userID, userName string) (FileInfo, error) {
	meta, err := s.store.Stat(ctx, s.effectivePath(ctx, path, fileID))
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

// GetFile streams the content at path for Collabora to load — or, if
// fileID's current lock already created a new version this session, the
// content of that version file instead.
func (s *Service) GetFile(ctx context.Context, path, fileID string) (io.ReadCloser, storage.FileMeta, error) {
	return s.store.Get(ctx, s.effectivePath(ctx, path, fileID))
}

// PutFileInput carries everything PutFile needs. FolderID, OwnerID,
// HostedAt, FileVersion and Filename are only used when
// VersioningEnabled is true, to populate the create callback to the Rust
// API.
type PutFileInput struct {
	Path        string
	FileID      string
	LockID      string
	Reader      io.Reader
	Size        int64
	ContentType string
	// VersioningEnabled mirrors the WOPI session's
	// is_file_versioning_enabled flag (models.Claims). When true, the
	// first PutFile of this lock's lifetime is written to a new version
	// file instead of overwriting Path in place, and reported to the
	// Rust API; every later PutFile under the same lock reuses that same
	// version file.
	VersioningEnabled bool
	FolderID          string
	OwnerID           string
	HostedAt          string
	// FileVersion is the version number the new version file is
	// reported under — the session's latest version plus one.
	FileVersion int32
	// Filename is reported as metadata.file_name on the create callback.
	Filename string
}

// PutFile overwrites the content at in.Path with what Collabora saved,
// provided the caller holds the current lock on in.FileID (or the file
// is currently unlocked, which WOPI also permits for zero-byte/new
// files).
//
// When in.VersioningEnabled is set, the first PutFile since the lock was
// acquired (tracked via LockStore.GetVersionState/SetVersionState, not
// this call's own memory) writes to a brand-new storage path instead of
// in.Path, and reports it to the Rust API via s.notifier so it can
// record the new version. Every subsequent PutFile under the same lock
// — regardless of which WOPI session/token makes the call, since WOPI's
// lock protocol already serializes concurrent editors of the same file —
// reuses that same version path and is not reported again. Releasing the
// lock (Unlock, or TTL expiry) clears this state, so the next edit
// session creates a new version again.
func (s *Service) PutFile(ctx context.Context, in PutFileInput) error {
	current, locked, err := s.locks.Get(ctx, in.FileID)
	if err != nil {
		return err
	}
	if locked && current != in.LockID {
		return &LockConflictError{Err: ErrLockMismatch, ConflictLockID: current}
	}

	writePath := in.Path
	creatingVersion := false
	if in.VersioningEnabled {
		versionPath, created, err := s.locks.GetVersionState(ctx, in.FileID)
		if err != nil {
			return err
		}
		if created {
			writePath = versionPath
		} else {
			writePath = newVersionPath(in.Path, in.FileID)
			creatingVersion = true
		}
	}

	var reader io.Reader = in.Reader
	var hashing *utils.HashingReader
	if creatingVersion {
		hashing = utils.NewHashingReader(in.Reader)
		reader = hashing
	}

	meta, err := s.store.Put(ctx, writePath, reader, in.Size, in.ContentType)
	if err != nil {
		return err
	}

	if creatingVersion {
		if err := s.locks.SetVersionState(ctx, in.FileID, writePath, s.lockTTL); err != nil {
			slog.ErrorContext(ctx, "wopi: recording new version state failed", "file_id", in.FileID, "path", writePath, "error", err)
		}

		fileHash := ""
		if hashing != nil {
			fileHash = hashing.Sum256()
		}
		metadata, _ := json.Marshal(map[string]string{
			"file_name": in.Filename,
			"file_type": in.ContentType,
		})
		cb := models.FileOpsCallback{
			FolderID:     in.FolderID,
			FileID:       in.FileID,
			OwnerID:      in.OwnerID,
			FileVersion:  in.FileVersion,
			FileLocation: writePath,
			HostedAt:     in.HostedAt,
			FileSize:     meta.Size,
			Metadata:     metadata,
			FileHash:     fileHash,
		}
		if err := s.notifier.NotifyCreate(ctx, cb); err != nil {
			slog.ErrorContext(ctx, "wopi: new version callback failed", "file_id", in.FileID, "path", writePath, "error", err)
		}
	}

	return nil
}

// newVersionPath derives a fresh storage key for a new version of
// fileID, alongside originalPath's directory, so the original object is
// left untouched and each version gets its own immutable key.
func newVersionPath(originalPath, fileID string) string {
	dir := path.Dir(originalPath)
	ext := path.Ext(originalPath)
	return fmt.Sprintf("%s/versions/%s-%s%s", dir, fileID, uuid.NewString(), ext)
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
// holder. This also clears any versioning state recorded against the
// lock, so the next edit session starts a new version.
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
