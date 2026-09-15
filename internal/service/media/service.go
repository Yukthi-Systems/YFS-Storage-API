// Package media serves images, video, audio, and PDF content plus their
// thumbnails/previews, and computes the HTTP response headers
// (Content-Type, Content-Disposition, Cache-Control, ETag) each should
// carry. It deliberately does not touch net/http itself — handlers set
// the headers this package computes and then hand the stream to
// http.ServeContent, which is what actually implements Range/206/ETag
// negotiation.
package media

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// Service resolves caller-supplied file/thumbnail/preview paths against
// storage.Storage.
type Service struct {
	store storage.Storage
}

// New builds a media Service backed by store.
func New(store storage.Storage) *Service {
	return &Service{store: store}
}

// Open returns a seekable stream of a file's primary content at path
// plus its metadata, for inline media playback/preview.
func (s *Service) Open(ctx context.Context, path string) (storage.ReadSeekCloser, storage.FileMeta, error) {
	return s.store.OpenSeeker(ctx, path)
}

// OpenThumbnail returns the pre-generated thumbnail stored at path, if
// one exists. Thumbnail generation itself is out of scope for the
// Storage API (see Future Requirements); this only serves whatever
// bytes were previously stored there.
func (s *Service) OpenThumbnail(ctx context.Context, path string) (io.ReadCloser, storage.FileMeta, error) {
	return s.store.Get(ctx, path)
}

// OpenPreview returns the pre-generated preview (e.g. a low-resolution
// render of a document or video) stored at path, if one exists.
func (s *Service) OpenPreview(ctx context.Context, path string) (io.ReadCloser, storage.FileMeta, error) {
	return s.store.Get(ctx, path)
}

// Headers are the HTTP response headers a media/download/stream handler
// should set before calling http.ServeContent.
type Headers struct {
	ContentType        string
	ContentDisposition string
	CacheControl       string
	ETag               string
	LastModified       time.Time
}

// BuildHeaders derives the appropriate response headers for meta. inline
// forces an "inline" Content-Disposition (used by preview/stream
// endpoints); otherwise natively-viewable kinds (image/video/audio/pdf)
// default to inline and everything else defaults to a download
// attachment.
func BuildHeaders(meta storage.FileMeta, filename string, inline bool) Headers {
	kind := utils.ClassifyMedia(meta.ContentType)

	disposition := "attachment"
	cacheControl := "private, max-age=0, must-revalidate"
	if inline || kind == utils.MediaImage || kind == utils.MediaVideo || kind == utils.MediaAudio || kind == utils.MediaPDF {
		disposition = "inline"
		cacheControl = "private, max-age=3600"
	}
	if filename != "" {
		disposition = fmt.Sprintf(`%s; filename=%q`, disposition, filename)
	}

	return Headers{
		ContentType:        meta.ContentType,
		ContentDisposition: disposition,
		CacheControl:       cacheControl,
		ETag:               meta.ETag,
		LastModified:       meta.ModTime,
	}
}

// Apply sets h's headers on the given header map (typically
// http.ResponseWriter.Header()).
func (h Headers) Apply(set func(key, value string)) {
	if h.ContentType != "" {
		set("Content-Type", h.ContentType)
	}
	if h.ContentDisposition != "" {
		set("Content-Disposition", h.ContentDisposition)
	}
	if h.CacheControl != "" {
		set("Cache-Control", h.CacheControl)
	}
	if h.ETag != "" {
		set("ETag", h.ETag)
	}
}
