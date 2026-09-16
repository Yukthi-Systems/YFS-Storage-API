// Package file provides storage-level CRUD for immutable files: stat,
// existence checks, soft-delete (move to a trash path), permanent purge,
// and duplication. It knows nothing about filenames, folder hierarchy,
// ownership, or sharing — every path it operates on is an opaque,
// complete storage key supplied verbatim by the Rust metadata service,
// which owns that hierarchy entirely.
package file

import (
	"context"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
)

// Service implements file-level storage operations on top of a
// storage.Storage backend.
type Service struct {
	store storage.Storage
}

// New builds a file Service backed by store.
func New(store storage.Storage) *Service {
	return &Service{store: store}
}

// Stat returns storage-level metadata for the file at path.
func (s *Service) Stat(ctx context.Context, path string) (models.FileInfo, error) {
	meta, err := s.store.Stat(ctx, path)
	if err != nil {
		return models.FileInfo{}, err
	}
	return toFileInfo(path, meta), nil
}

// Exists reports whether path has committed content in storage.
func (s *Service) Exists(ctx context.Context, path string) (bool, error) {
	return s.store.Exists(ctx, path)
}

// Restore moves a file back out of trashPath to path.
func (s *Service) Restore(ctx context.Context, trashPath, path string) error {
	return s.store.Move(ctx, trashPath, path)
}

// Purge permanently deletes a trashed file's content at trashPath.
func (s *Service) Purge(ctx context.Context, trashPath string) error {
	return s.store.Delete(ctx, trashPath)
}

// Copy duplicates srcPath's content to dstPath, e.g. to create a new
// version or a copy of a file. Both paths must already be known to the
// Rust metadata service; this call only moves bytes.
func (s *Service) Copy(ctx context.Context, srcPath, dstPath string) error {
	return s.store.Copy(ctx, srcPath, dstPath)
}

// PurgePrefix permanently deletes everything stored under prefix — e.g.
// an entire org's or user's folder, as decided by the Rust metadata
// service — in one call. This is unrecoverable.
func (s *Service) PurgePrefix(ctx context.Context, prefix string) error {
	return s.store.DeletePrefix(ctx, prefix)
}

func toFileInfo(path string, meta storage.FileMeta) models.FileInfo {
	return models.FileInfo{
		Path:        path,
		Size:        meta.Size,
		ContentType: meta.ContentType,
		ETag:        meta.ETag,
		ModifiedAt:  meta.ModTime,
	}
}
