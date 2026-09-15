// Package util holds small, dependency-free helpers shared across the
// Storage API. Nothing here may depend on internal/service, internal/api,
// or internal/storage, to keep the dependency graph acyclic.
package utils

import (
	"path"
	"strings"
)

// fileLocationSep joins a storage node's base_url and a storage path
// into the single "<base_url>;<path>" file_location string the Rust API
// sends on session creation and expects back on the upload-result
// callback.
const fileLocationSep = ";"

// SplitFileLocation splits a "<base_url>;<path>" file_location string
// into its base_url and path halves. If the separator isn't found, the
// whole value is treated as the path with an empty base_url.
func SplitFileLocation(location string) (baseURL, path string) {
	if before, after, ok := strings.Cut(location, fileLocationSep); ok {
		return before, after
	}
	return "", location
}

// JoinFileLocation rebuilds the complete "<base_url>;<path>"
// file_location string from its two halves.
func JoinFileLocation(baseURL, path string) string {
	return baseURL + fileLocationSep + path
}

// Storage keys are backend-agnostic, forward-slash separated strings
// (never absolute filesystem paths) — not derived here. Each Storage
// driver decides for itself how a key maps onto its backend (the local
// driver joins it under its base directory; the s3 driver uses it as the
// object key verbatim).
//
// The complete key for a file's content, thumbnail, preview, or trash
// copy is decided entirely by the Rust metadata service and given to the
// Storage API verbatim (via request bodies and session claims) — the Storage
// API attaches no meaning to it and derives no part of it. This buys the
// Rust service full control over its own folder hierarchy, ownership,
// and per-org/per-user layout without the Storage API needing to know
// any of it.
//
// The one exception is in-progress (tus) uploads: since a resumable
// upload isn't yet associated with any Rust-owned path when it starts,
// it stages under a generic, ID-keyed area until the caller-supplied
// destination path is known at commit time (see UploadKey below).

// UploadKey returns the staging key for an in-progress (tus) upload:
// uploads/<upload-id>.
func UploadKey(uploadID string) string {
	return path.Join("uploads", uploadID)
}
