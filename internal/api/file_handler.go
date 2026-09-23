package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// These endpoints are internal-only: called by the Rust API, which has
// already performed permission checks, never directly by end users. Every
// path they take is a complete storage key supplied verbatim by the Rust
// API — the Storage API attaches no meaning to it.

type pathRequest struct {
	Path string `json:"path"`
}

func decodePathRequest(w http.ResponseWriter, r *http.Request) (pathRequest, bool) {
	var req pathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return req, false
	}
	if req.Path == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "path is required")
		return req, false
	}
	return req, true
}

func (h *Handlers) handleFileStat(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePathRequest(w, r)
	if !ok {
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", req.Path))

	info, err := h.File.Stat(r.Context(), req.Path)
	if err != nil {
		writeFileError(w, err)
		return
	}
	utils.WriteJSON(w, http.StatusOK, info)
}

// handleFileDelete permanently deletes a batch of storage paths. The Rust
// API has already applied all trash/grace-period logic on its side (in
// its own metadata store) and hands over the final list of paths to
// remove — this endpoint does not move anything to trash itself. A path
// ending in "/" (e.g. an org, user, or folder root) is deleted
// recursively; anything else is deleted as a single file. The batch is
// durably recorded before responding, so a 202 here means the deletions
// will happen even if this process crashes or restarts before getting to
// them; it does not mean the files are gone yet. See
// internal/service/purge for the actual delete/retry/resume logic.
func (h *Handlers) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var paths []string
	if err := json.NewDecoder(r.Body).Decode(&paths); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(paths) == 0 {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "at least one path is required")
		return
	}
	if h.MaxDeleteBatchSize > 0 && len(paths) > h.MaxDeleteBatchSize {
		utils.WriteError(w, http.StatusBadRequest, "batch_too_large", fmt.Sprintf("at most %d paths per request", h.MaxDeleteBatchSize))
		return
	}
	for _, p := range paths {
		if p == "" {
			utils.WriteError(w, http.StatusBadRequest, "missing_fields", "paths must not be empty")
			return
		}
	}
	middleware.AddLogFields(r.Context(), slog.Int("count", len(paths)))

	if err := h.Purge.Enqueue(r.Context(), paths); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, "enqueue_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type moveRequest struct {
	Path      string `json:"path"`
	TrashPath string `json:"trash_path"`
}

func decodeMoveRequest(w http.ResponseWriter, r *http.Request) (moveRequest, bool) {
	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return req, false
	}
	if req.Path == "" || req.TrashPath == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "path and trash_path are required")
		return req, false
	}
	return req, true
}

func (h *Handlers) handleFileRestore(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeMoveRequest(w, r)
	if !ok {
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", req.Path), slog.String("trash_path", req.TrashPath))

	if err := h.File.Restore(r.Context(), req.TrashPath, req.Path); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type purgeRequest struct {
	TrashPath string `json:"trash_path"`
}

func (h *Handlers) handleFilePurge(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.TrashPath == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "trash_path is required")
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("trash_path", req.TrashPath))

	if err := h.File.Purge(r.Context(), req.TrashPath); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type copyFileRequest struct {
	SrcPath string `json:"src_path"`
	DstPath string `json:"dst_path"`
}

func (h *Handlers) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	var req copyFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.SrcPath == "" || req.DstPath == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "src_path and dst_path are required")
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("src_path", req.SrcPath), slog.String("dst_path", req.DstPath))

	if err := h.File.Copy(r.Context(), req.SrcPath, req.DstPath); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type purgePrefixRequest struct {
	Prefix string `json:"prefix"`
}

// handleFilePurgePrefix permanently deletes everything stored under a
// caller-supplied prefix — e.g. an entire org's or user's folder, as
// decided by the Rust metadata service — in one call. This is
// unrecoverable and internal-only (X-API-Token).
func (h *Handlers) handleFilePurgePrefix(w http.ResponseWriter, r *http.Request) {
	var req purgePrefixRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Prefix == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "prefix is required")
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("prefix", req.Prefix))

	if err := h.File.PurgePrefix(r.Context(), req.Prefix); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeFileError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		utils.WriteError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	utils.WriteError(w, http.StatusInternalServerError, "file_error", err.Error())
}

// handleInternalFileDownload streams a file's raw bytes to another
// trusted internal service (e.g. the Archive API building a zip), given
// its complete storage path in the file_location query parameter. Unlike
// /download/{fileID}, there is no session token or filename involved —
// the caller authenticates with X-API-Token and gets bare octet-stream
// content.
func (h *Handlers) handleInternalFileDownload(w http.ResponseWriter, r *http.Request) {
	location := r.URL.Query().Get("file_location")
	if location == "" {
		utils.WriteError(w, http.StatusBadRequest, "missing_fields", "file_location is required")
		return
	}
	key, err := cleanFileLocation(location)
	if err != nil {
		utils.WriteError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", key))

	content, meta, err := h.Download.Open(r.Context(), key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer content.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent sets Content-Length from the seeker and, for a local
	// *os.File, copies via sendfile rather than buffering in memory.
	http.ServeContent(w, r, "", meta.ModTime, content)
}

// cleanFileLocation validates a caller-supplied storage path before it
// reaches the storage driver. Paths come from the Rust API's database and
// are trusted, but a ".." segment is never legitimate and — with
// LOCAL_BASE_PATH="/" — the driver's own base-path check cannot catch it,
// so it is rejected here outright rather than cleaned away.
func cleanFileLocation(location string) (string, error) {
	if strings.ContainsRune(location, 0) {
		return "", errors.New("file_location contains a NUL byte")
	}
	for _, seg := range strings.Split(filepath.ToSlash(location), "/") {
		if seg == ".." {
			return "", errors.New("file_location must not contain '..'")
		}
	}
	if strings.HasSuffix(location, "/") {
		return "", errors.New("file_location must name a file, not a directory")
	}
	return filepath.Clean(location), nil
}
