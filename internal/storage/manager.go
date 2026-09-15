package storage

import "github.com/Yukthi-Systems/YFS-Storage-API/internal/config"

// NewFromConfig adapts internal/config.Config into a DriverConfig and
// constructs the Storage implementation it selects. cmd/server is
// expected to blank-import the driver package(s) it needs (e.g.
// internal/storage/local, internal/storage/s3) before calling this.
func NewFromConfig(cfg *config.Config) (Storage, error) {
	return New(string(cfg.StorageDriver), DriverConfig{
		LocalBasePath:     cfg.LocalBasePath,
		S3Bucket:          cfg.S3Bucket,
		S3Region:          cfg.S3Region,
		S3Endpoint:        cfg.S3Endpoint,
		S3AccessKeyID:     cfg.S3AccessKeyID,
		S3SecretAccessKey: cfg.S3SecretAccessKey,
		S3UsePathStyle:    cfg.S3UsePathStyle,
	})
}
