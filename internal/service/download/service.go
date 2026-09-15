// Package download serves committed files for download and inline
// preview. It only depends on storage.Storage, so downloads work
// identically regardless of the configured backend.
package download

import (
	"context"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
)

// Service resolves file paths and hands back seekable content for
// http.ServeContent.
type Service struct {
	store storage.Storage
}

// New builds a download Service backed by store.
func New(store storage.Storage) *Service {
	return &Service{store: store}
}

// Open returns a seekable stream of path's content plus its metadata,
// suitable for http.ServeContent. Callers must Close the returned
// stream.
func (s *Service) Open(ctx context.Context, path string) (storage.ReadSeekCloser, storage.FileMeta, error) {
	return s.store.OpenSeeker(ctx, path)
}
