// Package models holds data structures shared across the Storage API's
// internal packages and, potentially, external callers (e.g. the Rust API
// client library) that only need wire-format definitions.
package models

import (
	"encoding/json"
	"time"
)

// TokenAction identifies what a session issued by the Storage API
// authorizes its bearer to do.
type TokenAction string

const (
	ActionUpload   TokenAction = "upload"
	ActionDownload TokenAction = "download"
	ActionStream   TokenAction = "stream"
	ActionWOPI     TokenAction = "wopi"
)

// Claims are the session data cached in Redis under every opaque
// Storage API token. The Rust API never sees or sets these directly; it
// only ever receives opaque tokens back from the Storage API and hands
// them to clients.
//
// Path is the complete storage key, given verbatim by the Rust API and
// used as-is against the storage.Storage backend. The Storage API attaches
// no meaning to it beyond that — ownership, hierarchy, and identity
// (org/user) are entirely the Rust metadata service's concern.
type Claims struct {
	Action   TokenAction `json:"action"`
	Path     string      `json:"path"`
	FileID   string      `json:"file_id,omitempty"`
	FolderID string      `json:"folder_id,omitempty"`
	OwnerID  string      `json:"owner_id,omitempty"`
	// BaseURL is the base_url half of the file_location the Rust API
	// sent at session-creation time, cached here purely so it can be
	// echoed back — complete with Path — in the upload-result callback.
	BaseURL       string `json:"base_url,omitempty"`
	UploadID      string `json:"upload_id,omitempty"`
	Version       string `json:"version,omitempty"`
	ContentType   string `json:"content_type,omitempty"`
	MaxUploadSize int64  `json:"max_upload_size,omitempty"`
	Filename      string `json:"filename,omitempty"`
	CanWrite      bool   `json:"can_write,omitempty"`
	// BatchID is the same across every file created by one
	// /sessions/upload request, even though each file gets its own
	// independent token/TTL. Unused today; it exists so a future
	// "revoke/inspect this whole batch" endpoint has something to key
	// off without needing every individual token.
	BatchID string `json:"batch_id,omitempty"`
}

// UploadSession describes a newly created upload slot for exactly one
// file, returned to the Rust API (and then relayed to the UI) so a
// client can perform a direct upload. A batch /sessions/upload request
// returns one independent UploadSession per file (its own token and
// TTL) rather than a single shared session, since tus itself only ever
// uploads one file per resource.
type UploadSession struct {
	FileName string `json:"file_name"`
	// FilePath    string    `json:"file_location"`
	MaxFileSize int64     `json:"expected_file_size"`
	Token       string    `json:"token"`
	FileID      string    `json:"file_id"`
	FileVersion int32     `json:"file_version"`
	FolderID    string    `json:"folder_id"`
	OwnerID     string    `json:"owner_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	BaseURL     string    `json:"base_url"`
}

// UploadSessionBatch is the response to a batch upload-session request:
// one independent UploadSession per requested file, encoded directly as
// a JSON array (no wrapping object).
type UploadSessionBatch []UploadSession

// DownloadSession describes an issued download/stream/WOPI grant.
type DownloadSession struct {
	// FileID, FolderID, OwnerID, FileName and FileVersion are only
	// populated for batch download sessions, where they let the caller
	// match each session back to the file it requested without a
	// separate lookup.
	FileName    string    `json:"file_name,omitempty"`
	FileID      string    `json:"file_id,omitempty"`
	FolderID    string    `json:"folder_id,omitempty"`
	OwnerID     string    `json:"owner_id,omitempty"`
	FileVersion int32     `json:"file_version,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	URL         string    `json:"url"`
	Token       string    `json:"token"`
}

// DownloadSessionBatch is the response to a batch download-session
// request: one independent DownloadSession per requested file, encoded
// directly as a JSON array (no wrapping object) — mirroring
// UploadSessionBatch.
type DownloadSessionBatch []DownloadSession

// FileInfo is the storage-level metadata the Storage API knows about a
// file. It intentionally excludes anything owned by the Rust metadata
// service (filenames, folder hierarchy, ownership, permissions, etc.)
// beyond what is needed to serve bytes correctly.
type FileInfo struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"` // Bytes
	ContentType string    `json:"content_type"`
	Checksum    string    `json:"checksum,omitempty"`
	ETag        string    `json:"etag"`
	ModifiedAt  time.Time `json:"modified_at"`
}

// CommitResult is returned once a completed tus upload has been verified
// and moved from uploads/ into files/.
type CommitResult struct {
	FileID      string `json:"file_id"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Checksum    string `json:"checksum"`
}

// UploadCallback is the body POSTed to the Rust API's
// /internal/callback/upload/{is_success} endpoint once a tus upload has
// been processed — successfully or not — so the Rust API can be informed
// out-of-band. It mirrors the Rust API's own FileOpsCallBack struct
// field-for-field; is_success travels in the URL path, not the body. On
// failure FileHash is a random placeholder (there is no real checksum to
// report) and the underlying error is only ever logged, never sent.
type UploadCallback struct {
	FolderID     string          `json:"folder_id"`
	FileID       string          `json:"file_id"`
	OwnerID      string          `json:"owner_id"`
	FileVersion  int32           `json:"file_version"`
	FileLocation string          `json:"file_location"`
	FileSize     int64           `json:"file_size"`
	Metadata     json.RawMessage `json:"metadata"`
	FileHash     string          `json:"file_hash"`
}
