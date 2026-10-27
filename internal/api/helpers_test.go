package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"vodlocal/internal/api"
	"vodlocal/internal/response"
	"vodlocal/internal/store"
)

// fixture adalah satu database SQLite asli (store.Open + Migrate di t.TempDir)
// dengan data katalog yang siap dipakai seluruh test.
type fixture struct {
	t       *testing.T
	db      *sql.DB
	now     time.Time
	handler http.Handler
}

// newFixture menyiapkan database, menjalankan migrasi, mengisi data contoh, dan
// merakit router. adminOnly nil berarti route admin tidak terpasang.
func newFixture(t *testing.T, adminOnly func(http.Handler) http.Handler) *fixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "vodlocal-test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	f := &fixture{
		t:   t,
		db:  st.Write(),
		now: time.Date(2026, 10, 20, 11, 30, 0, 0, time.UTC),
	}
	f.seed()

	f.handler = api.NewRouter(api.Config{
		Read:      st.Read(),
		Write:     st.Write(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminOnly: adminOnly,
		Now:       func() time.Time { return f.now },
	})
	return f
}

// exec menjalankan satu perintah seed.
func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("seed gagal: %v\nquery: %s", err, query)
	}
}

// seed mengisi provider, generation aktif, katalog, dan rendisi.
func (f *fixture) seed() {
	// Provider.
	f.exec(`INSERT INTO providers (id, code, name) VALUES (1, 'dens_tv', 'Dens TV')`)

	// Generation aktif.
	f.exec(`INSERT INTO generations (id, label, root_path, status, file_count, total_bytes)
	        VALUES (1, 'gen-000001', '/srv/media/generations/gen-000001', 'active', 12, 4096)`)

	// Judul: 1 series siap, 2 movie siap, 3 movie belum siap (hanya rendisi failed).
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, synopsis,
	            release_year, content_rating, language, poster_path, backdrop_path,
	            duration_seconds, is_published, created_at, updated_at)
	        VALUES (1, 1, 'ext-1', 'series', 'Cinta Pertama', 'cinta-pertama', 'Kisah cinta pertama',
	            2024, '13+', 'id', 'dens_tv/1/poster.jpg', 'dens_tv/1/backdrop.jpg',
	            NULL, 1, '2026-10-20T09:00:00Z', '2026-10-20T10:00:00Z')`)
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, synopsis,
	            release_year, content_rating, language, poster_path, backdrop_path,
	            duration_seconds, is_published, created_at, updated_at)
	        VALUES (2, 1, 'ext-2', 'movie', 'Film Lama', 'film-lama', 'Film lawas',
	            2019, 'SU', 'id', 'dens_tv/2/poster.jpg', 'dens_tv/2/backdrop.jpg',
	            5400, 1, '2026-10-20T09:00:00Z', '2026-10-20T11:00:00Z')`)
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, synopsis,
	            release_year, content_rating, language, poster_path, backdrop_path,
	            duration_seconds, is_published, created_at, updated_at)
	        VALUES (3, 1, 'ext-3', 'movie', 'Belum Siap', 'belum-siap', 'Belum terunduh',
	            2025, '17+', 'id', 'dens_tv/3/poster.jpg', 'dens_tv/3/backdrop.jpg',
	            6000, 1, '2026-10-20T09:00:00Z', '2026-10-20T12:00:00Z')`)

	// Episode.
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name,
	            synopsis, duration_seconds, air_date, still_path, created_at, updated_at)
	        VALUES (301, 1, 'ep-301', 1, 1, 'Awal', 'Episode pembuka', 2700, '2024-01-10',
	            'dens_tv/1/still-1.jpg', '2026-10-20T09:00:00Z', '2026-10-20T09:00:00Z')`)
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name,
	            synopsis, duration_seconds, air_date, still_path, created_at, updated_at)
	        VALUES (302, 1, 'ep-302', 1, 2, 'Cinta Kedua', 'Episode kedua', 2600, '2024-01-17',
	            'dens_tv/1/still-2.jpg', '2026-10-20T09:00:00Z', '2026-10-20T09:00:00Z')`)

	// Playable: 9001 & 9002 milik series 1, 9003 movie 2, 9004 movie 3.
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9001, 1, 301, 2700)`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9002, 1, 302, 2600)`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9003, 2, NULL, 5400)`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9004, 3, NULL, 6000)`)

	// Rendisi pada generation aktif: 9001 siap (720p failed tidak boleh muncul),
	// 9002 pending, 9003 siap, 9004 failed.
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9001, 1, '720p', 2500, 1280, 720, 'dens_tv/series/1/s01e01/720p/index.m3u8', 'failed')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9001, 1, '480p', 1000, 854, 480, 'dens_tv/series/1/s01e01/480p/index.m3u8', 'ready')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9001, 1, '360p', 600, 640, 360, 'dens_tv/series/1/s01e01/360p/index.m3u8', 'ready')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9002, 1, '480p', 1000, 854, 480, 'dens_tv/series/1/s01e02/480p/index.m3u8', 'pending')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9003, 1, '480p', 900, 854, 480, 'dens_tv/movies/2/480p/index.m3u8', 'ready')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
	            playlist_path, status)
	        VALUES (9004, 1, '480p', 900, 854, 480, 'dens_tv/movies/3/480p/index.m3u8', 'failed')`)

	// Subtitle: satu siap, satu gagal.
	f.exec(`INSERT INTO subtitles (playable_id, generation_id, lang, label, format, file_path, bytes, status)
	        VALUES (9001, 1, 'id', 'Indonesia', 'vtt', 'dens_tv/series/1/s01e01/subtitle-id.vtt', 1024, 'ready')`)
	f.exec(`INSERT INTO subtitles (playable_id, generation_id, lang, label, format, file_path, bytes, status)
	        VALUES (9001, 1, 'en', 'English', 'vtt', 'dens_tv/series/1/s01e01/subtitle-en.vtt', 1024, 'failed')`)
}

// envelope adalah bentuk response standar yang dipakai seluruh test.
type envelope struct {
	Code   int               `json:"code"`
	Msg    string            `json:"msg"`
	Data   json.RawMessage   `json:"data"`
	Errors []response.Detail `json:"errors"`
}

// request menjalankan satu request HTTP ke router.
func (f *fixture) request(method, target, token string, body any) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// do menjalankan request dan mendekode envelope-nya. Test gagal bila body bukan
// envelope valid atau bila errors bernilai null (R4).
func (f *fixture) do(method, target, token string, body any) (envelope, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.request(method, target, token, body)

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		f.t.Fatalf("%s %s: body bukan envelope JSON: %v (body=%q)", method, target, err, rec.Body.String())
	}
	if env.Errors == nil {
		f.t.Fatalf("%s %s: errors bernilai null (R4): %s", method, target, rec.Body.String())
	}
	return env, rec
}

// decode memastikan data envelope bisa dipetakan ke dst.
func (f *fixture) decode(env envelope, dst any) {
	f.t.Helper()
	if len(env.Data) == 0 {
		f.t.Fatalf("data envelope kosong")
	}
	if err := json.Unmarshal(env.Data, dst); err != nil {
		f.t.Fatalf("decode data: %v (data=%s)", err, string(env.Data))
	}
}

// registerDevice mendaftarkan tablet dan mengembalikan token serta ID-nya.
func (f *fixture) registerDevice(name, mac string) (string, int64) {
	f.t.Helper()

	env, rec := f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"name":        name,
		"seat_number": "7",
		"mac_address": mac,
		"app_version": "1.0.3",
	})
	if rec.Code != http.StatusCreated || env.Code != response.CodeSuccess {
		f.t.Fatalf("register: status=%d code=%d body=%s", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
		Token string `json:"token"`
	}
	f.decode(env, &data)
	if data.Token == "" {
		f.t.Fatalf("register: token kosong")
	}
	return data.Token, data.Device.ID
}

// hashToken menghitung SHA-256 hex seperti yang disimpan server.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// listMeta adalah data.meta endpoint list.
type listMeta struct {
	Page       int64 `json:"page"`
	PerPage    int64 `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int64 `json:"total_pages"`
}
