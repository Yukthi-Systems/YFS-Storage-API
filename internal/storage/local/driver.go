// Package local implements internal/storage.Storage on top of the local
// filesystem. Keys are treated as slash-separated relative paths under a
// single base directory.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

func init() {
	storage.Register("local", func(cfg storage.DriverConfig) (storage.Storage, error) {
		return New(cfg.LocalBasePath)
	})
}

// Driver is a storage.Storage backed by a directory on the local
// filesystem.
type Driver struct {
	basePath string
}

// New returns a local-filesystem Storage rooted at basePath. basePath is
// created if it does not already exist.
func New(basePath string) (*Driver, error) {
	if basePath == "" {
		return nil, errors.New("local: base path must not be empty")
	}
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("local: resolving base path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("local: creating base path: %w", err)
	}
	return &Driver{basePath: abs}, nil
}

// resolve turns a storage key into an absolute filesystem path, rejecting
// any key that would escape basePath (defense against path traversal from
// malformed file IDs).
func (d *Driver) resolve(key string) (string, error) {
	full := filepath.Join(d.basePath, filepath.FromSlash(key))
	rel, err := filepath.Rel(d.basePath, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("local: key %q escapes base path", key)
	}
	return full, nil
}

func (d *Driver) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (storage.FileMeta, error) {
	path, err := d.resolve(key)
	if err != nil {
		return storage.FileMeta{}, err
	}
	if err := utils.EnsureDir(filepath.Dir(path)); err != nil {
		return storage.FileMeta{}, err
	}

	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return storage.FileMeta{}, err
	}

	written, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return storage.FileMeta{}, copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return storage.FileMeta{}, closeErr
	}
	if size >= 0 && written != size {
		os.Remove(tmp)
		return storage.FileMeta{}, fmt.Errorf("local: wrote %d bytes, expected %d", written, size)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return storage.FileMeta{}, err
	}

	return d.Stat(ctx, key)
}

func (d *Driver) Get(ctx context.Context, key string) (io.ReadCloser, storage.FileMeta, error) {
	path, err := d.resolve(key)
	if err != nil {
		return nil, storage.FileMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, storage.FileMeta{}, storage.ErrNotFound
		}
		return nil, storage.FileMeta{}, err
	}
	meta, err := d.Stat(ctx, key)
	if err != nil {
		f.Close()
		return nil, storage.FileMeta{}, err
	}
	return f, meta, nil
}

func (d *Driver) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	path, err := d.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if length < 0 {
		return f, nil
	}
	return &limitedReadCloser{r: io.LimitReader(f, length), c: f}, nil
}

func (d *Driver) OpenSeeker(ctx context.Context, key string) (storage.ReadSeekCloser, storage.FileMeta, error) {
	path, err := d.resolve(key)
	if err != nil {
		return nil, storage.FileMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, storage.FileMeta{}, storage.ErrNotFound
		}
		return nil, storage.FileMeta{}, err
	}
	meta, err := d.Stat(ctx, key)
	if err != nil {
		f.Close()
		return nil, storage.FileMeta{}, err
	}
	return f, meta, nil
}

func (d *Driver) Delete(ctx context.Context, key string) error {
	path, err := d.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Driver) DeletePrefix(ctx context.Context, prefix string) error {
	if strings.TrimSpace(prefix) == "" {
		return errors.New("local: refusing to delete an empty prefix")
	}
	path, err := d.resolve(prefix)
	if err != nil {
		return err
	}
	if path == d.basePath {
		return fmt.Errorf("local: refusing to delete the storage root")
	}
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ListPrefix walks the directory tree rooted at prefix, calling fn once
// per file found (skipping directory entries themselves). A prefix that
// doesn't exist on disk simply yields no calls, matching Delete's "not
// found is not an error" convention rather than failing the walk.
func (d *Driver) ListPrefix(ctx context.Context, prefix string, fn func(key string) error) error {
	if strings.TrimSpace(prefix) == "" {
		return errors.New("local: refusing to list an empty prefix")
	}
	root, err := d.resolve(prefix)
	if err != nil {
		return err
	}
	if root == d.basePath {
		return errors.New("local: refusing to list the storage root")
	}

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(d.basePath, path)
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel))
	})
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (d *Driver) Exists(ctx context.Context, key string) (bool, error) {
	path, err := d.resolve(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (d *Driver) Stat(ctx context.Context, key string) (storage.FileMeta, error) {
	path, err := d.resolve(key)
	if err != nil {
		return storage.FileMeta{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return storage.FileMeta{}, storage.ErrNotFound
		}
		return storage.FileMeta{}, err
	}
	contentType, _ := utils.DetectContentType(path)

	return storage.FileMeta{
		Size:        info.Size(),
		ETag:        fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()),
		ModTime:     info.ModTime(),
		ContentType: contentType,
	}, nil
}

// Move relocates a file via utils.AtomicRename, whose cross-device
// fallback copies into a temp file and renames it into place before
// removing the source, so the destination is always complete before the
// source disappears. See storage.Storage.Move for the full contract.
func (d *Driver) Move(ctx context.Context, srcKey, dstKey string) error {
	if srcKey == dstKey {
		return nil
	}
	src, err := d.resolve(srcKey)
	if err != nil {
		return err
	}
	dst, err := d.resolve(dstKey)
	if err != nil {
		return err
	}
	return utils.AtomicRename(src, dst)
}

func (d *Driver) Copy(ctx context.Context, srcKey, dstKey string) error {
	src, err := d.resolve(srcKey)
	if err != nil {
		return err
	}
	dst, err := d.resolve(dstKey)
	if err != nil {
		return err
	}
	if err := utils.EnsureDir(filepath.Dir(dst)); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return storage.ErrNotFound
		}
		return err
	}
	defer in.Close()

	tmp := dst + ".part"
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
	return os.Rename(tmp, dst)
}

// SpaceUsage reports the capacity of the filesystem backing basePath via
// statfs(2). It implements storage.SpaceReporter, letting callers (e.g.
// upload-session admission control) gate on real disk usage rather than
// tracked file sizes, which would miss space used by anything else on
// the same volume.
func (d *Driver) SpaceUsage(ctx context.Context) (storage.SpaceUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(d.basePath, &stat); err != nil {
		return storage.SpaceUsage{}, fmt.Errorf("local: statfs: %w", err)
	}
	blockSize := int64(stat.Bsize)
	total := int64(stat.Blocks) * blockSize
	free := int64(stat.Bfree) * blockSize
	available := int64(stat.Bavail) * blockSize
	return storage.SpaceUsage{
		TotalBytes:     total,
		UsedBytes:      total - free,
		AvailableBytes: available,
	}, nil
}

type limitedReadCloser struct {
	r io.Reader
	c io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.c.Close() }
