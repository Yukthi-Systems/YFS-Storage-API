// Package upload implements the Upload Manager: verifying a completed
// resumable (tus) upload and committing it from the org's uploads/
// staging area into its permanent files/ location. It depends only on
// the storage.Storage interface, so it never knows whether bytes end up
// on local disk or in S3/MinIO/Ceph.
package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// Notifier is implemented by whatever component tells the Rust API
// about an upload's outcome: POST /internal/callback/create on success,
// POST /internal/callback/delete on failure or cancellation.
type Notifier interface {
	NotifyCreate(ctx context.Context, cb models.FileOpsCallback) error
	NotifyDelete(ctx context.Context, cb models.FileOpsCallback) error
}

// NoopNotifier logs the callback and does nothing else. It is the
// default Notifier until a real Rust API client is wired in.
type NoopNotifier struct{}

func (NoopNotifier) NotifyCreate(ctx context.Context, cb models.FileOpsCallback) error {
	logCallback("create", cb)
	return nil
}

func (NoopNotifier) NotifyDelete(ctx context.Context, cb models.FileOpsCallback) error {
	logCallback("delete", cb)
	return nil
}

func logCallback(op string, cb models.FileOpsCallback) {
	slog.Info("upload_callback", "op", op, "folder_id", cb.FolderID, "file_id", cb.FileID, "owner_id", cb.OwnerID, "file_version", cb.FileVersion, "file_location", cb.FileLocation, "hosted_at", cb.HostedAt, "file_size", cb.FileSize, "file_hash", cb.FileHash)
}

// randomFileHash produces a sha256-shaped placeholder for FileOpsCallBack.
// file_hash on a delete callback, where there is no real checksum to
// report but the Rust API still requires the field.
func randomFileHash() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Manager commits finished tus uploads into permanent file storage.
type Manager struct {
	store    storage.Storage
	notifier Notifier
}

// NewManager builds an upload Manager backed by store, delivering
// completion events through notifier.
func NewManager(store storage.Storage, notifier Notifier) *Manager {
	if notifier == nil {
		notifier = NoopNotifier{}
	}
	return &Manager{store: store, notifier: notifier}
}

// CommitInput describes a finished upload ready to be verified and
// promoted from uploads/ into its final, caller-supplied location.
type CommitInput struct {
	UploadID string
	// FileID is the permanent file ID this upload becomes. Callers
	// typically reuse UploadID (a session results in exactly one file).
	FileID string
	// Path is the complete destination storage key, given verbatim by
	// the Rust API (via the upload token's claims) at session-creation
	// time.
	Path string
	// FolderID and OwnerID identify where this file lives in the Rust
	// metadata service's hierarchy, given verbatim via the upload
	// token's claims. Carried through only to report back on the
	// upload-result callback.
	FolderID string
	OwnerID  string
	// FileVersion is the upload token's claims.Version, carried through
	// only to report back on the upload-result callback.
	FileVersion string
	// HostedAt is the base path the Rust API originally sent alongside
	// file_location (see claims.HostedAt). Carried through only to
	// report back, unchanged, on the upload-result callback.
	HostedAt string
	// Metadata is client-supplied tus upload metadata (e.g. filename,
	// filetype), reshaped by the caller into the JSON object reported on
	// the upload-result callback's Metadata field. Nil reports as "{}".
	Metadata json.RawMessage
	// Reader streams the raw uploaded bytes exactly once.
	Reader io.Reader
	// Size is the size reported by the upload protocol (tus Upload-Length).
	Size int64
	// MaxAllowedSize is the ceiling from the upload's session claims; <=0
	// means unconstrained.
	MaxAllowedSize int64
}

// Commit verifies size limits, sniffs the content type, streams the
// upload into uploads/<upload-id>, computes its checksum as a side effect
// of that single pass, and promotes it to its final Path. Either way —
// success or failure — it reports the outcome to the Rust API via
// m.notifier before returning: a create callback on success, a delete
// callback on failure so the Rust API drops the version it reserved for
// this upload. The delete callback carries a random placeholder
// file_hash (there is no real checksum) and the error itself is only
// ever logged, never sent.
func (m *Manager) Commit(ctx context.Context, in CommitInput) (result models.CommitResult, err error) {
	defer func() {
		cb := models.FileOpsCallback{
			FolderID:     in.FolderID,
			FileID:       in.FileID,
			OwnerID:      in.OwnerID,
			FileVersion:  parseFileVersion(in.FileVersion),
			FileLocation: in.Path,
			HostedAt:     in.HostedAt,
			FileSize:     result.Size,
			Metadata:     in.Metadata,
			FileHash:     result.Checksum,
		}
		success := err == nil
		var notifyErr error
		if success {
			notifyErr = m.notifier.NotifyCreate(ctx, cb)
		} else {
			cb.FileHash = randomFileHash()
			notifyErr = m.notifier.NotifyDelete(ctx, cb)
		}
		if notifyErr != nil {
			slog.Error("upload callback failed", "error", notifyErr, "file_location", cb.FileLocation, "upload_id", in.UploadID, "success", success)
		}
	}()

	if in.MaxAllowedSize > 0 && in.Size > in.MaxAllowedSize {
		return models.CommitResult{}, fmt.Errorf("upload: size %d exceeds max allowed %d", in.Size, in.MaxAllowedSize)
	}

	contentType, sniffed, err := utils.SniffContentType(in.Reader)
	if err != nil {
		return models.CommitResult{}, fmt.Errorf("upload: detecting content type: %w", err)
	}
	hashing := utils.NewHashingReader(sniffed)

	uploadKey := utils.UploadKey(in.UploadID)
	meta, err := m.store.Put(ctx, uploadKey, hashing, in.Size, contentType)
	if err != nil {
		return models.CommitResult{}, fmt.Errorf("upload: staging to uploads/: %w", err)
	}
	if in.Size >= 0 && meta.Size != in.Size {
		_ = m.store.Delete(ctx, uploadKey)
		return models.CommitResult{}, fmt.Errorf("upload: staged size %d does not match reported size %d", meta.Size, in.Size)
	}

	if err := m.store.Move(ctx, uploadKey, in.Path); err != nil {
		return models.CommitResult{}, fmt.Errorf("upload: promoting to final path: %w", err)
	}

	return models.CommitResult{
		FileID:      in.FileID,
		Size:        meta.Size,
		ContentType: contentType,
		Checksum:    hashing.Sum256(),
	}, nil
}

// parseFileVersion converts a claims.Version string (itself
// strconv.Itoa'd from an int at session-creation time) back to the
// int32 FileOpsCallBack.file_version expects. An unparseable/empty
// version reports as 0 rather than failing the callback.
func parseFileVersion(v string) int32 {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}

	return int32(n)
}

// Abort discards a staged-but-never-completed upload.
func (m *Manager) Abort(ctx context.Context, uploadID string) error {
	return m.store.Delete(ctx, utils.UploadKey(uploadID))
}

// CancelInput describes a tus upload that was explicitly terminated
// (client DELETE) before it ever reached Commit. Unlike CommitInput, no
// bytes ever reached the store.Storage backend — tusd's own termination
// already cleared its local staging copy — so there is nothing here to
// promote or clean up, only the Rust API to notify.
type CancelInput struct {
	UploadID    string
	FileID      string
	Path        string
	FolderID    string
	OwnerID     string
	FileVersion string
	HostedAt    string
	Metadata    json.RawMessage
}

// NotifyCancelled reports an explicitly cancelled, never-finished
// upload to the Rust API as a delete callback. Without
// this, a cancelled upload never reaches Commit and is otherwise
// reported nowhere. As with Commit's failure path, FileHash carries a
// random placeholder and any notifier error is only ever logged.
func (m *Manager) NotifyCancelled(ctx context.Context, in CancelInput) {
	cb := models.FileOpsCallback{
		FolderID:     in.FolderID,
		FileID:       in.FileID,
		OwnerID:      in.OwnerID,
		FileVersion:  parseFileVersion(in.FileVersion),
		FileLocation: in.Path,
		HostedAt:     in.HostedAt,
		Metadata:     in.Metadata,
		FileHash:     randomFileHash(),
	}
	if err := m.notifier.NotifyDelete(ctx, cb); err != nil {
		slog.Error("upload cancel callback failed", "error", err, "file_location", cb.FileLocation, "upload_id", in.UploadID)
	}
}
