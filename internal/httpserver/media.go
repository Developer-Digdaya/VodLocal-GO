package httpserver

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// MediaResolver memetakan path media yang diminta klien ke path absolut di
// disk. Implementasi wajib menolak path yang keluar dari root-nya sendiri;
// handler di paket ini sudah membuang percobaan traversal yang paling umum,
// tetapi resolver adalah lapisan pertahanan terakhir.
//
// File yang tidak ada dilaporkan dengan error yang membungkus fs.ErrNotExist
// atau dengan ErrMediaNotFound, dan path yang ditolak dengan
// ErrInvalidMediaPath.
type MediaResolver interface {
	// ResolveMedia memetakan rel di dalam generation (mis. "gen-000042").
	ResolveMedia(ctx context.Context, generation, rel string) (string, error)
}

// Error yang bisa dikembalikan implementasi MediaResolver untuk menentukan
// status HTTP. Error lain selalu dianggap kegagalan internal (500).
var (
	// ErrMediaNotFound menandai berkas media yang tidak ada (404).
	ErrMediaNotFound = errors.New("httpserver: media not found")
	// ErrInvalidMediaPath menandai path media yang tidak valid (400).
	ErrInvalidMediaPath = errors.New("httpserver: invalid media path")
)

const (
	// segmentCacheControl dipakai segmen yang isinya tidak pernah berubah.
	segmentCacheControl = "public, max-age=31536000, immutable"
	// playlistCacheControl dipakai playlist dan subtitle.
	playlistCacheControl = "public, max-age=3600"
)

// MediaHandler melayani GET /media/{generation}/* sebagai bytes mentah, tanpa
// envelope JSON (docs/api-spec.md 11).
//
// Content-Type ditentukan dari ekstensi, Cache-Control mengikuti 13, dan body
// dikirim lewat http.ServeContent sehingga Range, If-Range, Last-Modified, dan
// ETag ditangani otomatis.
func MediaHandler(res MediaResolver) http.Handler {
	return &fileHandler{res: res}
}

// fileHandler melayani berkas dari media generation.
type fileHandler struct {
	res MediaResolver
}

// ServeHTTP mengubah request menjadi satu berkas di disk lalu mengirimnya.
func (h *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.res == nil {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	generation, rel, ok := mediaTarget(r)
	if !ok {
		writeStatus(w, http.StatusBadRequest)
		return
	}

	path, err := h.res.ResolveMedia(r.Context(), generation, rel)
	if err != nil {
		writeStatus(w, resolveStatus(err))
		return
	}

	file, err := os.Open(path)
	if err != nil {
		writeStatus(w, openStatus(err))
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeStatus(w, http.StatusNotFound)
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	w.Header().Set("Content-Type", mediaContentType(ext))
	if cacheControl := cacheControl(ext); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// mediaTarget mengambil generation dan path relatif dari URL params chi, lalu
// memvalidasinya. Bila handler dipasang di luar router chi (mis. dipanggil
// langsung pada test), nilainya diturunkan dari URL path.
func mediaTarget(r *http.Request) (generation, rel string, ok bool) {
	generation = chi.URLParam(r, "generation")
	rel = chi.URLParam(r, "*")
	if generation == "" || rel == "" {
		if rest, found := strings.CutPrefix(r.URL.Path, "/media/"); found {
			if first, tail, hasTail := strings.Cut(rest, "/"); hasTail {
				generation, rel = first, tail
			}
		}
	}
	if !safeSegment(generation) {
		return "", "", false
	}
	rel, ok = safeRel(rel)
	return generation, rel, ok
}

// safeSegment memvalidasi label generation: tidak kosong, tanpa pemisah path,
// dan tanpa komponen ".." (baik mentah maupun setelah URL-unescape).
func safeSegment(segment string) bool {
	if segment == "" || strings.ContainsRune(segment, 0) {
		return false
	}
	if strings.ContainsAny(segment, `/\`) {
		return false
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return false
	}
	return !strings.Contains(segment, "..") && !strings.Contains(decoded, "..")
}

// safeRel memvalidasi path relatif media dan mengembalikan bentuk yang sudah
// di-unescape. Path yang memuat ".." (termasuk "%2e%2e") atau NUL ditolak,
// begitu juga path absolut dan path kosong.
func safeRel(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	decoded, err := url.PathUnescape(rel)
	if err != nil {
		return "", false
	}
	for _, candidate := range []string{rel, decoded} {
		if strings.Contains(candidate, "..") || strings.ContainsRune(candidate, 0) {
			return "", false
		}
	}
	if decoded == "" || decoded == "." || strings.HasPrefix(decoded, "/") {
		return "", false
	}
	return decoded, true
}

// resolveStatus memetakan error resolver ke status HTTP.
func resolveStatus(err error) int {
	switch {
	case errors.Is(err, ErrMediaNotFound), errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidMediaPath):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// openStatus memetakan error os.Open ke status HTTP.
func openStatus(err error) int {
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// mediaContentType mengembalikan Content-Type berdasarkan ekstensi berkas
// (api-spec 12.2). Ekstensi tak dikenal dilayani sebagai bytes mentah.
func mediaContentType(ext string) string {
	switch ext {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".m4s":
		return "video/iso.segment"
	case ".vtt":
		return "text/vtt; charset=utf-8"
	case ".key":
		return "application/octet-stream"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// cacheControl mengembalikan nilai Cache-Control untuk ekstensi tersebut
// (api-spec 13). Tipe yang tidak diatur dokumen tidak diberi header.
func cacheControl(ext string) string {
	switch ext {
	case ".ts", ".m4s":
		// Segmen tidak pernah berubah isinya: generation baru berarti path baru.
		return segmentCacheControl
	case ".m3u8", ".vtt":
		return playlistCacheControl
	default:
		return ""
	}
}

// writeStatus mengirim status HTTP tanpa body dan tanpa envelope JSON. Ini
// bentuk error yang benar untuk route media (docs/standar-response.md 11).
func writeStatus(w http.ResponseWriter, status int) {
	w.Header().Del("Cache-Control")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
}
