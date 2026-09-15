package middleware

import (
	"net/http"
	"regexp"
	"strings"
)

// CORS restricts cross-origin browser requests to the given comma-separated
// exact origins (e.g. "https://app.example.com,http://localhost:3000"), the
// same format as tus's CorsAllowedOrigins (see internal/tus.corsConfig).
// Leaving allowedOrigins empty, or set to "*", accepts every origin.
//
// It exists for browser-facing endpoints the React app calls directly via
// fetch/XHR rather than a plain <a>/<img> navigation — currently just
// /download/{fileID} when the caller needs the response body as JS (e.g. to
// show progress or save it via a Blob) instead of letting the browser
// navigate to the URL directly.
func CORS(allowedOrigins string) func(http.Handler) http.Handler {
	allowAny, allowOrigin := parseCorsOrigins(allowedOrigins)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			switch {
			case origin != "" && allowAny:
				// download is authorized via a bearer/query token, never
				// cookies, so a literal "*" (no Access-Control-Allow-
				// Credentials) is both simplest and spec-legal here —
				// unlike echoing the origin, "*" can't be paired with
				// credentials mode at all.
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, Content-Range, ETag, Last-Modified")
			case origin != "" && allowOrigin.MatchString(origin):
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, Content-Range, ETag, Last-Modified")
			}

			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Range")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// parseCorsOrigins builds an origin matcher from a comma-separated list of
// exact origins. allowAny is true (matching any origin) when allowedOrigins
// is empty or "*".
func parseCorsOrigins(allowedOrigins string) (allowAny bool, allowOrigin *regexp.Regexp) {
	allowedOrigins = strings.TrimSpace(allowedOrigins)
	if allowedOrigins == "" || allowedOrigins == "*" {
		return true, nil
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
	if len(patterns) == 0 {
		return true, nil
	}
	return false, regexp.MustCompile(strings.Join(patterns, "|"))
}
