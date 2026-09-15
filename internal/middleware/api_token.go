package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

// RequireAPIToken restricts a route to callers who present the exact
// shared secret in the X-API-Token header. It exists to lock down the
// internal-only endpoints (session issuance, direct file CRUD) to the
// Rust API alone — end users and Collabora reach the Storage API only
// through the short-lived session tokens those endpoints hand out, never through
// this header.
func RequireAPIToken(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-API-Token")
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
				utils.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid X-API-Token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
