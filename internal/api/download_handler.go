package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/media"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

func (h *Handlers) handleDownload(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("fileID")

	claims, err := authorizeFileGrant(h.Issuer, r, models.ActionDownload, fileID)
	if err != nil {
		utils.WriteError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	middleware.AddLogFields(r.Context(), slog.String("path", claims.Path), slog.String("file_id", fileID))

	content, meta, err := h.Download.Open(r.Context(), claims.Path)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	defer content.Close()

	headers := media.BuildHeaders(meta, claims.Filename, false)
	headers.Apply(w.Header().Set)

	http.ServeContent(w, r, fileID, meta.ModTime, content)
}

func writeStreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		utils.WriteError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	utils.WriteError(w, http.StatusInternalServerError, "storage_error", err.Error())
}
