package utils

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

// DetectContentType determines the MIME type of a file, preferring the
// extension-based mapping (fast, no read required) and falling back to
// sniffing the first 512 bytes of content via http.DetectContentType.
func DetectContentType(path string) (string, error) {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "application/octet-stream", nil
	}
	return http.DetectContentType(buf[:n]), nil
}

// SniffContentType peeks at the first 512 bytes of r to detect its
// content type and returns a reader that replays those bytes followed by
// the rest of r, so no data is lost. Use this when a filename/extension
// is not available (e.g. tus uploads, which are identified only by an
// opaque upload ID).
func SniffContentType(r io.Reader) (string, io.Reader, error) {
	buf := make([]byte, 512)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", nil, err
	}
	buf = buf[:n]
	contentType := http.DetectContentType(buf)
	return contentType, io.MultiReader(bytes.NewReader(buf), r), nil
}

// MediaKind is a coarse classification of a file's content type, used to
// pick appropriate response headers (Cache-Control, Content-Disposition)
// for the media service.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaAudio MediaKind = "audio"
	MediaPDF   MediaKind = "pdf"
	MediaOther MediaKind = "other"
)

// ClassifyMedia buckets a content type into a MediaKind.
func ClassifyMedia(contentType string) MediaKind {
	switch {
	case contentType == "application/pdf":
		return MediaPDF
	case len(contentType) >= 5 && contentType[:5] == "image":
		return MediaImage
	case len(contentType) >= 5 && contentType[:5] == "video":
		return MediaVideo
	case len(contentType) >= 5 && contentType[:5] == "audio":
		return MediaAudio
	default:
		return MediaOther
	}
}
