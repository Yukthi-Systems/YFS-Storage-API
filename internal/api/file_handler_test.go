package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/download"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage/local"
)

const testAPIToken = "test-secret-key"

// newInternalDownloadServer mirrors production: the local driver is
// rooted at "/" and file_location is a filesystem-absolute path.
func newInternalDownloadServer(t *testing.T) *http.ServeMux {
	t.Helper()
	store, err := local.New("/")
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	h := &Handlers{Download: download.New(store), RustAPIToken: testAPIToken}
	mux := http.NewServeMux()
	mux.Handle("GET /internal/files/download", middleware.RequireAPIToken(h.RustAPIToken)(http.HandlerFunc(h.handleInternalFileDownload)))
	return mux
}

func doInternalDownload(mux http.Handler, location, token string, setLocation bool) *httptest.ResponseRecorder {
	target := "/internal/files/download"
	if setLocation {
		target += "?file_location=" + url.QueryEscape(location)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestInternalFileDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "yfs", "530b2473", "57580bac")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(dir, "e0b87c27-a38f-486e-ae91-ab700dae1422.1")
	want := bytes.Repeat([]byte("yfs-archive\x00\xff"), 10_000)
	if err := os.WriteFile(filePath, want, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve"), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := newInternalDownloadServer(t)

	t.Run("success streams bytes and headers", func(t *testing.T) {
		rec := doInternalDownload(mux, filePath, testAPIToken, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(want)) {
			t.Errorf("Content-Length = %q, want %d", got, len(want))
		}
		if !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("body mismatch: got %d bytes, want %d", rec.Body.Len(), len(want))
		}
	})

	t.Run("missing token", func(t *testing.T) {
		if rec := doInternalDownload(mux, filePath, "", true); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		if rec := doInternalDownload(mux, filePath, "wrong", true); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("missing file_location", func(t *testing.T) {
		if rec := doInternalDownload(mux, "", testAPIToken, false); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("empty file_location", func(t *testing.T) {
		if rec := doInternalDownload(mux, "", testAPIToken, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		rec := doInternalDownload(mux, filepath.Join(dir, "missing.1"), testAPIToken, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		for _, loc := range []string{
			dir + "/../../secret.txt",
			root + "/yfs/../secret.txt",
			root + "/yfs/530b2473/../../../../../etc/passwd",
		} {
			rec := doInternalDownload(mux, loc, testAPIToken, true)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%q: status = %d, want 400", loc, rec.Code)
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("do not serve")) {
				t.Errorf("%q: leaked file content", loc)
			}
		}
	})
}
