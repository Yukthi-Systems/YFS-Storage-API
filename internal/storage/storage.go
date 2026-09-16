// Package storage defines the storage-backend abstraction used by every
// upload, download, streaming, and WOPI service. No caller may depend on
// whether the concrete implementation is the local filesystem, S3, MinIO,
// Ceph, or any future backend — everything goes through the Storage
// interface below.
//
// Keys are flat, backend-agnostic strings decided entirely by the Rust
// metadata service and passed through verbatim (e.g.
// "acme-corp/reports/2026/q3.pdf"), aside from in-progress tus uploads,
// which stage under internal/utils.UploadKey until their destination key
// is known. Drivers must not interpret keys beyond treating them as
// opaque paths/object names.
package storage

import (
	"context"
	"io"
	"time"
)

// FileMeta is the backend-reported metadata for a stored file.
type FileMeta struct {
	Size        int64
	ContentType string
	ETag        string
	ModTime     time.Time
}

// ReadSeekCloser is the concrete type handed back for range/seek-capable
// reads (e.g. to feed http.ServeContent for downloads, streaming and
// WOPI GetFile).
//
// It represents a single stateful cursor over one file and is not safe
// for concurrent Read/Seek calls. Each caller (e.g. each HTTP request)
// must obtain and use its own instance via OpenSeeker rather than sharing
// one across requests.
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// Storage is the sole abstraction upload, download, streaming, media and
// WOPI services are allowed to depend on. Adding a new backend (GCS,
// Azure Blob, ...) means writing one new implementation of this interface
// — no other package changes.
type Storage interface {
	// Put writes the contents of r to key. size may be -1 if unknown
	// (the driver will not be able to pre-allocate/validate length in
	// that case). Returns the resulting file metadata.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (FileMeta, error)

	// Get opens the full file for reading. Callers must Close the
	// returned ReadCloser.
	Get(ctx context.Context, key string) (io.ReadCloser, FileMeta, error)

	// GetRange opens length bytes of the file starting at offset. If
	// length < 0, reads to the end of the file. Use this when the caller
	// already knows exactly which byte range it needs in one shot (e.g.
	// an explicit range API, media processing, or another internal
	// service) — it maps directly onto a single backend range request.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// OpenSeeker opens the file as a stateful, seekable stream suitable
	// for http.ServeContent, which issues its own internal Seek/Read
	// calls to satisfy HTTP Range requests. Unlike GetRange, the caller
	// does not know in advance which bytes it needs; implementations are
	// free to fetch data lazily and reuse/cache recently-fetched ranges
	// across Seek/Read calls, but must never buffer the entire file.
	OpenSeeker(ctx context.Context, key string) (ReadSeekCloser, FileMeta, error)

	// Delete removes the file at key. Deleting a non-existent key is
	// not an error.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every file whose key starts with prefix
	// (interpreted as a "directory" boundary, i.e. prefix + "/"). This is
	// the bulk-wipe primitive for a caller-supplied path prefix (e.g. an
	// entire org's or user's folder, as decided by the Rust metadata
	// service) in one call, rather than requiring callers to enumerate
	// individual file IDs. Implementations must refuse an empty prefix to
	// avoid accidentally wiping an entire backend.
	DeletePrefix(ctx context.Context, prefix string) error

	// Exists reports whether a file exists at key.
	Exists(ctx context.Context, key string) (bool, error)

	// Stat returns metadata for the file at key without opening it.
	Stat(ctx context.Context, key string) (FileMeta, error)

	// Move relocates a file from srcKey to dstKey, using the most
	// efficient primitive available (rename for local disk,
	// CopyObject+DeleteObject for S3-compatible backends).
	//
	// The operation guarantees the destination contains the complete
	// file before the source is removed, but implementations are not
	// required to provide atomic rename semantics: object-storage
	// backends implement this as Copy followed by Delete. If the copy
	// succeeds but deleting the source then fails, Move returns an error
	// while both srcKey and dstKey exist — callers must not assume the
	// source is gone just because Move returned an error. Implementations
	// must never delete the source before the destination is confirmed
	// complete, and must never delete the destination as a "rollback" for
	// a failed source delete (a leftover duplicate is preferred over data
	// loss).
	//
	// Moving a file onto itself (srcKey == dstKey) is a no-op. Moving
	// onto an already-existing dstKey overwrites it.
	Move(ctx context.Context, srcKey, dstKey string) error

	// Copy duplicates a file from srcKey to dstKey, leaving srcKey
	// intact.
	Copy(ctx context.Context, srcKey, dstKey string) error

	// ListPrefix calls fn once for every file stored under prefix
	// (interpreted as a directory boundary, i.e. prefix + "/"), stopping
	// and returning fn's error the moment it returns one. A prefix with
	// nothing under it (including one that doesn't exist at all) simply
	// yields no calls to fn, not an error.
	//
	// Implementations must stream keys rather than buffering the whole
	// listing in memory, so callers (e.g. a recursive delete walking an
	// entire org's or user's storage) can use it over arbitrarily large
	// subtrees. Callers that want to bound how much work one ListPrefix
	// call does should have fn itself check ctx and return its error,
	// since ListPrefix does not otherwise limit how many keys it visits.
	ListPrefix(ctx context.Context, prefix string, fn func(key string) error) error
}

// SpaceUsage reports a storage backend's capacity, in bytes, at a point
// in time.
type SpaceUsage struct {
	TotalBytes     int64
	UsedBytes      int64
	AvailableBytes int64
}

// UsedPercent returns the fraction of TotalBytes currently used, as a
// value in [0, 100]. It returns 0 if TotalBytes is not positive, so
// callers with no meaningful total never trip a percentage-based check.
func (u SpaceUsage) UsedPercent() float64 {
	if u.TotalBytes <= 0 {
		return 0
	}
	return float64(u.UsedBytes) / float64(u.TotalBytes) * 100
}

// SpaceReporter is optionally implemented by a Storage backend that has
// a meaningful, finite notion of total capacity (e.g. a local disk).
// Backends without one (S3 and other object stores are effectively
// unbounded from the Storage API's point of view) simply do not
// implement it; callers doing space-based admission control must treat
// a failed type assertion as "not applicable," not as an error.
type SpaceReporter interface {
	SpaceUsage(ctx context.Context) (SpaceUsage, error)
}

// ErrNotFound is returned by Get/GetRange/Stat/OpenSeeker when key does
// not exist. Drivers must wrap their backend-specific "not found" errors
// so callers can use errors.Is(err, storage.ErrNotFound).
var ErrNotFound = notFoundError{}

type notFoundError struct{}

func (notFoundError) Error() string { return "storage: file not found" }
