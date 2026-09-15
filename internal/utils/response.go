package utils

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// APIError is the JSON body returned for any non-2xx response.
type APIError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// WriteJSON marshals v as JSON and writes it with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode json response", "error", err)
	}
}

// WriteError writes a JSON error body with the given status code.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, APIError{Error: code, Message: message})
}
