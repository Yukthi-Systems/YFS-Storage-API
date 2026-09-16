package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/wopi"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

func (h *Handlers) authorizeWOPI(w http.ResponseWriter, r *http.Request, fileID string) (*models.Claims, bool) {
	claims, err := authorizeFileGrant(h.Issuer, r, models.ActionWOPI, fileID)
	if err != nil {
		utils.WriteError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return nil, false
	}
	middleware.AddLogFields(r.Context(), slog.String("path", claims.Path), slog.String("file_id", fileID))
	return claims, true
}

// handleWOPICheckFileInfo implements WOPI's CheckFileInfo.
// GET /wopi/files/{fileID}?access_token=...
func (h *Handlers) handleWOPICheckFileInfo(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("fileID")
	claims, ok := h.authorizeWOPI(w, r, fileID)
	if !ok {
		return
	}

	info, err := h.WOPI.CheckFileInfo(r.Context(), claims.Path, fileID, claims.Filename, claims.CanWrite, claims.OwnerID, claims.UserID, claims.UserName)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	utils.WriteJSON(w, http.StatusOK, info)
}

// handleWOPIGetFile implements WOPI's GetFile.
// GET /wopi/files/{fileID}/contents?access_token=...
func (h *Handlers) handleWOPIGetFile(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("fileID")
	claims, ok := h.authorizeWOPI(w, r, fileID)
	if !ok {
		return
	}

	content, meta, err := h.WOPI.GetFile(r.Context(), claims.Path)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	defer content.Close()

	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	if _, err := io.Copy(w, content); err != nil {
		slog.ErrorContext(r.Context(), "wopi: streaming file contents", "file_id", fileID, "error", err)
	}
}

// handleWOPIPutFile implements WOPI's PutFile.
// POST /wopi/files/{fileID}/contents?access_token=...
func (h *Handlers) handleWOPIPutFile(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("fileID")
	claims, ok := h.authorizeWOPI(w, r, fileID)
	if !ok {
		return
	}
	if !claims.CanWrite {
		utils.WriteError(w, http.StatusForbidden, "forbidden", "token is not authorized to write this file")
		return
	}

	lockID := r.Header.Get("X-WOPI-Lock")
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	err := h.WOPI.PutFile(r.Context(), claims.Path, fileID, lockID, r.Body, r.ContentLength, contentType)
	if writeWOPILockError(w, err) {
		return
	}
	if err != nil {
		writeStreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleWOPIPost dispatches the WOPI lock family of operations, which
// all arrive as POST with an X-WOPI-Override header identifying the
// specific action: LOCK, UNLOCK, or REFRESH_LOCK.
// POST /wopi/files/{fileID}?access_token=...
func (h *Handlers) handleWOPIPost(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("fileID")
	claims, ok := h.authorizeWOPI(w, r, fileID)
	if !ok {
		return
	}

	lockID := r.Header.Get("X-WOPI-Lock")
	override := r.Header.Get("X-WOPI-Override")

	var err error
	switch override {
	case "LOCK":
		if !claims.CanWrite {
			utils.WriteError(w, http.StatusForbidden, "forbidden", "token is not authorized to lock this file")
			return
		}
		err = h.WOPI.Lock(r.Context(), fileID, lockID)
	case "UNLOCK":
		err = h.WOPI.Unlock(r.Context(), fileID, lockID)
	case "REFRESH_LOCK":
		err = h.WOPI.RefreshLock(r.Context(), fileID, lockID)
	default:
		utils.WriteError(w, http.StatusNotImplemented, "unsupported_override", "unsupported X-WOPI-Override: "+override)
		return
	}

	if writeWOPILockError(w, err) {
		return
	}
	if err != nil {
		writeStreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// writeWOPILockError writes the WOPI-mandated 409 Conflict response
// (with the current lock echoed via X-WOPI-Lock) when err is a lock
// conflict, and reports whether it did so.
func writeWOPILockError(w http.ResponseWriter, err error) bool {
	var conflict *wopi.LockConflictError
	if !errors.As(err, &conflict) {
		return false
	}
	w.Header().Set("X-WOPI-Lock", conflict.ConflictLockID)
	w.WriteHeader(http.StatusConflict)
	return true
}
