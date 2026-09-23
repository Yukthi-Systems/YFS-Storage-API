// Package session issues the short-lived, single-purpose grants (upload,
// download, stream, WOPI) that the Rust API requests on behalf of an
// already-authorized user and relays to the UI/desktop/mobile client.
// This package never checks permissions itself — by the time it is
// called, the Rust API has already decided the caller is allowed to
// perform the action; session only mints the token and URL for it.
package session

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
	"github.com/google/uuid"
)

// ErrStorageFull is returned by CreateUploadSession when the storage
// backend's used capacity is at or above the configured admission
// threshold. It is a transient condition — callers should surface it as
// a 507 Insufficient Storage response with a Retry-After hint, not as a
// hard failure.
var ErrStorageFull = errors.New("session: storage capacity threshold reached")

// URLBases holds the public-facing path prefixes used to build the URLs
// returned alongside each issued token.
type URLBases struct {
	Upload   string // e.g. "/upload/tus/"
	Download string // e.g. "/download/"
	Stream   string // e.g. "/stream/"
	WOPI     string // e.g. "/wopi/files/"
}

// Service mints Storage API sessions (token + URL pairs) for each action
// kind.
type Service struct {
	issuer     *token.Issuer
	store      storage.Storage
	maxUsedPct float64
	urls       URLBases
}

// New builds a session Service. maxUsedPct gates CreateUploadSession on
// store's reported capacity (see checkCapacity); 0 disables the check.
func New(issuer *token.Issuer, store storage.Storage, maxUsedPct float64, urls URLBases) *Service {
	return &Service{issuer: issuer, store: store, maxUsedPct: maxUsedPct, urls: urls}
}

// checkCapacity enforces the configured storage admission threshold. It
// is a no-op whenever there is nothing meaningful to check: the
// threshold is disabled (maxUsedPct <= 0), or the backend has no finite
// notion of capacity (e.g. S3) and so doesn't implement
// storage.SpaceReporter.
func (s *Service) checkCapacity(ctx context.Context) error {
	if s.maxUsedPct <= 0 || s.store == nil {
		return nil
	}
	reporter, ok := s.store.(storage.SpaceReporter)
	if !ok {
		return nil
	}
	usage, err := reporter.SpaceUsage(ctx)
	if err != nil {
		return fmt.Errorf("session: checking storage capacity: %w", err)
	}
	if usage.UsedPercent() >= s.maxUsedPct {
		return ErrStorageFull
	}
	return nil
}

// UploadFileInput describes one file within a requested (possibly
// multi-file) upload session request.
type UploadFileInput struct {
	// Path is the complete destination storage key, given verbatim by
	// the Rust API. The upload is committed here once finished.
	Path string
	// FileID is the permanent file ID this upload authorizes, supplied
	// by the Rust API. It stays the same across every version of a
	// file.
	FileID string
	// FolderID and OwnerID identify where this file lives in the Rust
	// metadata service's hierarchy. The Storage API attaches no meaning
	// to them beyond caching them in the session (Redis) alongside the
	// token.
	FolderID      string
	OwnerID       string
	FileVersion   int32
	Filename      string
	ContentType   string
	MaxUploadSize int64
	// TTL is this file's own session lifetime, requested by the Rust
	// API.
	TTL time.Duration
	// HostedAt is the base path this file is hosted under, given
	// verbatim by the Rust API. The Storage API attaches no meaning to
	// it beyond caching it in the session (Redis) so it can be echoed
	// back on the upload-result callback.
	HostedAt string
}

// NewUploadInput describes a requested batch of upload sessions — one
// or more files, each independently authorized (own token, own TTL).
type NewUploadInput struct {
	Files []UploadFileInput
}

// CreateUploadSession mints one independent session token per file in
// in.Files — not a single shared token — since tus itself only ever
// uploads one file per resource, so a per-batch token would just add
// indirection without anything using it. Every file also gets its own
// single-use internal upload ID (never exposed to the caller) that pins
// where tus stages its bytes, and all files from one call share a
// BatchID stamped into their claims (unused today, but there for a
// future "act on this whole batch" endpoint).
func (s *Service) CreateUploadSession(ctx context.Context, in NewUploadInput) (models.UploadSessionBatch, error) {
	if len(in.Files) == 0 {
		return models.UploadSessionBatch{}, fmt.Errorf("session: at least one file is required")
	}
	if err := s.checkCapacity(ctx); err != nil {
		return models.UploadSessionBatch{}, err
	}

	batchID := uuid.NewString()
	sessions := make([]models.UploadSession, 0, len(in.Files))
	for _, f := range in.Files {
		if f.FileID == "" || f.Path == "" {
			return models.UploadSessionBatch{}, fmt.Errorf("session: file id and path must not be empty")
		}

		tok, expiresAt, err := s.issuer.GenerateUploadToken(ctx, models.Claims{
			Path:     f.Path,
			UploadID: uuid.NewString(),
			FileID:   f.FileID,
			FolderID: f.FolderID,
			OwnerID:  f.OwnerID,
			HostedAt: f.HostedAt,
			BatchID:  batchID,
			Version:  strconv.FormatInt(int64(f.FileVersion), 10),
			Filename: f.Filename,
			// ContentType:   f.ContentType,
			MaxUploadSize: f.MaxUploadSize,
		}, f.TTL)
		if err != nil {
			return models.UploadSessionBatch{}, fmt.Errorf("session: generating upload token for file_id %q: %w", f.FileID, err)
		}

		sessions = append(sessions, models.UploadSession{
			Token:       tok,
			FileID:      f.FileID,
			FileVersion: f.FileVersion,
			ExpiresAt:   expiresAt,
			FileName:    f.Filename,
			// FileType:    f.ContentType,
			OwnerID:  f.OwnerID,
			FolderID: f.FolderID,
			// FilePath:    f.Path,
			MaxFileSize: f.MaxUploadSize,
			BaseURL:     f.HostedAt,
		})
	}

	return models.UploadSessionBatch(sessions), nil
}

// GrantInput describes the file an existing session should be scoped
// to.
type GrantInput struct {
	// Path is the complete storage key, given verbatim by the Rust API.
	Path     string
	FileID   string
	Version  string
	CanWrite bool // relevant only for WOPI grants
	// Filename, OwnerID, FolderID, UserID and UserName are relevant only
	// for WOPI grants — cached into the session so CheckFileInfo (and,
	// for FolderID/OwnerID, the new-version callback) can answer without
	// another round trip to the Rust API.
	Filename string
	OwnerID  string
	FolderID string
	UserID   string
	UserName string
	// BaseURL is the storage node's public host for this file (WOPI's
	// server_host), folded into the returned WOPI URL the same way it
	// is threaded through download sessions — see DownloadFileInput. It
	// is also cached into the session's Claims.HostedAt so it can be
	// echoed back on the new-version callback.
	BaseURL string
	// IsFileVersioningEnabled is relevant only for WOPI grants. See
	// models.Claims.IsFileVersioningEnabled.
	IsFileVersioningEnabled bool
	// TTL is the session lifetime requested by the Rust API.
	TTL time.Duration
}

// DownloadFileInput describes one file within a requested (possibly
// multi-file) download session request.
type DownloadFileInput struct {
	// Path is the complete storage key, given verbatim by the Rust API.
	Path        string
	FileID      string
	FileName    string
	FolderID    string
	OwnerID     string
	FileVersion int32
	// HostedAt is the base path this file is hosted under, given
	// verbatim by the Rust API. It is folded into the returned download
	// URL so the link points at whichever storage node actually holds
	// the file, the same way it is threaded through upload sessions.
	HostedAt string
	// TTL is this file's own session lifetime, requested by the Rust
	// API.
	TTL time.Duration
}

// NewDownloadInput describes a requested batch of download sessions —
// one or more files, each independently authorized (own token, own
// TTL, own URL).
type NewDownloadInput struct {
	Files []DownloadFileInput
}

// CreateDownloadSession mints one independent token+URL per file in
// in.Files, so a batch spanning multiple storage nodes can be requested
// in a single call. Each returned URL is "<base_url><download
// path>/<file_id>?token=<token>" so it can be handed straight to a
// browser or client without any extra header — base_url is left off
// when the file didn't carry one, producing a path relative to this
// Storage API instance.
func (s *Service) CreateDownloadSession(ctx context.Context, in NewDownloadInput) (models.DownloadSessionBatch, error) {
	if len(in.Files) == 0 {
		return nil, fmt.Errorf("session: at least one file is required")
	}

	sessions := make([]models.DownloadSession, 0, len(in.Files))
	for _, f := range in.Files {
		if f.FileID == "" || f.Path == "" {
			return nil, fmt.Errorf("session: file id and path must not be empty")
		}

		tok, expiresAt, err := s.issuer.GenerateDownloadToken(ctx, models.Claims{
			Path:     f.Path,
			FileID:   f.FileID,
			FolderID: f.FolderID,
			OwnerID:  f.OwnerID,
			Version:  strconv.FormatInt(int64(f.FileVersion), 10),
			Filename: f.FileName,
		}, f.TTL)
		if err != nil {
			return nil, fmt.Errorf("session: generating download token for file_id %q: %w", f.FileID, err)
		}

		sessions = append(sessions, models.DownloadSession{
			FileName:       f.FileName,
			FileID:         f.FileID,
			FolderID:       f.FolderID,
			OwnerID:        f.OwnerID,
			FileVersion:    f.FileVersion,
			URL:            fmt.Sprintf("%s%s%s?token=%s", strings.TrimRight(f.HostedAt, "/"), s.urls.Download, f.FileID, url.QueryEscape(tok)),
			AccessToken:    tok,
			ExpiresAt:      expiresAt,
			AccessTokenTTL: expiresAt.UnixMilli(),
		})
	}

	return models.DownloadSessionBatch(sessions), nil
}

// CreateStreamSession mints a token+URL for range-based media streaming
// of a single file.
func (s *Service) CreateStreamSession(ctx context.Context, in GrantInput) (models.DownloadSession, error) {
	tok, expiresAt, err := s.issuer.GenerateStreamToken(ctx, models.Claims{
		Path:    in.Path,
		FileID:  in.FileID,
		Version: in.Version,
	}, in.TTL)
	if err != nil {
		return models.DownloadSession{}, fmt.Errorf("session: generating stream token: %w", err)
	}
	return models.DownloadSession{
		URL:            fmt.Sprintf("%s%s", s.urls.Stream, in.FileID),
		AccessToken:    tok,
		ExpiresAt:      expiresAt,
		AccessTokenTTL: expiresAt.UnixMilli(),
	}, nil
}

// CreateWOPISession mints a WOPI access_token+URL for Collabora Online to
// edit or view a single file.
func (s *Service) CreateWOPISession(ctx context.Context, in GrantInput) (models.DownloadSession, error) {
	tok, expiresAt, err := s.issuer.GenerateWOPIToken(ctx, models.Claims{
		Path:                    in.Path,
		FileID:                  in.FileID,
		FolderID:                in.FolderID,
		Version:                 in.Version,
		CanWrite:                in.CanWrite,
		Filename:                in.Filename,
		OwnerID:                 in.OwnerID,
		UserID:                  in.UserID,
		UserName:                in.UserName,
		HostedAt:                in.BaseURL,
		IsFileVersioningEnabled: in.IsFileVersioningEnabled,
	}, in.TTL)
	if err != nil {
		return models.DownloadSession{}, fmt.Errorf("session: generating wopi token: %w", err)
	}
	return models.DownloadSession{
		FileName:       in.Filename,
		FileID:         in.FileID,
		OwnerID:        in.OwnerID,
		URL:            fmt.Sprintf("%s%s%s", strings.TrimRight(in.BaseURL, "/"), s.urls.WOPI, in.FileID),
		AccessToken:    tok,
		ExpiresAt:      expiresAt,
		AccessTokenTTL: expiresAt.UnixMilli(),
	}, nil
}
