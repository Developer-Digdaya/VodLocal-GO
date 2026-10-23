package httpserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"vodlocal/internal/httpserver"
)

// testResolver adalah MediaResolver tiruan yang menyimpan berkas di bawah satu
// root dan menolak path yang keluar dari direktori dasarnya, seperti
// implementasi penyimpanan yang sebenarnya.
type testResolver struct {
	root string
	// err, bila diisi, selalu dikembalikan kedua method.
	err error
	// calls menghitung berapa kali resolver dipanggil.
	calls int
	// lastGeneration dan lastRel merekam argumen pemanggilan terakhir.
	lastGeneration string
	lastRel        string
}

func (r *testResolver) ResolveMedia(ctx context.Context, generation, rel string) (string, error) {
	r.calls++
	r.lastGeneration, r.lastRel = generation, rel
	if r.err != nil {
		return "", r.err
	}
	return r.within(filepath.Join(r.root, generation), rel)
}

// within menggabungkan rel ke base dan menolak hasil yang keluar dari base.
func (r *testResolver) within(base, rel string) (string, error) {
	if rel == "" {
		return "", httpserver.ErrInvalidMediaPath
	}
	full := filepath.Join(base, filepath.FromSlash(rel))
	inside, err := filepath.Rel(base, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", httpserver.ErrInvalidMediaPath
	}
	if _, err := os.Stat(full); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", httpserver.ErrMediaNotFound
		}
		return "", err
	}
	return full, nil
}

// mediaRouter memasang handler media pada router chi sungguhan agar URL params
// {generation} dan * tersedia.
func mediaRouter(res httpserver.MediaResolver) http.Handler {
	r := chi.NewRouter()
	r.Get("/media/{generation}/*", httpserver.MediaHandler(res).ServeHTTP)
	return r
}

// writeFile menulis content ke path relatif terhadap base, membuat direktori
// yang belum ada.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMediaHandlerSegment(t *testing.T) {
	root := t.TempDir()
	const body = "SEGMENT-0123456789"
	writeFile(t, filepath.Join(root, "gen-000001", "seg-00001.ts"), body)

	rec := doGet(t, mediaRouter(&testResolver{root: root}), "/media/gen-000001/seg-00001.ts")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Type"), "video/mp2t"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Accept-Ranges"), "bytes"; got != want {
		t.Errorf("Accept-Ranges = %q, want %q", got, want)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestMediaHandlerRange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "gen-000001", "seg-00001.ts"), "0123456789")

	req := httptest.NewRequest(http.MethodGet, "/media/gen-000001/seg-00001.ts", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	mediaRouter(&testResolver{root: root}).ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got, want := rec.Body.String(), "0123"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := len(rec.Body.Bytes()), 4; got != want {
		t.Errorf("panjang body = %d, want %d", got, want)
	}
	if got, want := rec.Header().Get("Content-Range"), "bytes 0-3/10"; got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}

func TestMediaHandlerPlaylist(t *testing.T) {
	root := t.TempDir()
	const playlist = "#EXTM3U\n#EXT-X-VERSION:3\n"
	writeFile(t, filepath.Join(root, "gen-000001", "index.m3u8"), playlist)

	rec := doGet(t, mediaRouter(&testResolver{root: root}), "/media/gen-000001/index.m3u8")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/vnd.apple.mpegurl"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=3600"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got := rec.Body.String(); got != playlist {
		t.Errorf("body = %q, want %q", got, playlist)
	}
}

func TestMediaHandlerRejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "media")
	writeFile(t, filepath.Join(root, "secret.txt"), "TOPSECRET-INSIDE-MEDIA-ROOT")
	writeFile(t, filepath.Join(tmp, "secret.txt"), "TOPSECRET-OUTSIDE-MEDIA-ROOT")
	writeFile(t, filepath.Join(root, "gen-000001", "seg-00001.ts"), "ok")

	for _, target := range []string{
		"/media/gen-000001/../secret.txt",
		"/media/gen-000001/%2e%2e%2fsecret.txt",
		"/media/gen-000001/../../secret.txt",
		"/media/gen-000001/%2E%2E%2F%2E%2E%2Fsecret.txt",
	} {
		t.Run(target, func(t *testing.T) {
			res := &testResolver{root: root}
			rec := doGet(t, mediaRouter(res), target)

			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 400 atau 404", rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, "TOPSECRET") {
				t.Fatalf("body membocorkan berkas di luar root: %q", body)
			}
			if res.calls != 0 {
				t.Errorf("resolver dipanggil %d kali untuk path tidak valid", res.calls)
			}
		})
	}
}

func TestMediaHandlerMissingFile(t *testing.T) {
	root := t.TempDir()
	rec := doGet(t, mediaRouter(&testResolver{root: root}), "/media/gen-000001/tidak-ada.ts")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("body = %q, want kosong (tanpa envelope)", body)
	}
	if got := rec.Header().Get("Content-Type"); strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, route media tidak boleh memakai envelope JSON", got)
	}
}

func TestMediaHandlerResolverErrors(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", httpserver.ErrMediaNotFound, http.StatusNotFound},
		{"wrapped not exist", errors.Join(errors.New("context"), os.ErrNotExist), http.StatusNotFound},
		{"invalid path", httpserver.ErrInvalidMediaPath, http.StatusBadRequest},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(t, mediaRouter(&testResolver{root: root, err: tc.err}), "/media/gen-000001/seg-00001.ts")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestMediaContentTypes(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"index.m3u8":   "application/vnd.apple.mpegurl",
		"seg-00001.ts": "video/mp2t",
		"init.m4s":     "video/iso.segment",
		"sub.vtt":      "text/vtt; charset=utf-8",
		"enc.key":      "application/octet-stream",
		"poster.jpg":   "image/jpeg",
		"poster.jpeg":  "image/jpeg",
		"poster.png":   "image/png",
		"catatan.bin":  "application/octet-stream",
	}
	res := &testResolver{root: root}
	for name, want := range files {
		writeFile(t, filepath.Join(root, "gen-000001", name), "x")
		t.Run(name, func(t *testing.T) {
			rec := doGet(t, mediaRouter(res), "/media/gen-000001/"+name)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
		})
	}
}
