package rustapi

import (
	"context"
	"encoding/json"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
)

// NotifyCreate tells the Rust API that a file version now exists in
// storage — a finished upload (new file, or new version of an existing
// one) or a new version created by a WOPI save — via
// POST /internal/callback/create.
func (c *Client) NotifyCreate(ctx context.Context, cb models.FileOpsCallback) error {
	return c.post(ctx, "/internal/callback/create", withMetadata(cb))
}

// NotifyDelete tells the Rust API that a file version it was expecting
// does not (or no longer) exist in storage — a failed or cancelled
// upload, or a removed version — via POST /internal/callback/delete.
// The Rust API only keys off file_id, owner_id and file_version here;
// the remaining fields must still be present but may be placeholders.
func (c *Client) NotifyDelete(ctx context.Context, cb models.FileOpsCallback) error {
	return c.post(ctx, "/internal/callback/delete", withMetadata(cb))
}

// withMetadata defaults a nil Metadata to "{}": the Rust API's
// FileOpsCallBack requires the field, and a nil RawMessage would encode
// as null.
func withMetadata(cb models.FileOpsCallback) models.FileOpsCallback {
	if len(cb.Metadata) == 0 {
		cb.Metadata = json.RawMessage("{}")
	}
	return cb
}
