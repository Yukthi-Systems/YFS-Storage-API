package utils

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// EnsureDir creates dir (and any parents) if it does not already exist.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// AtomicRename moves src to dst, creating dst's parent directory first and
// falling back to a copy+remove when src and dst live on different
// filesystems/volumes (os.Rename returns a LinkError in that case).
func AtomicRename(src, dst string) error {
	if err := EnsureDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("util: creating destination dir: %w", err)
	}

	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Cross-device rename: fall back to copy into a temp file on the
	// destination volume, then rename that temp file into place.
	if err := copyThenRemove(src, dst); err != nil {
		return fmt.Errorf("util: atomic rename fallback: %w", err)
	}
	return nil
}

func copyThenRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}

// FileSize returns the size in bytes of the file at path.
func FileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// FileExists reports whether path exists and is a regular file.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// PathNextToBinary returns name resolved next to the currently running
// binary (resolving a symlink if the binary was launched through one),
// falling back to "./<name>" if the executable's own path can't be
// determined. Useful as a zero-configuration default location for local
// state that should live alongside the deployed binary (e.g. a durable
// queue's database file) without callers needing to know the deployment
// path.
func PathNextToBinary(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return "./" + name
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), name)
}
