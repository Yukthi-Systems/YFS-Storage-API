// Package config loads and validates the Storage API's runtime
// configuration from environment variables (and an optional .env file).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// StorageDriver identifies which Storage implementation to construct.
type StorageDriver string

const (
	DriverLocal StorageDriver = "local"
	DriverS3    StorageDriver = "s3"
)

// Config is the fully resolved configuration for the Storage API process.
// It is built once at startup and passed by value/pointer to constructors;
// nothing in the codebase should read environment variables directly after
// Load returns.
type Config struct {
	// Server
	ListenAddr      string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// BaseURL is this application's own public-facing base URL (e.g.
	// "https://files.example.com"), used to build fully-qualified URLs
	// returned to callers.
	BaseURL string

	// Storage
	StorageDriver StorageDriver
	LocalBasePath string

	S3Bucket          string
	S3Region          string
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3UsePathStyle    bool

	// RustAPIToken authenticates the trusted Rust API on internal-only
	// endpoints (session issuance, direct file CRUD). No other caller
	// may know this value.
	RustAPIToken string

	// Redis backs the session store: every issued session (upload,
	// download, stream, WOPI) lives there as an entry with its own TTL,
	// and revocation is just deleting that entry.
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// TokenEncryptionKey is a base64-encoded 32-byte (AES-256) key used
	// to seal every issued session token around its file_id.
	TokenEncryptionKey string

	// MaxStorageUsagePercent gates new upload session creation once the
	// storage backend's used capacity reaches this percentage (0-100).
	// Only enforced for backends that report real usage (the local
	// driver; object stores like S3 have no finite capacity to check).
	// 0 disables the check.
	MaxStorageUsagePercent float64
	// StorageBackoff is the Retry-After hint returned alongside a 507
	// Insufficient Storage response, telling the caller how long to
	// back off before requesting another upload session.
	StorageBackoff time.Duration

	// MaxUploadBatchSize caps how many files a single /sessions/upload
	// request may authorize at once, bounding the size of the claims
	// blob cached in Redis under the resulting token.
	MaxUploadBatchSize int

	// MaxDeleteBatchSize caps how many paths a single /files/delete
	// request may enqueue at once.
	MaxDeleteBatchSize int
	// DeleteWorkerCount is the number of concurrent goroutines the purge
	// queue uses to remove files enqueued via /files/delete.
	DeleteWorkerCount int

	// Tus
	TusBasePath      string
	TusMaxUploadSize int64
	// TusStagingDir is where in-progress tus uploads are buffered before
	// being moved into the storage backend. Deliberately independent of
	// LocalBasePath: the local driver's base path is the caller-supplied
	// key namespace (which may itself be "/" when keys are already
	// filesystem-absolute), not a safe place to stage temp files.
	TusStagingDir string
	// TusCorsAllowedOrigins is a comma-separated list of exact origins
	// (e.g. "https://app.example.com,http://localhost:3000") allowed to
	// make cross-origin requests to the tus upload endpoint. Empty allows
	// any origin, matching tusd's own default.
	TusCorsAllowedOrigins string

	// DownloadCorsAllowedOrigins is the same comma-separated exact-origin
	// list as TusCorsAllowedOrigins, but for /download/{fileID} — the only
	// other endpoint the React app's browser JS (rather than the Rust API
	// server-to-server, or Collabora's WOPI client) calls directly. Empty
	// allows any origin.
	DownloadCorsAllowedOrigins string

	// WOPISessionTTL is how long a WOPI session (Collabora's
	// access_token) stays valid. Unlike upload/download/stream, the
	// Rust API's WOPI grant request carries no per-request TTL, so this
	// is the only source for it.
	WOPISessionTTL time.Duration

	// Logging
	LogLevel      string
	LogOutput     string // "file" or "stdout"
	LogDir        string
	LogFileName   string
	LogMaxSizeMB  int
	LogMaxBackups int
	LogMaxAgeDays int

	// RustCallbackBaseURL is the Rust API's own base URL, e.g.
	// "https://yfs-api.test.yukthi.net", used by the Storage API to
	// report upload results back to it (see internal/rustapi).
	RustCallbackBaseURL string
	// RustCallbackAPIKey authenticates the Storage API to the Rust
	// API's internal callback endpoints, sent as the x-api-key header.
	RustCallbackAPIKey string
}

// Load reads a .env file if present (it is not an error for it to be
// absent — real deployments set environment variables directly) and then
// builds a validated Config from the process environment.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		ListenAddr:      getEnv("LISTEN_ADDR", ":8080"),
		ReadTimeout:     getEnvDuration("READ_TIMEOUT", 30*time.Second),
		WriteTimeout:    getEnvDuration("WRITE_TIMEOUT", 60*time.Second),
		IdleTimeout:     getEnvDuration("IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
		BaseURL:         getEnv("BASE_URL", ""),

		StorageDriver: StorageDriver(getEnv("STORAGE_DRIVER", string(DriverLocal))),
		LocalBasePath: getEnv("LOCAL_BASE_PATH", "./data"),

		S3Bucket:          getEnv("S3_BUCKET", ""),
		S3Region:          getEnv("S3_REGION", "us-east-1"),
		S3Endpoint:        getEnv("S3_ENDPOINT", ""),
		S3AccessKeyID:     getEnv("S3_ACCESS_KEY_ID", ""),
		S3SecretAccessKey: getEnv("S3_SECRET_ACCESS_KEY", ""),
		S3UsePathStyle:    getEnvBool("S3_USE_PATH_STYLE", true),

		RustAPIToken: getEnv("RUST_API_TOKEN", ""),

		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		TokenEncryptionKey: getEnv("TOKEN_ENCRYPTION_KEY", ""),

		MaxStorageUsagePercent: getEnvFloat("MAX_STORAGE_USAGE_PERCENT", 90),
		StorageBackoff:         getEnvDuration("STORAGE_BACKOFF", 30*time.Second),
		MaxUploadBatchSize:     getEnvInt("MAX_UPLOAD_BATCH_SIZE", 100),
		MaxDeleteBatchSize:     getEnvInt("MAX_DELETE_BATCH_SIZE", 1000),
		DeleteWorkerCount:      getEnvInt("DELETE_WORKER_COUNT", 8),

		TusBasePath:           getEnv("TUS_BASE_PATH", "/upload/tus/"),
		TusMaxUploadSize:      getEnvInt64("TUS_MAX_UPLOAD_SIZE", 10*1024*1024*1024), // 10GiB
		TusStagingDir:         getEnv("TUS_STAGING_DIR", "./data/_tus-staging"),
		TusCorsAllowedOrigins: getEnv("TUS_CORS_ALLOWED_ORIGINS", ""),

		DownloadCorsAllowedOrigins: getEnv("DOWNLOAD_CORS_ALLOWED_ORIGINS", ""),

		WOPISessionTTL: getEnvDuration("WOPI_SESSION_TTL", 30*time.Minute),

		LogLevel:      getEnv("LOG_LEVEL", "info"),
		LogOutput:     getEnv("LOG_OUTPUT", "file"),
		LogDir:        getEnv("LOG_DIR", "./logs"),
		LogFileName:   getEnv("LOG_FILE_NAME", "storage-api.log"),
		LogMaxSizeMB:  getEnvInt("LOG_MAX_SIZE_MB", 100),
		LogMaxBackups: getEnvInt("LOG_MAX_BACKUPS", 7),
		LogMaxAgeDays: getEnvInt("LOG_MAX_AGE_DAYS", 30),

		RustCallbackBaseURL: getEnv("RUST_CALLBACK_BASE_URL", "https://yfs-api.test.yukthi.net"),
		RustCallbackAPIKey:  getEnv("RUST_CALLBACK_API_KEY", ""),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.RustAPIToken == "" {
		return fmt.Errorf("config: RUST_API_TOKEN is required")
	}
	if c.RedisAddr == "" {
		return fmt.Errorf("config: REDIS_ADDR is required")
	}
	if c.MaxStorageUsagePercent < 0 || c.MaxStorageUsagePercent > 100 {
		return fmt.Errorf("config: MAX_STORAGE_USAGE_PERCENT must be between 0 and 100")
	}
	if c.MaxUploadBatchSize <= 0 {
		return fmt.Errorf("config: MAX_UPLOAD_BATCH_SIZE must be positive")
	}
	if c.MaxDeleteBatchSize <= 0 {
		return fmt.Errorf("config: MAX_DELETE_BATCH_SIZE must be positive")
	}
	if c.DeleteWorkerCount <= 0 {
		return fmt.Errorf("config: DELETE_WORKER_COUNT must be positive")
	}
	if c.TokenEncryptionKey == "" {
		return fmt.Errorf("config: TOKEN_ENCRYPTION_KEY is required")
	}
	if c.TusStagingDir == "" {
		return fmt.Errorf("config: TUS_STAGING_DIR is required")
	}
	switch c.StorageDriver {
	case DriverLocal:
		if c.LocalBasePath == "" {
			return fmt.Errorf("config: LOCAL_BASE_PATH is required for the local storage driver")
		}
	case DriverS3:
		if c.S3Bucket == "" {
			return fmt.Errorf("config: S3_BUCKET is required for the s3 storage driver")
		}
	default:
		return fmt.Errorf("config: unsupported STORAGE_DRIVER %q", c.StorageDriver)
	}
	switch c.LogOutput {
	case "file", "stdout":
	default:
		return fmt.Errorf("config: unsupported LOG_OUTPUT %q (must be \"file\" or \"stdout\")", c.LogOutput)
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func getEnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvFloat(key string, fallback float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func getEnvInt64(key string, fallback int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
