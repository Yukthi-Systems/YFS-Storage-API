package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/download"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/file"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/session"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/wopi"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
)

// Handlers aggregates every service the HTTP layer dispatches to.
type Handlers struct {
	Issuer   *token.Issuer
	Session  *session.Service
	File     *file.Service
	Download *download.Service
	// Stream   *stream.Service
	// Media    *media.Service
	WOPI *wopi.Service
	Tus  http.Handler // mounted verbatim at the configured tus base path

	// RustAPIToken is the shared secret only the Rust API knows. It
	// gates the internal-only routes (session issuance, direct file
	// CRUD) via the X-API-Token header — see internal/middleware.RequireAPIToken.
	RustAPIToken string

	// StorageBackoff is the Retry-After hint sent alongside a 507
	// Insufficient Storage response from /sessions/upload.
	StorageBackoff time.Duration
	// MaxUploadBatchSize caps how many files a single /sessions/upload
	// request may authorize at once.
	MaxUploadBatchSize int

	// DownloadCorsAllowedOrigins is a comma-separated list of exact
	// origins allowed to make cross-origin browser requests (fetch/XHR)
	// to /download/{fileID}. Empty allows any origin. See
	// middleware.CORS.
	DownloadCorsAllowedOrigins string

	Logger *slog.Logger
}

// Register wires every route onto mux.
func (h *Handlers) Register(mux *http.ServeMux, tusBasePath string) {
	internalOnly := middleware.RequireAPIToken(h.RustAPIToken)

	// Health/readiness.
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)

	// Session issuance (called by the Rust API on behalf of an
	// already-authorized user). Locked to the Rust API via X-API-Token.
	mux.Handle("POST /sessions/upload", internalOnly(http.HandlerFunc(h.handleCreateUploadSession)))
	mux.Handle("POST /sessions/download", internalOnly(http.HandlerFunc(h.handleCreateDownloadSession)))
	mux.Handle("POST /sessions/wopi", internalOnly(http.HandlerFunc(h.handleCreateWOPISession)))
	mux.Handle("POST /sessions/revoke", internalOnly(http.HandlerFunc(h.handleRevokeSession)))

	// Resumable (tus) uploads. h.Tus already strips tusBasePath itself
	// (see internal/tus.NewHandler), since tusd's own internal routing
	// expects paths relative to its mount point. Authorized per-upload
	// via the bearer session token minted by /sessions/upload, not
	// X-API-Token.
	mux.Handle(tusBasePath, h.Tus)

	// Direct file CRUD (trusted, called by the Rust API only). Every
	// request body carries the complete storage path(s) involved — there
	// is no org/user/file-ID route segment for the Storage API to
	// interpret. Locked to the Rust API via X-API-Token.
	mux.Handle("POST /files/stat", internalOnly(http.HandlerFunc(h.handleFileStat)))
	mux.Handle("POST /files/delete", internalOnly(http.HandlerFunc(h.handleFileDelete)))
	mux.Handle("POST /files/restore", internalOnly(http.HandlerFunc(h.handleFileRestore)))
	mux.Handle("POST /files/purge", internalOnly(http.HandlerFunc(h.handleFilePurge)))
	mux.Handle("POST /files/copy", internalOnly(http.HandlerFunc(h.handleFileCopy)))
	mux.Handle("POST /files/purge-prefix", internalOnly(http.HandlerFunc(h.handleFilePurgePrefix)))

	// End-user download/stream/media, authorized by the token minted
	// during session creation. CORS'd because, unlike every other route
	// above, this one is called directly from the React app's browser JS
	// (fetch/XHR) rather than server-to-server or via a plain navigation.
	downloadCors := middleware.CORS(h.DownloadCorsAllowedOrigins)
	mux.Handle("GET /download/{fileID}", downloadCors(http.HandlerFunc(h.handleDownload)))
	mux.Handle("OPTIONS /download/{fileID}", downloadCors(http.HandlerFunc(h.handleDownload)))

	// WOPI host endpoints for Collabora Online.
	mux.HandleFunc("GET /wopi/files/{fileID}", h.handleWOPICheckFileInfo)
	mux.HandleFunc("GET /wopi/files/{fileID}/contents", h.handleWOPIGetFile)
	mux.HandleFunc("POST /wopi/files/{fileID}/contents", h.handleWOPIPutFile)
	mux.HandleFunc("POST /wopi/files/{fileID}", h.handleWOPIPost)
}

func (h *Handlers) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handlers) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
