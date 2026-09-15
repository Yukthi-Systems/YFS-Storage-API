package rustapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
)

// NotifyUploadResult tells the Rust API a tus upload has finished —
// successfully or not — via POST /internal/callback/upload/{is_success}.
// success travels in the URL path; cb is the body, matching the Rust
// API's FileOpsCallBack struct.
func (c *Client) NotifyUploadResult(ctx context.Context, success bool, cb models.UploadCallback) error {
	if cb.Metadata == nil {
		cb.Metadata = json.RawMessage("{}")
	}
	path := fmt.Sprintf("/internal/callback/upload/%t", success)
	return c.post(ctx, path, cb)
}
