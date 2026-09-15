// Package tus wires tusd (the tus resumable-upload protocol implementation)
// into the Storage API. tusd owns the resumable-upload wire protocol only;
// it stages incoming chunks on local disk regardless of the configured
// storage.Storage backend (even S3-backed uploads need a durable local
// buffer to receive out-of-order/resumed chunks). Once a client marks an
// upload complete, this package hands the finished bytes to
// internal/service/upload.Manager, which is the only thing that talks to
// the real storage.Storage backend and performs the uploads/ -> files/
// promotion.
package tus

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/upload"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
)

// metaPathKey/metaContentTypeKey/metaFileIDKey are the tus upload
// metadata keys used to carry claims from upload creation through to
// completion, since tusd's FileInfo has no notion of a destination
// storage path (or file identity) on its own.
const (
	metaPathKey        = "path"
	metaContentTypeKey = "content_type"
	metaFileIDKey      = "file_id"
	metaFolderIDKey    = "folder_id"
	metaOwnerIDKey     = "owner_id"
	metaVersionKey     = "version"
	metaBaseURLKey     = "base_url"
)

// filenameKeys/filetypeKeys are the client-supplied tus Upload-Metadata
// keys that might carry the original file's name and MIME type,
// reported back to the Rust API on the upload-result callback's
// Metadata field. The tus protocol doesn't mandate key names, so both
// the tus-js-client/Uppy convention (filename/filetype) and tusd's own
// doc-example convention (name/type) are accepted; the first key
// present wins.
var (
	filenameKeys = []string{"filename", "name"}
	filetypeKeys = []string{"filetype", "type"}
)

// firstMetaValue returns the value of the first of keys present (and
// non-empty) in meta, or "" if none are.
func firstMetaValue(meta tusd.MetaData, keys []string) string {
	for _, k := range keys {
		if v := meta[k]; v != "" {
			return v
		}
	}
	return ""
}

// Config configures the tus integration.
type Config struct {
	// StagingDir is where tusd persists in-progress upload chunks and
	// their .info sidecar files. This is purely an implementation detail
	// of the resumable-upload protocol, independent of the configured
	// storage.Storage backend.
	StagingDir string
	// BasePath is the URL path tusd is mounted at, e.g. "/upload/tus/".
	BasePath string
	// MaxSize is the hard ceiling on any single upload, in bytes.
	MaxSize int64
	// CorsAllowedOrigins is a comma-separated list of exact origins
	// allowed to make cross-origin requests to this endpoint. Empty
	// allows any origin (tusd's own default).
	CorsAllowedOrigins string

	Issuer  *token.Issuer
	Manager *upload.Manager
	Logger  *slog.Logger
}

// NewHandler builds the http.Handler serving the tus protocol at
// cfg.BasePath, enforcing Redis-backed upload sessions and committing finished
// uploads via cfg.Manager.
func NewHandler(cfg Config) (http.Handler, error) {
	if err := os.MkdirAll(cfg.StagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("tus: creating staging dir: %w", err)
	}

	store := filestore.New(cfg.StagingDir)
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	composer.UseLocker(memorylocker.New())

	w := &wiring{issuer: cfg.Issuer, manager: cfg.Manager, logger: cfg.Logger, composer: composer, basePath: strings.TrimSuffix(cfg.BasePath, "/")}

	tusdHandler, err := tusd.NewHandler(tusd.Config{
		BasePath:                  cfg.BasePath,
		StoreComposer:             composer,
		MaxSize:                   cfg.MaxSize,
		Cors:                      corsConfig(cfg.CorsAllowedOrigins),
		PreUploadCreateCallback:   w.preCreate,
		PreFinishResponseCallback: w.preFinish,
		// correct URL https need to append
		RespectForwardedHeaders: true,
	})
	if err != nil {
		return nil, fmt.Errorf("tus: constructing handler: %w", err)
	}

	stripped := http.StripPrefix(strings.TrimSuffix(cfg.BasePath, "/"), tusdHandler)
	return w.requireUploadToken(stripped), nil
}

// corsConfig builds tusd's CORS config, restricting Access-Control-Allow-Origin
// to the given comma-separated exact origins. An empty allowedOrigins, or
// "*", falls back to tusd's own default, which allows any origin.
func corsConfig(allowedOrigins string) *tusd.CorsConfig {
	cfg := tusd.DefaultCorsConfig
	allowedOrigins = strings.TrimSpace(allowedOrigins)
	if allowedOrigins == "" || allowedOrigins == "*" {
		return &cfg
	}

	origins := strings.Split(allowedOrigins, ",")
	patterns := make([]string, 0, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		patterns = append(patterns, "^"+regexp.QuoteMeta(origin)+"$")
	}
	if len(patterns) > 0 {
		cfg.AllowOrigin = regexp.MustCompile(strings.Join(patterns, "|"))
	}
	return &cfg
}

type wiring struct {
	issuer   *token.Issuer
	manager  *upload.Manager
	logger   *slog.Logger
	composer *tusd.StoreComposer
	basePath string
}

// bearerToken extracts the session token from the Authorization header of
// the originating HTTP request embedded in the hook event.
func bearerToken(h http.Header) string {
	auth := h.Get("Authorization")
	const prefix = "Bearer "
	if after, ok := strings.CutPrefix(auth, prefix); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// requireUploadToken wraps next so that every request must carry a valid,
// live upload-scoped session. tusd itself only invokes an auth hook for
// upload creation (POST, via preCreate below) - HEAD (offset check), PATCH
// (chunk write) and DELETE (termination) reach tusd's handler with no
// authorization check of their own, and OPTIONS is served entirely inside
// tusd before any hook runs. Without this wrapper, anyone who learns or
// guesses an upload_id could read its metadata or write bytes into it with
// no token at all.
//
// POST is exempt here because preCreate already authenticates it and,
// unlike the other methods, is also responsible for pinning the new
// upload's ID from the token's claims rather than an ID in the URL.
// OPTIONS is exempt because it's a CORS preflight: browsers send it
// without an Authorization header, and tusd's own handler answers it
// with the appropriate Access-Control-* headers.
func (w *wiring) requireUploadToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodOptions {
			next.ServeHTTP(rw, r)
			return
		}

		tok := bearerToken(r.Header)
		if tok == "" {
			utils.WriteError(rw, http.StatusUnauthorized, "missing_token", "missing bearer token")
			return
		}

		claims, err := w.issuer.VerifyToken(r.Context(), tok, models.ActionUpload)
		if err != nil {
			utils.WriteError(rw, http.StatusUnauthorized, "invalid_token", "invalid or expired upload token")
			return
		}

		// HEAD/PATCH/DELETE address a specific upload via the URL path.
		// requireUploadToken wraps the StripPrefix handler rather than
		// being wrapped by it, so r.URL.Path here still carries
		// BasePath; strip it ourselves before comparing to the token's
		// UploadID so one upload's token can't touch another's.
		path := strings.TrimPrefix(r.URL.Path, w.basePath)
		if id := strings.Trim(path, "/"); id != "" && claims.UploadID != id {
			utils.WriteError(rw, http.StatusForbidden, "upload_mismatch", "upload token is not authorized for this upload")
			return
		}

		next.ServeHTTP(rw, r)
	})
}

// preCreate validates the upload token before a new tus upload is
// created, pins the upload's ID to the token's UploadID claim (so the
// staged file lands at a predictable, pre-authorized location), and
// stashes the claims needed at completion time (destination path,
// content type, file_id) in the upload's metadata.
func (w *wiring) preCreate(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	tok := bearerToken(hook.HTTPRequest.Header)
	if tok == "" {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("missing_token", "missing bearer token", http.StatusUnauthorized)
	}

	claims, err := w.issuer.VerifyToken(hook.Context, tok, models.ActionUpload)
	if err != nil {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("invalid_token", "invalid or expired upload token", http.StatusUnauthorized)
	}
	if claims.Path == "" {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("missing_claims", "upload token is missing path", http.StatusUnauthorized)
	}

	if claims.MaxUploadSize > 0 && hook.Upload.Size > claims.MaxUploadSize {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
			"upload_too_large",
			fmt.Sprintf("upload size %d exceeds authorized max %d", hook.Upload.Size, claims.MaxUploadSize),
			http.StatusRequestEntityTooLarge,
		)
	}

	metaData := tusd.MetaData{}
	for k, v := range hook.Upload.MetaData {
		metaData[k] = v
	}
	metaData[metaPathKey] = claims.Path
	metaData[metaFileIDKey] = claims.FileID
	metaData[metaFolderIDKey] = claims.FolderID
	metaData[metaOwnerIDKey] = claims.OwnerID
	metaData[metaVersionKey] = claims.Version
	metaData[metaBaseURLKey] = claims.BaseURL
	if claims.ContentType != "" {
		metaData[metaContentTypeKey] = claims.ContentType
	}

	return tusd.HTTPResponse{}, tusd.FileInfoChanges{
		ID:       claims.UploadID,
		MetaData: metaData,
	}, nil
}

// uploadResultMetadata builds the JSON object reported on the
// upload-result callback's Metadata field from the client-supplied tus
// Upload-Metadata (filename/filetype), independent of the server-injected
// claim keys read elsewhere in preFinish. Missing keys are simply
// omitted; a client that sent neither reports as "{}".
func uploadResultMetadata(meta tusd.MetaData) json.RawMessage {
	out := map[string]string{}
	if v := firstMetaValue(meta, filenameKeys); v != "" {
		out["file_name"] = v
	}
	if v := firstMetaValue(meta, filetypeKeys); v != "" {
		out["file_type"] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// preFinish is invoked once all bytes have been received but before the
// success response is sent to the client. It streams the finished
// upload straight into permanent storage via the upload Manager, then
// removes the tus staging copy.
func (w *wiring) preFinish(hook tusd.HookEvent) (tusd.HTTPResponse, error) {
	ctx := hook.Context

	path := hook.Upload.MetaData[metaPathKey]
	if path == "" {
		return tusd.HTTPResponse{}, tusd.NewError("missing_metadata", "upload is missing path metadata", http.StatusBadRequest)
	}
	fileID := hook.Upload.MetaData[metaFileIDKey]
	if fileID == "" {
		return tusd.HTTPResponse{}, tusd.NewError("missing_metadata", "upload is missing file_id metadata", http.StatusBadRequest)
	}

	dataStore := w.composer.Core
	stagedUpload, err := dataStore.GetUpload(ctx, hook.Upload.ID)
	if err != nil {
		return tusd.HTTPResponse{}, fmt.Errorf("tus: fetching finished upload: %w", err)
	}

	reader, err := stagedUpload.GetReader(ctx)
	if err != nil {
		return tusd.HTTPResponse{}, fmt.Errorf("tus: reading finished upload: %w", err)
	}
	defer reader.Close()

	result, err := w.manager.Commit(ctx, upload.CommitInput{
		Path:        path,
		UploadID:    hook.Upload.ID,
		FileID:      fileID,
		FolderID:    hook.Upload.MetaData[metaFolderIDKey],
		OwnerID:     hook.Upload.MetaData[metaOwnerIDKey],
		FileVersion: hook.Upload.MetaData[metaVersionKey],
		BaseURL:     hook.Upload.MetaData[metaBaseURLKey],
		Metadata:    uploadResultMetadata(hook.Upload.MetaData),
		Reader:      reader,
		Size:        hook.Upload.Size,
	})
	if err != nil {
		return tusd.HTTPResponse{}, fmt.Errorf("tus: committing upload: %w", err)
	}

	if w.composer.UsesTerminater {
		if term := w.composer.Terminater.AsTerminatableUpload(stagedUpload); term != nil {
			if err := term.Terminate(ctx); err != nil {
				w.logger.Warn("tus: failed to clean up staging file", "error", err, "upload_id", hook.Upload.ID)
			}
		}
	}

	w.logger.Info("tus_upload_committed",
		"path", path, "upload_id", hook.Upload.ID, "file_id", result.FileID,
		"size", result.Size, "content_type", result.ContentType,
		"checksum_algo", "sha256", "checksum", result.Checksum)

	return tusd.HTTPResponse{}, nil
}
