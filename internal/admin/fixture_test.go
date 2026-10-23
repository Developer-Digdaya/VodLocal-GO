package admin_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"vodlocal/internal/admin"
	"vodlocal/internal/catalog"
	"vodlocal/internal/httpserver"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
	"vodlocal/internal/syncjob"
	"vodlocal/internal/transfer"
)

// Kredensial admin yang dipakai seluruh test.
const (
	testUsername = "admin"
	testPassword = "rahasia-sekali-123"
)

// fixture adalah satu database SQLite asli (store.Open + Migrate di t.TempDir)
// dengan panel admin terpasang pada router chi.
type fixture struct {
	t        *testing.T
	store    *store.Store
	db       *sql.DB
	now      time.Time
	handler  http.Handler
	admin    *admin.Admin
	sessions *httpserver.SessionManager
	syncer   *stubSyncer
	media    *stubMedia
	transfer *transfer.Service
	lister   *stubLister
	ingest   *stubIngest
	// mediaRoot dan artworkRoot adalah direktori sementara yang dipakai uji
	// penghapusan storage dan aksi "Remove all batch".
	mediaRoot   string
	artworkRoot string
	// jar adalah cookie jar sederhana: nama cookie → cookie terakhir.
	jar map[string]*http.Cookie
}

// stubSyncer mencatat panggilan Run/Cancel tanpa menyentuh database.
type stubSyncer struct {
	mu        sync.Mutex
	triggers  []string
	cancelled []int64
	started   chan string
	err       error
}

func (s *stubSyncer) Run(_ context.Context, trigger string) (syncjob.Result, error) {
	s.mu.Lock()
	s.triggers = append(s.triggers, trigger)
	s.mu.Unlock()

	select {
	case s.started <- trigger:
	default:
	}
	return syncjob.Result{RunID: 99}, s.err
}

func (s *stubSyncer) Cancel(runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, runID)
	return s.err
}

func (s *stubSyncer) triggersSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.triggers))
	copy(out, s.triggers)
	return out
}

func (s *stubSyncer) cancelledRuns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.cancelled))
	copy(out, s.cancelled)
	return out
}

// stubMedia mencatat label yang dihapus dan — bila root diisi — benar-benar
// menghapus direktori generation di bawahnya, sehingga test bisa memeriksa isi
// disk setelahnya.
type stubMedia struct {
	mu      sync.Mutex
	root    string
	removed []string
	err     error
}

func (m *stubMedia) Remove(label string) error {
	m.mu.Lock()
	m.removed = append(m.removed, label)
	m.mu.Unlock()

	if m.err != nil {
		return m.err
	}
	if m.root == "" {
		return nil
	}
	// Perilaku mediafs.Manager.Remove: direktori yang tidak ada bukan kegagalan
	// untuk pemanggil panel (isMissingFile menanganinya).
	return os.RemoveAll(filepath.Join(m.root, label))
}

// Root mengembalikan akar media yang dipakai aksi "Remove all batch".
func (m *stubMedia) Root() string { return m.root }

func (m *stubMedia) removedLabels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// stubLister menggantikan catalog.SourceSet pada modal "List Content".
// Perilaku per kategori dan halaman diatur lewat fn agar test bisa menguji
// pagination maupun pemisahan kategori (movie/series/short).
type stubLister struct {
	mu         sync.Mutex
	pages      []int
	categories []string
	fn         func(category string, page int) (catalog.SeriesPage, error)
}

func (l *stubLister) ListSeries(_ context.Context, category string, page int) (catalog.SeriesPage, error) {
	l.mu.Lock()
	l.pages = append(l.pages, page)
	l.categories = append(l.categories, category)
	fn := l.fn
	l.mu.Unlock()

	if fn == nil {
		return catalog.SeriesPage{Page: page, PerPage: 20, Total: 0, LastPage: 1}, nil
	}
	return fn(category, page)
}

func (l *stubLister) pagesSeen() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int, len(l.pages))
	copy(out, l.pages)
	return out
}

func (l *stubLister) categoriesSeen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.categories))
	copy(out, l.categories)
	return out
}

// stubIngest mencatat setiap pemanggilan Start tanpa menjalankan worker.
type stubIngest struct {
	mu         sync.Mutex
	selections [][]catalog.Series
	runID      int64
	err        error
}

func (s *stubIngest) Start(_ context.Context, selections []catalog.Series) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selections = append(s.selections, append([]catalog.Series(nil), selections...))
	if s.err != nil {
		return 0, s.err
	}
	if s.runID == 0 {
		return 77, nil
	}
	return s.runID, nil
}

func (s *stubIngest) calls() [][]catalog.Series {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]catalog.Series, len(s.selections))
	copy(out, s.selections)
	return out
}

// fixtureOption menyesuaikan Config panel sebelum admin.New dipanggil.
type fixtureOption func(*admin.Config)

// withoutContent memasang panel tanpa SeriesLister dan IngestStarter sehingga
// route /content tidak ikut ter-mount.
func withoutContent() fixtureOption {
	return func(cfg *admin.Config) {
		cfg.Lister = nil
		cfg.Ingest = nil
	}
}

// withoutArtworkRoot memasang panel tanpa akar artwork, sehingga aksi "Remove
// all batch" tidak menyentuh artwork sama sekali.
func withoutArtworkRoot() fixtureOption {
	return func(cfg *admin.Config) {
		cfg.ArtworkRoot = ""
	}
}

// newFixture menyiapkan database, migrasi, data contoh, dan panel admin.
func newFixture(t *testing.T, opts ...fixtureOption) *fixture {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "admin-test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	f := &fixture{
		t:           t,
		store:       st,
		db:          st.Write(),
		now:         time.Date(2026, 10, 20, 11, 30, 0, 0, time.UTC),
		mediaRoot:   t.TempDir(),
		artworkRoot: t.TempDir(),
		jar:         make(map[string]*http.Cookie),
	}
	f.seed()

	f.sessions = httpserver.NewSessionManager([]byte("test-secret-0123456789"), time.Hour)
	f.syncer = &stubSyncer{started: make(chan string, 4)}
	f.media = &stubMedia{root: f.mediaRoot}
	f.lister = &stubLister{}
	f.ingest = &stubIngest{}
	mediaManager, err := mediafs.New(f.mediaRoot)
	if err != nil {
		t.Fatalf("mediafs.New: %v", err)
	}
	transferSvc, err := transfer.New(transfer.Config{
		Store:  st,
		Media:  mediaManager,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("transfer.New: %v", err)
	}
	f.transfer = transferSvc

	cfg := admin.Config{
		Store:       st,
		Syncer:      f.syncer,
		Media:       f.media,
		Sessions:    f.sessions,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return f.now },
		Version:     "test",
		Lister:      f.lister,
		Ingest:      f.ingest,
		ArtworkRoot: f.artworkRoot,
		Transfer:    f.transfer,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	panel, err := admin.New(cfg)
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	if err := panel.SeedAdmin(t.Context(), testUsername, testPassword); err != nil {
		t.Fatalf("SeedAdmin: %v", err)
	}
	f.admin = panel

	router := chi.NewRouter()
	panel.Mount(router)
	f.handler = router
	return f
}

// exec menjalankan satu perintah SQL seed/uji.
func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("exec gagal: %v\nquery: %s", err, query)
	}
}

// seed mengisi generation, katalog, perangkat, dan riwayat sync.
func (f *fixture) seed() {
	f.exec(`INSERT INTO providers (id, code, name) VALUES (1, 'dens_tv', 'Dens TV')`)

	// Generation: failed, retired, dan active.
	f.exec(`INSERT INTO generations (id, label, root_path, status, file_count, total_bytes, created_at)
	        VALUES (40, 'gen-000040', '/srv/media/generations/gen-000040', 'failed', 0, 0, '2026-10-01T00:00:00Z')`)
	f.exec(`INSERT INTO generations (id, label, root_path, status, file_count, total_bytes, created_at, activated_at, retired_at)
	        VALUES (41, 'gen-000041', '/srv/media/generations/gen-000041', 'retired', 10, 1048576,
	                '2026-10-05T00:00:00Z', '2026-10-05T00:00:00Z', '2026-10-10T00:00:00Z')`)
	f.exec(`INSERT INTO generations (id, label, root_path, status, file_count, total_bytes, created_at, activated_at)
	        VALUES (42, 'gen-000042', '/srv/media/generations/gen-000042', 'active', 120, 5242880,
	                '2026-10-15T00:00:00Z', '2026-10-15T00:00:00Z')`)

	// Katalog minimal: satu series dengan dua episode + satu movie.
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (1, 1, 'ext-1', 'series', 'Cinta Pertama', 'cinta-pertama')`)
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, duration_seconds)
	        VALUES (2, 1, 'ext-2', 'movie', 'Film Lama', 'film-lama', 5400)`)
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
	        VALUES (301, 1, 'ep-301', 1, 1, 'Awal')`)
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
	        VALUES (302, 1, 'ep-302', 1, 2, 'Kedua')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9001, 1, 301, 2700)`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9002, 1, 302, 2600)`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9003, 2, NULL, 5400)`)

	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9001, 42, '480p', 'ready', 'dens_tv/series/1/s01e01/480p/index.m3u8')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9001, 42, '360p', 'ready', 'dens_tv/series/1/s01e01/360p/index.m3u8')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9002, 42, '480p', 'pending', 'dens_tv/series/1/s01e02/480p/index.m3u8')`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9003, 42, '480p', 'failed', 'dens_tv/movies/2/480p/index.m3u8')`)

	// Perangkat: satu aktif (online), satu nonaktif.
	f.exec(`INSERT INTO devices (id, name, seat_number, mac_address, ip_address, token_hash, is_active, last_seen_at, created_at)
	        VALUES (1, 'Tablet Kursi 12', '12', 'AA:BB:CC:DD:EE:12', '192.0.2.12', ?, 1, '2026-10-20T11:28:00Z', '2026-10-01T00:00:00Z')`,
		hashToken("mc_token-lama-perangkat-12"))
	f.exec(`INSERT INTO devices (id, name, seat_number, mac_address, ip_address, token_hash, is_active, created_at)
	        VALUES (2, 'Tablet Kursi 13', '13', 'AA:BB:CC:DD:EE:13', '192.0.2.13', ?, 0, '2026-10-01T00:00:00Z')`,
		hashToken("mc_token-lama-perangkat-13"))

	// Riwayat sync: satu run selesai (4/10 rendition siap, tanpa kegagalan).
	f.exec(`INSERT INTO sync_runs (id, generation_id, trigger_source, scope, status, phase, triggered_by,
	                               total_renditions, renditions_ready, renditions_failed, bytes_downloaded,
	                               started_at, finished_at, created_at)
	        VALUES (1, 42, 'manual', 'full', 'completed', 'done', 'admin', 10, 4, 0, 1048576,
	                '2026-10-19T01:00:00Z', '2026-10-19T02:00:00Z', '2026-10-19T01:00:00Z')`)
}

// do menjalankan satu request HTTP dan memperbarui cookie jar.
func (f *fixture) do(method, target string, form url.Values, accept string) *httptest.ResponseRecorder {
	f.t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for _, cookie := range f.jar {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	for _, cookie := range rec.Result().Cookies() {
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(f.jar, cookie.Name)
			continue
		}
		f.jar[cookie.Name] = &http.Cookie{Name: cookie.Name, Value: cookie.Value}
	}
	return rec
}

// get menjalankan GET.
func (f *fixture) get(target string) *httptest.ResponseRecorder {
	return f.do(http.MethodGet, target, nil, "")
}

// post menjalankan POST dengan menambahkan _csrf dari cookie bila belum ada.
func (f *fixture) post(target string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if _, ok := form["_csrf"]; !ok {
		form.Set("_csrf", f.csrf())
	}
	return f.do(http.MethodPost, target, form, "")
}

// postRaw menjalankan POST apa adanya, tanpa menambahkan _csrf.
func (f *fixture) postRaw(target string, form url.Values) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, target, form, "")
}

// csrf mengembalikan nilai cookie CSRF yang sedang dipegang klien.
func (f *fixture) csrf() string {
	f.t.Helper()
	cookie, ok := f.jar[httpserver.CSRFCookieName]
	if !ok {
		f.t.Fatalf("cookie CSRF belum ada; buka halaman lebih dulu")
	}
	return cookie.Value
}

// login memasukkan sesi admin ke cookie jar.
func (f *fixture) login() {
	f.t.Helper()

	f.get("/login")
	rec := f.post("/login", url.Values{
		"username": {testUsername},
		"password": {testPassword},
	})
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("login: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := f.jar[httpserver.SessionCookieName]; !ok {
		f.t.Fatalf("login: cookie %s tidak dikirim", httpserver.SessionCookieName)
	}
}

// hasSession melaporkan apakah cookie jar memegang sesi admin.
func (f *fixture) hasSession() bool {
	_, ok := f.jar[httpserver.SessionCookieName]
	return ok
}

// hashToken menghitung SHA-256 hex seperti yang disimpan server.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
