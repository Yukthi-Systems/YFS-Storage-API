package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// SHA256 streams r and returns the lowercase hex-encoded digest without
// buffering the whole file in memory.
func SHA256(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashingReader wraps an io.Reader and accumulates a SHA-256 digest of
// every byte read through it, so a checksum can be computed as a side
// effect of a single pass (e.g. while writing an upload to disk) instead
// of a second read of the data.
type HashingReader struct {
	source io.Reader
	hasher hash.Hash
}

// NewHashingReader returns a reader that transparently hashes everything
// read from src.
func NewHashingReader(src io.Reader) *HashingReader {
	hasher := sha256.New()
	return &HashingReader{source: io.TeeReader(src, hasher), hasher: hasher}
}

func (hr *HashingReader) Read(p []byte) (int, error) {
	return hr.source.Read(p)
}

// Sum256 returns the hex-encoded SHA-256 digest of all bytes read so far.
func (hr *HashingReader) Sum256() string {
	return hex.EncodeToString(hr.hasher.Sum(nil))
}
