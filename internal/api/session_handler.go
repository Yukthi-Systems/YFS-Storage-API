package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/session"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

type createUploadFileRequest struct {
	// FileLocation is "<base_url>;<path>", given verbatim by the Rust
	// API. Only the path half is meaningful to the Storage API — the
	// Storage API always responds with its own base_url
	// (session.UploadStorageBaseURL) rather than the one embedded here.
	FileLocation string `json:"file_location"`
	FileName     string `json:"file_name"`
	// FolderID and OwnerID identify where this file lives in the Rust
	// metadata service's hierarchy. The Storage API attaches no meaning
	// to them beyond caching them in the session (Redis) alongside the
	// token.
	FolderID string `json:"folder_id"`
	// FileID is the permanent file ID, chosen by the Rust API and
	// reused across every version of the same file.
	FileID      string `json:"file_id"`
	OwnerID     string `json:"owner_id"`
	FileVersion int32  `json:"file_version"`
	// ExpectedFileSize is the size, in bytes, the Rust API expects this
	// upload to be. Enforced as the ceiling on the tus upload.
	ExpectedFileSize uint64 `json:"expected_file_size"`
	// MaxOperationTime is this file's own session lifetime in seconds,
	// decided by the Rust API. If omitted or zero, the Storage API's
	// configured default applies. Each file in the batch gets its own
	// token and TTL — there is no batch-wide default here.
	MaxOperationTime uint16 `json:"max_operation_time"`
}

// createUploadSessionRequest is a bare JSON array — each element gets
// its own independent token, since tus uploads one file per resource.
type createUploadSessionRequest []createUploadFileRequest

func (h *Handlers) handleCreateUploadSession(w http.ResponseWriter, r *http.Request) {
	var req createUploadSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(req) == 0 {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "request body must contain at least one file")
		return
	}
	if h.MaxUploadBatchSize > 0 && len(req) > h.MaxUploadBatchSize {
		utils.WriteError(w, http.StatusBadRequest, "batch_too_large", fmt.Sprintf("request body must not contain more than %d entries", h.MaxUploadBatchSize))
		return
	}

	files := make([]session.UploadFileInput, 0, len(req))
	for _, f := range req {
		if f.FileLocation == "" || f.FileID == "" || f.FolderID == "" || f.OwnerID == "" {
			utils.WriteError(w, http.StatusBadRequest, "missing_fields", "file_location, file_id, folder_id and owner_id are required for every file")
			return
		}
		version := f.FileVersion
		if version <= 0 {
			version = 1
		}
		baseURL, path := utils.SplitFileLocation(f.FileLocation)
		files = append(files, session.UploadFileInput{
			Path:        path,
			FileID:      f.FileID,
			FolderID:    f.FolderID,
			OwnerID:     f.OwnerID,
			FileVersion: version,
			Filename:    f.FileName,
			// ContentType:   mime.TypeByExtension(filepath.Ext(f.FileName)),
			MaxUploadSize: int64(f.ExpectedFileSize),
			TTL:           time.Duration(f.MaxOperationTime) * time.Second,
			BaseURL:       baseURL,
		})
	}

	result, err := h.Session.CreateUploadSession(r.Context(), session.NewUploadInput{
		Files: files,
	})
	if err != nil {
		if errors.Is(err, session.ErrStorageFull) {
			h.Logger.Warn("upload session rejected: storage capacity threshold reached", "file_count", len(req))
			w.Header().Set("Retry-After", strconv.Itoa(int(h.StorageBackoff.Seconds())))
			utils.WriteError(w, http.StatusInsufficientStorage, "storage_full", "storage capacity threshold reached, retry later")
			return
		}
		h.Logger.Error("create upload session failed", "error", err)
		utils.WriteError(w, http.StatusInternalServerError, "session_error", "failed to create upload session")
		return
	}

	middleware.AddLogFields(r.Context(), slog.Int("file_count", len(result)))
	utils.WriteJSON(w, http.StatusCreated, result)
}

type createGrantSessionRequest struct {
	Path     string `json:"path"`
	FileID   string `json:"file_id"`
	Version  string `json:"version"`
	CanWrite bool   `json:"can_write"`
	// TTLSeconds is the session lifetime, decided by the Rust API. If
	// omitted or zero, the Storage API's configured default applies.
	TTLSeconds int64 `json:"ttl_seconds"`
}

func decodeGrantRequest(w http.ResponseWriter, r *http.Request) (createGrantSessionRequest, bool) {
	var req createGrantSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return req, false
	}
	if req.Path == "" || req.FileID == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "path and file_id are required")
		return req, false
	}
	return req, true
}

type createDownloadFileRequest struct {
	// FileLocation is "<base_url>;<path>", given verbatim by the Rust
	// API — same convention as createUploadFileRequest. The base_url
	// half is folded into this file's returned download URL so it
	// points at whichever storage node actually holds the file.
	FileLocation string `json:"file_location"`
	FileName     string `json:"file_name"`
	FileID       string `json:"file_id"`
	// FolderID and OwnerID identify where this file lives in the Rust
	// metadata service's hierarchy. The Storage API attaches no meaning
	// to them beyond echoing them back alongside the token.
	FolderID    string `json:"folder_id"`
	OwnerID     string `json:"owner_id"`
	FileVersion int32  `json:"file_version"`
	// MaxOperationTime is this file's own session lifetime in seconds.
	// If omitted or zero, the Storage API's configured default applies.
	MaxOperationTime uint16 `json:"max_operation_time"`
}

// createDownloadSessionRequest is a bare JSON array — each element gets
// its own independent token and URL, since a batch's files may live
// behind different storage nodes.
type createDownloadSessionRequest []createDownloadFileRequest

func (h *Handlers) handleCreateDownloadSession(w http.ResponseWriter, r *http.Request) {
	var req createDownloadSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(req) == 0 {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "request body must contain at least one file")
		return
	}

	files := make([]session.DownloadFileInput, 0, len(req))
	for _, f := range req {
		if f.FileLocation == "" || f.FileID == "" || f.FolderID == "" || f.OwnerID == "" {
			utils.WriteError(w, http.StatusBadRequest, "missing_fields", "file_location, file_id, folder_id and owner_id are required for every file")
			return
		}
		baseURL, path := utils.SplitFileLocation(f.FileLocation)
		files = append(files, session.DownloadFileInput{
			Path:        path,
			FileID:      f.FileID,
			FileName:    f.FileName,
			FolderID:    f.FolderID,
			OwnerID:     f.OwnerID,
			FileVersion: f.FileVersion,
			BaseURL:     baseURL,
			TTL:         time.Duration(f.MaxOperationTime) * time.Second,
		})
	}

	result, err := h.Session.CreateDownloadSession(r.Context(), session.NewDownloadInput{Files: files})
	if err != nil {
		h.Logger.Error("create download session failed", "error", err)
		utils.WriteError(w, http.StatusInternalServerError, "session_error", "failed to create download session")
		return
	}

	middleware.AddLogFields(r.Context(), slog.Int("file_count", len(result)))
	utils.WriteJSON(w, http.StatusCreated, result)
}

func (h *Handlers) handleCreateStreamSession(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeGrantRequest(w, r)
	if !ok {
		return
	}
	result, err := h.Session.CreateStreamSession(r.Context(), session.GrantInput{
		Path: req.Path, FileID: req.FileID, Version: req.Version,
		TTL: time.Duration(req.TTLSeconds) * time.Second,
	})
	if err != nil {
		h.Logger.Error("create stream session failed", "error", err)
		utils.WriteError(w, http.StatusInternalServerError, "session_error", "failed to create stream session")
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", req.Path), slog.String("file_id", req.FileID))
	utils.WriteJSON(w, http.StatusCreated, result)
}

// createWOPISessionRequest mirrors the Rust API's FileStorageWopiAPI
// struct field-for-field.
type createWOPISessionRequest struct {
	FileName string `json:"file_name"`
	// FileLocation is the complete storage key, given verbatim by the
	// Rust API. Unlike upload/download, it carries no "<base_url>;"
	// prefix to split off — that half travels separately as ServerHost.
	FileLocation string `json:"file_location"`
	// ServerHost is the storage node's public host for this file (e.g.
	// "https://store-1.files.test.yukthi.net"), folded into the
	// returned WOPI URL so Collabora calls the node that actually holds
	// the file — the WOPI equivalent of DownloadFileInput.BaseURL.
	ServerHost string `json:"server_host"`
	FileID     string `json:"file_id"`
	OwnerID    string `json:"owner_id"`
	// UserID and UserName identify the person the session is granted
	// to, cached so CheckFileInfo can answer Collabora's UserId/
	// UserFriendlyName without another round trip to the Rust API.
	UserID            string `json:"user_id"`
	UserName          string `json:"user_name"`
	LatestFileVersion int32  `json:"latest_file_version"`
	CanWrite          bool   `json:"can_write"`
}

func (h *Handlers) handleCreateWOPISession(w http.ResponseWriter, r *http.Request) {
	var req createWOPISessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.FileLocation == "" || req.FileID == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "file_location and file_id are required")
		return
	}
	result, err := h.Session.CreateWOPISession(r.Context(), session.GrantInput{
		Path:     req.FileLocation,
		FileID:   req.FileID,
		Version:  strconv.FormatInt(int64(req.LatestFileVersion), 10),
		CanWrite: req.CanWrite,
		Filename: req.FileName,
		OwnerID:  req.OwnerID,
		UserID:   req.UserID,
		UserName: req.UserName,
		BaseURL:  req.ServerHost,
		TTL:      h.WOPISessionTTL,
	})
	if err != nil {
		h.Logger.Error("create wopi session failed", "error", err)
		utils.WriteError(w, http.StatusInternalServerError, "session_error", "failed to create wopi session")
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", req.FileLocation), slog.String("file_id", req.FileID))
	utils.WriteJSON(w, http.StatusCreated, result)
}

type revokeSessionRequest struct {
	Token string `json:"token"`
}

// handleRevokeSession clears a previously issued session out of Redis,
// so its token can no longer authorize any request (upload, download,
// stream, or WOPI). Internal-only, called by the Rust API — e.g. when a
// user logs out or a Collabora editing session ends.
func (h *Handlers) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	var req revokeSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Token == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "token is required")
		return
	}

	if err := h.Issuer.RevokeToken(r.Context(), req.Token); err != nil {
		if errors.Is(err, token.ErrInvalidToken) {
			utils.WriteError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		h.Logger.Error("revoke session failed", "error", err)
		utils.WriteError(w, http.StatusInternalServerError, "session_error", "failed to revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
