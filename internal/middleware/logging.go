package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// statusRecorder captures the status code written by the wrapped handler
// so it can be included in the access-log line (net/http does not expose
// this otherwise).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so callers using
// http.ResponseController (as tusd does internally for per-chunk
// SetReadDeadline/SetWriteDeadline calls) can reach the real writer's
// optional interfaces instead of failing with "feature not supported".
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// clientIP returns the real client address for a request that may have
// passed through a reverse proxy (e.g. Caddy). Caddy's reverse_proxy always
// appends the address it accepted the connection from as the last entry of
// X-Forwarded-For, so that entry cannot be spoofed by the client itself
// (any value the client supplies is pushed earlier in the list). Requests
// that don't come through a proxy fall back to the raw connection address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if last != "" {
			return last
		}
	}
	return r.RemoteAddr
}

// RequestLogging tags every request with a request ID (from the
// X-Request-Id header if the caller supplied one, otherwise a generated
// UUID), makes it available via RequestIDFromContext and the
// X-Request-Id response header, and emits one structured JSON access-log
// line per request with method, path, status, latency, and any extra
// fields (upload_id, path, file_id, ...) attached during the request
// via AddLogFields.
func RequestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			reqID := r.Header.Get("X-Request-Id")
			if reqID == "" {
				reqID = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", reqID)

			ctx := WithRequestID(r.Context(), reqID)
			ctx = withLogFieldBag(ctx)
			r = r.WithContext(ctx)

			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			attrs := []slog.Attr{
				slog.String("request_id", reqID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("latency", time.Since(start)),
				slog.String("remote_addr", clientIP(r)),
			}
			attrs = append(attrs, logFieldsFromContext(ctx)...)

			logger.LogAttrs(ctx, slog.LevelInfo, "http_request", attrs...)
		})
	}
}
