package rustapi

import (
	"context"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
)

// NotifyNewVersion tells the Rust API that a versioning-enabled WOPI
// editing session's first save created a new file version, via
// POST /internal/callback/version/new. cb identifies where the new
// version's bytes live so the Rust API can record it against the file.
func (c *Client) NotifyNewVersion(ctx context.Context, cb models.NewVersionCallback) error {
	return c.post(ctx, "/internal/callback/version/new", cb)
}
