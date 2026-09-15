package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// Recovery catches panics from downstream handlers, logs them with a
// stack trace, and returns a 500 instead of crashing the connection (and,
// left unhandled, potentially the process).
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.LogAttrs(r.Context(), slog.LevelError, "panic_recovered",
						slog.String("request_id", RequestIDFromContext(r.Context())),
						slog.Any("panic", err),
						slog.String("stack", string(debug.Stack())),
					)
					utils.WriteError(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
