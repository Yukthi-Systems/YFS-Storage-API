package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
)

// bearerOrQueryToken extracts the request's session token from the
// Authorization header if present, otherwise falls back to a "token" (or
// WOPI's "access_token") query parameter — browsers and Collabora both
// need the query-param form since they cannot always set custom headers
// (e.g. <video>/<img> tags, iframe src URLs).
func bearerOrQueryToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		return tok
	}
	return r.URL.Query().Get("access_token")
}

// authorizeFileGrant looks up the request's token and fileID in Redis
// (via issuer), ensuring the underlying session exists, authorizes
// wantAction, and was scoped to fileID — preventing a valid session for
// one file from being replayed against another.
func authorizeFileGrant(issuer *token.Issuer, r *http.Request, wantAction models.TokenAction, fileID string) (*models.Claims, error) {
	tok := bearerOrQueryToken(r)
	if tok == "" {
		return nil, fmt.Errorf("missing token")
	}
	claims, err := issuer.VerifyToken(r.Context(), tok, wantAction)
	if err != nil {
		return nil, err
	}
	if claims.FileID != fileID {
		return nil, fmt.Errorf("token is not authorized for this file")
	}
	return claims, nil
}
