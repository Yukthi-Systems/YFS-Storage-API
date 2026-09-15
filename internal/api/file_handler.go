package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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

func (h *Handlers) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeMoveRequest(w, r)
	if !ok {
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", req.Path), slog.String("trash_path", req.TrashPath))

	if err := h.File.Delete(r.Context(), req.Path, req.TrashPath); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
