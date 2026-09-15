package storage

import "fmt"

// DriverConfig is the superset of settings any registered driver factory
// might need. Each driver reads only the fields relevant to it, so adding
// a new backend (GCS, Azure Blob, ...) means adding fields here and a new
// registered factory — no changes to existing drivers or to callers of
// New.
type DriverConfig struct {
	LocalBasePath string

	S3Bucket          string
	S3Region          string
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3UsePathStyle    bool
}

// Factory constructs a Storage implementation from a DriverConfig.
type Factory func(cfg DriverConfig) (Storage, error)

var registry = map[string]Factory{}

// Register makes a driver factory available under name. Driver packages
// call this from an init() function; the top-level storage package never
// imports driver packages directly, which is what keeps
// internal/storage/local and internal/storage/s3 free to depend on this
// package without creating an import cycle. Whoever assembles the binary
// (cmd/server) is responsible for blank-importing the driver packages it
// wants available.
func Register(name string, f Factory) {
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("storage: driver %q already registered", name))
	}
	registry[name] = f
}

// New constructs the Storage implementation registered under
// driverName. Callers get this via internal/storage/manager.go, which
// adapts it to internal/config.Config.
func New(driverName string, cfg DriverConfig) (Storage, error) {
	factory, ok := registry[driverName]
	if !ok {
		return nil, fmt.Errorf("storage: no driver registered for %q (forgot a blank import?)", driverName)
	}
	return factory(cfg)
}
