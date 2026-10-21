package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vodlocal/internal/catalog"
	"vodlocal/internal/download"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

// ---------------------------------------------------------------- helpers ---

// Identifier provider yang dipakai hampir seluruh skenario. ID berbentuk UUID
// supaya path media yang diharapkan benar-benar memakai ID, bukan slug.
const (
	testSeriesID    = "3f8a1c2e-5b4d-4e6f-8a9b-0c1d2e3f4a5b"
	testSeriesSlug  = "series-uji"
	testEpisodeID   = "7d2b9e10-4c6a-4b8d-9f3e-1a2b3c4d5e6f"
	testEpisodeSlug = "ep-satu"
)

// seriesPlaylistPath menyusun path playlist relatif terhadap root generation.
func seriesPlaylistPath(seriesDir, episodeDir, quality string) string {
	return "dramahits/series/" + seriesDir + "/" + episodeDir + "/" + quality + "/index.m3u8"
}

// seriesPosterPath menyusun path poster relatif terhadap root generation.
func seriesPosterPath(seriesDir string) string {
	return "dramahits/series/" + seriesDir + "/poster.jpg"
}

// newTestStore membuka SQLite sungguhan di t.TempDir() dan menjalankan migrasi.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("buka store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrasi: %v", err)
	}
	return st
}

// testEnv mengumpulkan komponen yang dipakai satu skenario uji.
type testEnv struct {
	svc     *Service
	store   *store.Store
	media   *mediafs.Manager
	root    string
	artwork string
}

// newEnv menyiapkan Service lengkap dengan media root dan artwork root di
// t.TempDir(). client boleh nil. opts, bila ada, dipanggil berurutan untuk
// menyesuaikan Config sebelum Service dibuat (mis. MaxEpisodes).
func newEnv(t *testing.T, src catalog.Source, client *http.Client, opts ...func(*Config)) *testEnv {
	t.Helper()

	st := newTestStore(t)

	root := filepath.Join(t.TempDir(), "media")
	media, err := mediafs.New(root)
	if err != nil {
		t.Fatalf("mediafs.New: %v", err)
	}

	artwork := filepath.Join(t.TempDir(), "artwork")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := Config{
		Store:       st,
		Media:       media,
		Source:      src,
		Logger:      logger,
		ArtworkRoot: artwork,
	}
	if client != nil {
		cfg.Downloader = download.New(download.Options{Client: client, Retries: 0})
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}

	return &testEnv{svc: svc, store: st, media: media, root: root, artwork: artwork}
}

// fakeSource adalah catalog.Source in-memory. gate, bila tidak nil, membuat
// ListEpisodes menunggu sampai channel ditutup.
type fakeSource struct {
	code  string
	pages map[string][]catalog.EpisodePage
	gate  chan struct{}

	mu   sync.Mutex
	seen map[string][]int
}

func (f *fakeSource) Code() string { return f.code }

func (f *fakeSource) ListSeries(context.Context, int) (catalog.SeriesPage, error) {
	return catalog.SeriesPage{}, nil
}

func (f *fakeSource) ListEpisodes(ctx context.Context, seriesSlug string, page int) (catalog.EpisodePage, error) {
	f.mu.Lock()
	if f.seen == nil {
		f.seen = make(map[string][]int)
	}
	f.seen[seriesSlug] = append(f.seen[seriesSlug], page)
	f.mu.Unlock()

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return catalog.EpisodePage{}, ctx.Err()
		}
	}

	pages := f.pages[seriesSlug]
	if page < 1 || page > len(pages) {
		return catalog.EpisodePage{Page: page, LastPage: len(pages)}, nil
	}
	return pages[page-1], nil
}

// requestedPages mengembalikan halaman yang diminta untuk seriesSlug.
func (f *fakeSource) requestedPages(seriesSlug string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.seen[seriesSlug]...)
}

// playlistServer menyajikan master playlist dua varian (540x960 dan 640x360),
// media playlist tiga segmen, segmen, dan poster. Semua permintaan dicatat.
type playlistServer struct {
	*httptest.Server

	mu   sync.Mutex
	hits map[string]int
}

func (p *playlistServer) record(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits[path]++
}

func (p *playlistServer) hit(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

// newHLSServer menjalankan server uji HLS.
func newHLSServer(t *testing.T) *playlistServer {
	t.Helper()

	ps := &playlistServer{hits: make(map[string]int)}

	const master = "#EXTM3U\n#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=540x960\nv540/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nv360/index.m3u8\n"

	media := func() string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ps.record(r.URL.Path)

		switch {
		case r.URL.Path == "/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, master)
		case strings.HasSuffix(r.URL.Path, "/index.m3u8"):
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, media())
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(bytes.Repeat([]byte{0x47}, 188))
		case r.URL.Path == "/art/poster.jpg":
			_, _ = io.WriteString(w, "poster-bytes")
		default:
			http.NotFound(w, r)
		}
	})

	ps.Server = httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return ps
}

// selection mengembalikan satu series Dramahits untuk diuji.
func selection(posterURL string) []catalog.Series {
	return []catalog.Series{{
		Provider:    "dramahits",
		Category:    catalog.CategoryShort,
		ID:          testSeriesID,
		Slug:        testSeriesSlug,
		Title:       "Series Uji",
		Description: "Sinopsis series uji.",
		PosterURL:   posterURL,
		Year:        2024,
		AgeRating:   "13+",
	}}
}

// episodePages membangun satu halaman berisi satu episode.
func episodePages(streamURL string) map[string][]catalog.EpisodePage {
	return map[string][]catalog.EpisodePage{
		testSeriesSlug: {{
			Items: []catalog.Episode{{
				ID:              testEpisodeID,
				Slug:            testEpisodeSlug,
				Title:           "Episode Satu",
				Season:          1,
				Number:          1,
				DurationSeconds: 12,
				Streams: []catalog.Stream{
					{Device: "ios", URL: streamURL + "/v360/index.m3u8"},
					{Device: "android", URL: streamURL + "/master.m3u8"},
				},
			}},
			Page:     1,
			PerPage:  20,
			Total:    1,
			LastPage: 1,
		}},
	}
}

// waitRun menunggu sampai run mencapai status terminal dan mengembalikannya.
func waitRun(t *testing.T, st *store.Store, runID int64) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := st.Read().QueryRow(`SELECT status FROM sync_runs WHERE id = ?`, runID).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			t.Fatalf("baca status run %d: %v", runID, err)
		case status == "completed", status == "failed", status == "cancelled":
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("run %d tidak mencapai status terminal dalam 20 detik", runID)
	return ""
}

// queryString membaca satu kolom string.
func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// queryNullString membaca satu kolom string yang boleh NULL.
func queryNullString(t *testing.T, db *sql.DB, query string, args ...any) (string, bool) {
	t.Helper()

	var ns sql.NullString
	if err := db.QueryRow(query, args...).Scan(&ns); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return ns.String, ns.Valid
}

// queryInt membaca satu kolom integer.
func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()

	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// mustExist memastikan path ada.
func mustExist(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("berkas %s tidak ada: %v", path, err)
	}
}

// stagingDirs mengembalikan direktori staging yang tersisa di root media.
func stagingDirs(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("baca root media: %v", err)
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".building-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// --------------------------------------------------------------- skenario ---

// TestIngestHappyPath memastikan satu series dengan satu episode menghasilkan
// generation aktif: varian 640x360 (bukan 540x960) dipilih, playlist dan
// segmennya ada di disk, dan seluruh baris database terisi.
func TestIngestHappyPath(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	sels := selection(srv.URL + "/art/poster.jpg")

	runID, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runID == 0 {
		t.Fatal("runID = 0, mau > 0")
	}

	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	// Generation aktif.
	if got := queryString(t, env.store.Read(),
		`SELECT label FROM generations WHERE status = 'active'`); got != "gen-000001" {
		t.Errorf("generation aktif = %q, mau gen-000001", got)
	}
	if got := queryInt(t, env.store.Read(),
		`SELECT file_count FROM generations WHERE label = 'gen-000001'`); got <= 0 {
		t.Errorf("generations.file_count = %d, mau > 0", got)
	}

	// Rendition: varian kecil yang dipilih, bukan yang portrait 540x960.
	if got := queryString(t, env.store.Read(), `SELECT quality FROM renditions`); got != "360p" {
		t.Errorf("renditions.quality = %q, mau 360p", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT width FROM renditions`); got != 640 {
		t.Errorf("renditions.width = %d, mau 640", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT height FROM renditions`); got != 360 {
		t.Errorf("renditions.height = %d, mau 360", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM renditions`); got != "ready" {
		t.Errorf("renditions.status = %q, mau ready", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT container FROM renditions`); got != "ts" {
		t.Errorf("renditions.container = %q, mau ts", got)
	}

	// Folder media memakai UUID provider, bukan slug/sNNENN.
	wantPlaylist := seriesPlaylistPath(testSeriesID, testEpisodeID, "360p")
	if got := queryString(t, env.store.Read(), `SELECT playlist_path FROM renditions`); got != wantPlaylist {
		t.Errorf("renditions.playlist_path = %q, mau %q", got, wantPlaylist)
	}
	if got := queryInt(t, env.store.Read(), `SELECT segment_count FROM renditions`); got != 3 {
		t.Errorf("renditions.segment_count = %d, mau 3", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT segments_done FROM renditions`); got != 3 {
		t.Errorf("renditions.segments_done = %d, mau 3", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT total_bytes FROM renditions`); got <= 0 {
		t.Errorf("renditions.total_bytes = %d, mau > 0", got)
	}
	if _, ok := queryNullString(t, env.store.Read(), `SELECT verified_at FROM renditions`); !ok {
		t.Error("renditions.verified_at NULL, mau terisi")
	}
	if got := queryString(t, env.store.Read(), `SELECT source_url FROM renditions`); !strings.HasSuffix(got, "/v360/index.m3u8") {
		t.Errorf("renditions.source_url = %q, mau berakhir /v360/index.m3u8", got)
	}

	// Berkas di disk.
	genDir := filepath.Join(env.root, "gen-000001")
	playlistFile := filepath.Join(genDir, filepath.FromSlash(wantPlaylist))
	mustExist(t, playlistFile)
	for i := 1; i <= 3; i++ {
		mustExist(t, filepath.Join(filepath.Dir(playlistFile), fmt.Sprintf("seg-%05d.ts", i)))
	}

	body, err := os.ReadFile(playlistFile)
	if err != nil {
		t.Fatalf("baca playlist: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if name := fmt.Sprintf("seg-%05d.ts", i); !bytes.Contains(body, []byte(name)) {
			t.Errorf("playlist lokal tidak memuat %s", name)
		}
	}
	if bytes.Contains(body, []byte("http")) {
		t.Errorf("playlist lokal masih memuat URL absolut:\n%s", body)
	}

	// Varian besar tidak pernah diunduh.
	if n := srv.hit("/v540/index.m3u8"); n != 0 {
		t.Errorf("varian 540x960 diunduh %d kali, mau 0", n)
	}
	if n := srv.hit("/v360/index.m3u8"); n != 1 {
		t.Errorf("varian 640x360 diunduh %d kali, mau 1", n)
	}

	// Run selesai beserta counter-nya.
	if got := queryString(t, env.store.Read(), `SELECT status FROM sync_runs WHERE id = ?`, runID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT trigger_source || '/' || scope FROM sync_runs WHERE id = ?`, runID); got != "manual/selection" {
		t.Errorf("trigger/scope = %q, mau manual/selection", got)
	}
	if _, ok := queryNullString(t, env.store.Read(), `SELECT finished_at FROM sync_runs WHERE id = ?`, runID); !ok {
		t.Error("sync_runs.finished_at NULL, mau terisi")
	}
	if got := queryInt(t, env.store.Read(), `SELECT total_titles FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("total_titles = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT total_playables FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("total_playables = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT total_renditions FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("total_renditions = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("renditions_ready = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_failed FROM sync_runs WHERE id = ?`, runID); got != 0 {
		t.Errorf("renditions_failed = %d, mau 0", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT bytes_downloaded FROM sync_runs WHERE id = ?`, runID); got <= 0 {
		t.Errorf("bytes_downloaded = %d, mau > 0", got)
	}

	// Katalog.
	if got := queryString(t, env.store.Read(), `SELECT code || '/' || name FROM providers`); got != "dramahits/Dramahits" {
		t.Errorf("providers = %q, mau dramahits/Dramahits", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT external_id FROM titles`); got != "series-uji" {
		t.Errorf("titles.external_id = %q, mau series-uji", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT kind FROM titles`); got != "series" {
		t.Errorf("titles.kind = %q, mau series", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT title FROM titles`); got != "Series Uji" {
		t.Errorf("titles.title = %q, mau Series Uji", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT slug FROM titles`); got != "series-uji" {
		t.Errorf("titles.slug = %q, mau series-uji", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT synopsis FROM titles`); got != "Sinopsis series uji." {
		t.Errorf("titles.synopsis = %q", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT release_year FROM titles`); got != 2024 {
		t.Errorf("titles.release_year = %d, mau 2024", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT content_rating FROM titles`); got != "13+" {
		t.Errorf("titles.content_rating = %q, mau 13+", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT is_published FROM titles`); got != 1 {
		t.Errorf("titles.is_published = %d, mau 1", got)
	}
	// Poster disimpan di dalam generation, satu folder dengan media series.
	wantPoster := seriesPosterPath(testSeriesID)
	if got := queryString(t, env.store.Read(), `SELECT poster_path FROM titles`); got != wantPoster {
		t.Errorf("titles.poster_path = %q, mau %q", got, wantPoster)
	}
	mustExist(t, filepath.Join(genDir, filepath.FromSlash(wantPoster)))

	// ArtworkRoot tidak lagi dipakai ingest: tidak ada berkas poster di sana.
	if _, err := os.Stat(filepath.Join(env.artwork, "dramahits")); !os.IsNotExist(err) {
		t.Errorf("ArtworkRoot masih dipakai ingest, mau tidak ada (err=%v)", err)
	}
	// Folder series memakai UUID; tidak ada folder slug yang dibuat.
	if _, err := os.Stat(filepath.Join(genDir, "dramahits", "series", testSeriesSlug)); !os.IsNotExist(err) {
		t.Errorf("folder slug %q ada, mau hanya UUID (err=%v)", testSeriesSlug, err)
	}

	if got := queryString(t, env.store.Read(), `SELECT external_id FROM episodes`); got != "ep-satu" {
		t.Errorf("episodes.external_id = %q, mau ep-satu", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT season_number FROM episodes`); got != 1 {
		t.Errorf("episodes.season_number = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT episode_number FROM episodes`); got != 1 {
		t.Errorf("episodes.episode_number = %d, mau 1", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT name FROM episodes`); got != "Episode Satu" {
		t.Errorf("episodes.name = %q, mau Episode Satu", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT duration_seconds FROM episodes`); got != 12 {
		t.Errorf("episodes.duration_seconds = %d, mau 12", got)
	}

	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM playables WHERE episode_id IS NOT NULL`); got != 1 {
		t.Errorf("playables per episode = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT duration_seconds FROM playables`); got != 12 {
		t.Errorf("playables.duration_seconds = %d, mau 12", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT is_encrypted FROM playables`); got != 0 {
		t.Errorf("playables.is_encrypted = %d, mau 0", got)
	}

	if dirs := stagingDirs(t, env.root); len(dirs) != 0 {
		t.Errorf("staging tersisa: %v, mau kosong", dirs)
	}
}

// TestIngestMediaPathUsesProviderIDs memastikan folder media memakai UUID dari
// provider untuk series maupun episode (bukan slug/sNNENN), dan bahwa poster
// tersimpan di dalam generation yang sama dengan path relatif terhadap root
// generation.
func TestIngestMediaPathUsesProviderIDs(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	runID, err := env.svc.Start(context.Background(), selection(srv.URL+"/art/poster.jpg"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	wantPlaylist := seriesPlaylistPath(testSeriesID, testEpisodeID, "360p")
	if got := queryString(t, env.store.Read(), `SELECT playlist_path FROM renditions`); got != wantPlaylist {
		t.Errorf("renditions.playlist_path = %q, mau %q", got, wantPlaylist)
	}

	wantPoster := seriesPosterPath(testSeriesID)
	if got := queryString(t, env.store.Read(), `SELECT poster_path FROM titles`); got != wantPoster {
		t.Errorf("titles.poster_path = %q, mau %q", got, wantPoster)
	}

	// Poster dan playlist berada di generation yang sama, relatif terhadap
	// root generation (bisa disajikan lewat /media/<label>/<path>).
	genDir := filepath.Join(env.root, "gen-000001")
	playlistFile := filepath.Join(genDir, filepath.FromSlash(wantPlaylist))
	mustExist(t, filepath.Join(genDir, filepath.FromSlash(wantPoster)))
	mustExist(t, playlistFile)
	for i := 1; i <= 3; i++ {
		mustExist(t, filepath.Join(filepath.Dir(playlistFile), fmt.Sprintf("seg-%05d.ts", i)))
	}

	// Path langsung yang memakai UUID ada; path berbasis slug tidak dibuat.
	mustExist(t, filepath.Join(genDir, "dramahits", "series", testSeriesID, posterFileName))
	mustExist(t, filepath.Join(genDir, "dramahits", "series", testSeriesID, testEpisodeID, "360p", playlistFileName))
	if _, err := os.Stat(filepath.Join(genDir, "dramahits", "series", testSeriesSlug)); !os.IsNotExist(err) {
		t.Errorf("folder series %q ada, mau hanya UUID (err=%v)", testSeriesSlug, err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "dramahits", "series", testSeriesID, "s01e01")); !os.IsNotExist(err) {
		t.Errorf("folder episode s01e01 ada, mau UUID episode (err=%v)", err)
	}
}

// TestIngestPathFallsBackWithoutIDs memastikan fallback path tetap berlaku:
// series tanpa ID memakai slug, dan episode tanpa ID memakai sNNENN.
func TestIngestPathFallsBackWithoutIDs(t *testing.T) {
	srv := newHLSServer(t)

	// Episode tanpa ID: nama foldernya diturunkan dari season/nomor.
	episodeTanpaID := func(number int) catalog.Episode {
		return catalog.Episode{
			Title:           fmt.Sprintf("Episode %d", number),
			Season:          1,
			Number:          number,
			DurationSeconds: 12,
			Streams:         []catalog.Stream{{Device: "android", URL: srv.URL + "/master.m3u8"}},
		}
	}

	src := &fakeSource{code: "dramahits", pages: map[string][]catalog.EpisodePage{
		"series-tanpa-id": {{
			Items:    []catalog.Episode{episodeTanpaID(1)},
			Page:     1,
			PerPage:  20,
			Total:    1,
			LastPage: 1,
		}},
		testSeriesSlug: {{
			Items:    []catalog.Episode{episodeTanpaID(2)},
			Page:     1,
			PerPage:  20,
			Total:    1,
			LastPage: 1,
		}},
	}}
	env := newEnv(t, src, srv.Client())

	sels := []catalog.Series{
		{
			Provider:  "dramahits",
			Category:  catalog.CategoryShort,
			Slug:      "series-tanpa-id",
			Title:     "Series Tanpa ID",
			PosterURL: srv.URL + "/art/poster.jpg",
		},
		{
			Provider:  "dramahits",
			Category:  catalog.CategoryShort,
			ID:        testSeriesID,
			Slug:      testSeriesSlug,
			Title:     "Series Uji",
			PosterURL: srv.URL + "/art/poster.jpg",
		},
	}

	runID, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 2 {
		t.Errorf("renditions_ready = %d, mau 2", got)
	}

	// seriesPlaylistFor membaca playlist rendition milik titles.external_id.
	seriesPlaylistFor := func(externalID string) string {
		return queryString(t, env.store.Read(), `
SELECT r.playlist_path FROM renditions r
  JOIN playables p ON p.id = r.playable_id
  JOIN episodes e ON e.id = p.episode_id
  JOIN titles t ON t.id = e.title_id
 WHERE t.external_id = ?`, externalID)
	}

	cases := []struct {
		name       string
		externalID string
		wantPoster string
		wantPlay   string
	}{
		{
			name:       "series dan episode tanpa ID",
			externalID: "series-tanpa-id",
			wantPoster: seriesPosterPath("series-tanpa-id"),
			wantPlay:   seriesPlaylistPath("series-tanpa-id", "s01e01", "360p"),
		},
		{
			name:       "series ber-UUID, episode tanpa ID",
			externalID: testSeriesSlug,
			wantPoster: seriesPosterPath(testSeriesID),
			wantPlay:   seriesPlaylistPath(testSeriesID, "s01e02", "360p"),
		},
	}

	genDir := filepath.Join(env.root, "gen-000001")
	for _, c := range cases {
		if got := seriesPlaylistFor(c.externalID); got != c.wantPlay {
			t.Errorf("%s: playlist_path = %q, mau %q", c.name, got, c.wantPlay)
		}
		if got := queryString(t, env.store.Read(),
			`SELECT poster_path FROM titles WHERE external_id = ?`, c.externalID); got != c.wantPoster {
			t.Errorf("%s: titles.poster_path = %q, mau %q", c.name, got, c.wantPoster)
		}
		mustExist(t, filepath.Join(genDir, filepath.FromSlash(c.wantPlay)))
		mustExist(t, filepath.Join(genDir, filepath.FromSlash(c.wantPoster)))
	}
}

// TestStartRejectsConcurrentRun memastikan Start kedua ditolak selama run
// pertama masih berjalan.
func TestStartRejectsConcurrentRun(t *testing.T) {
	srv := newHLSServer(t)
	gate := make(chan struct{})
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL), gate: gate}
	env := newEnv(t, src, srv.Client())

	sels := selection("")

	firstRun, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start pertama: %v", err)
	}

	if _, err := env.svc.Start(context.Background(), sels); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Start kedua: err = %v, mau ErrAlreadyRunning", err)
	}

	// Run yang ditolak tidak boleh meninggalkan generation tambahan.
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations`); got != 1 {
		t.Errorf("jumlah generation = %d, mau 1", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM sync_runs`); got != 1 {
		t.Errorf("jumlah sync run = %d, mau 1", got)
	}

	close(gate)

	if got := waitRun(t, env.store, firstRun); got != "completed" {
		t.Fatalf("status run pertama = %q, mau completed", got)
	}
}

// TestIngestFailsWhenAllStreamsFail memastikan generation dan run ditandai gagal
// bila tidak ada satu pun rendition yang bisa disiapkan.
func TestIngestFailsWhenAllStreamsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := waitRun(t, env.store, runID); got != "failed" {
		t.Fatalf("sync_runs.status = %q, mau failed", got)
	}

	if got := queryString(t, env.store.Read(), `SELECT status FROM generations`); got != "failed" {
		t.Errorf("generations.status = %q, mau failed", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations WHERE status = 'active'`); got != 0 {
		t.Errorf("ada generation aktif, mau 0")
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM sync_runs WHERE id = ?`, runID); got != "failed" {
		t.Errorf("sync_runs.status = %q, mau failed", got)
	}
	if got, ok := queryNullString(t, env.store.Read(), `SELECT error_message FROM sync_runs WHERE id = ?`, runID); !ok || got == "" {
		t.Error("sync_runs.error_message kosong, mau terisi")
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 0 {
		t.Errorf("renditions_ready = %d, mau 0", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_failed FROM sync_runs WHERE id = ?`, runID); got == 0 {
		t.Error("renditions_failed = 0, mau > 0")
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM renditions`); got != 0 {
		t.Errorf("baris renditions = %d, mau 0", got)
	}

	// Staging dibuang dan tidak ada direktori generation yang terbit.
	if dirs := stagingDirs(t, env.root); len(dirs) != 0 {
		t.Errorf("staging tersisa: %v, mau kosong", dirs)
	}
	if _, err := os.Stat(filepath.Join(env.root, "gen-000001")); !os.IsNotExist(err) {
		t.Errorf("direktori gen-000001 ada, mau tidak ada (err=%v)", err)
	}
}

// TestIngestPosterFailureIsNotFatal memastikan kegagalan unduh poster tidak
// menggagalkan episode maupun generation.
func TestIngestPosterFailureIsNotFatal(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	// URL poster yang tidak dilayani server (404).
	sels := selection(srv.URL + "/art/hilang.jpg")

	runID, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got, ok := queryNullString(t, env.store.Read(), `SELECT poster_path FROM titles`); ok {
		t.Errorf("titles.poster_path = %q, mau NULL", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM generations`); got != "active" {
		t.Errorf("generations.status = %q, mau active", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM renditions`); got != "ready" {
		t.Errorf("renditions.status = %q, mau ready", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("renditions_ready = %d, mau 1", got)
	}
	if _, err := os.Stat(filepath.Join(env.root, "gen-000001", filepath.FromSlash(seriesPosterPath(testSeriesID)))); !os.IsNotExist(err) {
		t.Errorf("berkas poster ada di generation, mau tidak ada (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(env.artwork, "dramahits", testSeriesSlug, "poster.jpg")); !os.IsNotExist(err) {
		t.Errorf("berkas poster ada di ArtworkRoot, mau tidak ada (err=%v)", err)
	}
}

// TestIngestFetchesEveryEpisodePage memastikan seluruh halaman episode diambil
// dan bahwa media playlist tanpa master dipakai apa adanya.
func TestIngestFetchesEveryEpisodePage(t *testing.T) {
	srv := newHLSServer(t)

	episode := func(id, slug string, number int, streamURL string) catalog.Episode {
		return catalog.Episode{
			ID:              id,
			Slug:            slug,
			Title:           "Episode " + slug,
			Season:          1,
			Number:          number,
			DurationSeconds: 12,
			Streams:         []catalog.Stream{{Device: "android", URL: streamURL}},
		}
	}

	src := &fakeSource{code: "dramahits", pages: map[string][]catalog.EpisodePage{
		"series-uji": {
			{
				Items: []catalog.Episode{
					episode("ep1", "ep-1", 1, srv.URL+"/master.m3u8"),
					episode("ep2", "ep-2", 2, srv.URL+"/master.m3u8"),
				},
				Page: 1, PerPage: 2, Total: 3, LastPage: 2,
			},
			{
				// Media playlist langsung, tanpa master: harus dipakai apa adanya.
				Items:    []catalog.Episode{episode("ep3", "ep-3", 3, srv.URL+"/v360/index.m3u8")},
				Page:     2,
				PerPage:  2,
				Total:    3,
				LastPage: 2,
			},
		},
	}}
	env := newEnv(t, src, srv.Client())

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got := fmt.Sprint(src.requestedPages("series-uji")); got != "[1 2]" {
		t.Errorf("halaman diminta = %s, mau [1 2]", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM episodes`); got != 3 {
		t.Errorf("jumlah episode = %d, mau 3", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM renditions WHERE status = 'ready'`); got != 3 {
		t.Errorf("rendition ready = %d, mau 3", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 3 {
		t.Errorf("sync_runs.renditions_ready = %d, mau 3", got)
	}

	// Episode ketiga memakai media playlist langsung sehingga label kualitasnya
	// jatuh ke skema bandwidth.
	if got := queryString(t, env.store.Read(), `
SELECT r.quality FROM renditions r
  JOIN playables p ON p.id = r.playable_id
  JOIN episodes e ON e.id = p.episode_id
 WHERE e.external_id = 'ep-3'`); got != "q0" {
		t.Errorf("quality episode ke-3 = %q, mau q0", got)
	}
}

// TestIngestRetiresPreviousGeneration memastikan ingest kedua menerbitkan
// generation baru dan me-retire generation lama, lalu membiarkannya di disk:
// tidak ada penghapusan otomatis, generation 'retired' tetap ada sampai admin
// menghapusnya.
func TestIngestRetiresPreviousGeneration(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())
	ctx := context.Background()

	first, err := env.svc.Start(ctx, selection(""))
	if err != nil {
		t.Fatalf("Start pertama: %v", err)
	}
	if got := waitRun(t, env.store, first); got != "completed" {
		t.Fatalf("status run pertama = %q, mau completed", got)
	}

	second, err := env.svc.Start(ctx, selection(""))
	if err != nil {
		t.Fatalf("Start kedua: %v", err)
	}
	if second == first {
		t.Fatalf("runID kedua = %d, mau berbeda dari run pertama", second)
	}
	if got := waitRun(t, env.store, second); got != "completed" {
		t.Fatalf("status run kedua = %q, mau completed", got)
	}

	if got := queryString(t, env.store.Read(), `SELECT label FROM generations WHERE status = 'active'`); got != "gen-000002" {
		t.Errorf("generation aktif = %q, mau gen-000002", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM generations WHERE label = 'gen-000001'`); got != "retired" {
		t.Errorf("generation lama = %q, mau retired", got)
	}
	if _, ok := queryNullString(t, env.store.Read(), `SELECT retired_at FROM generations WHERE label = 'gen-000001'`); !ok {
		t.Error("generations.retired_at NULL pada generation lama, mau terisi")
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM sync_runs WHERE status = 'completed'`); got != 2 {
		t.Errorf("jumlah run completed = %d, mau 2", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM renditions WHERE status = 'ready'`); got != 2 {
		t.Errorf("jumlah rendition ready = %d, mau 2", got)
	}

	// Generation lama tidak pernah dihapus otomatis: baris dan medianya tetap
	// ada setelah run berikutnya.
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations WHERE label = 'gen-000001'`); got != 1 {
		t.Errorf("baris gen-000001 = %d, mau 1 (tidak dihapus otomatis)", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations WHERE status = 'retired'`); got != 1 {
		t.Errorf("jumlah generation retired = %d, mau 1", got)
	}
	mustExist(t, filepath.Join(env.root, "gen-000001", "dramahits", "series", testSeriesID, testEpisodeID, "360p", "index.m3u8"))
	mustExist(t, filepath.Join(env.root, "gen-000002", "dramahits", "series", testSeriesID, testEpisodeID, "360p", "index.m3u8"))

	// Tidak ada masa tenggang: generation retired dengan retired_at yang sudah
	// sangat tua pun tetap dipertahankan pada run berikutnya.
	if _, err := env.store.Write().Exec(
		`UPDATE generations SET retired_at = ? WHERE label = 'gen-000001'`,
		"2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set retired_at lama: %v", err)
	}

	third, err := env.svc.Start(ctx, selection(""))
	if err != nil {
		t.Fatalf("Start ketiga: %v", err)
	}
	if got := waitRun(t, env.store, third); got != "completed" {
		t.Fatalf("status run ketiga = %q, mau completed", got)
	}

	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations WHERE label = 'gen-000001'`); got != 1 {
		t.Errorf("baris gen-000001 hilang setelah run ketiga, mau tetap ada")
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM generations WHERE label = 'gen-000001'`); got != "retired" {
		t.Errorf("status gen-000001 = %q, mau retired", got)
	}
	mustExist(t, filepath.Join(env.root, "gen-000001", "dramahits", "series", testSeriesID, testEpisodeID, "360p", "index.m3u8"))
	if got := queryString(t, env.store.Read(), `SELECT label FROM generations WHERE status = 'active'`); got != "gen-000003" {
		t.Errorf("generation aktif = %q, mau gen-000003", got)
	}
}

// TestIngestFallsBackToIOSStream memastikan stream ios dipakai bila tidak ada
// stream android.
func TestIngestFallsBackToIOSStream(t *testing.T) {
	srv := newHLSServer(t)

	src := &fakeSource{code: "dramahits", pages: map[string][]catalog.EpisodePage{
		"series-uji": {{
			Items: []catalog.Episode{{
				ID:     "ep1",
				Slug:   "ep-satu",
				Title:  "Episode Satu",
				Season: 1,
				Number: 1,
				Streams: []catalog.Stream{
					{Device: "web", URL: srv.URL + "/tidak-dipakai.m3u8"},
					{Device: "ios", URL: srv.URL + "/master.m3u8"},
				},
			}},
			Page: 1, PerPage: 20, Total: 1, LastPage: 1,
		}},
	}}
	env := newEnv(t, src, srv.Client())

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT quality FROM renditions`); got != "360p" {
		t.Errorf("renditions.quality = %q, mau 360p", got)
	}
	if n := srv.hit("/tidak-dipakai.m3u8"); n != 0 {
		t.Errorf("stream web diunduh %d kali, mau 0", n)
	}
}

// TestStartValidation memastikan selection kosong dan provider campuran ditolak
// tanpa menyentuh database.
func TestStartValidation(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())
	ctx := context.Background()

	if _, err := env.svc.Start(ctx, nil); !errors.Is(err, ErrNoSelections) {
		t.Errorf("Start(nil) = %v, mau ErrNoSelections", err)
	}

	mixed := selection("")
	mixed = append(mixed, catalog.Series{Provider: "dens_tv", Slug: "lain"})
	if _, err := env.svc.Start(ctx, mixed); !errors.Is(err, ErrMixedProviders) {
		t.Errorf("Start(campuran) = %v, mau ErrMixedProviders", err)
	}

	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations`); got != 0 {
		t.Errorf("jumlah generation = %d, mau 0", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM sync_runs`); got != 0 {
		t.Errorf("jumlah sync run = %d, mau 0", got)
	}
}

// TestIngestSurvivesRequestCancellation memastikan worker tetap berjalan
// setelah context request (yang dipakai Start) dibatalkan oleh handler HTTP.
func TestIngestSurvivesRequestCancellation(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	runID, err := env.svc.Start(ctx, selection(srv.URL+"/art/poster.jpg"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed walau request dibatalkan", got)
	}
}

// TestIngestRemovesFailedRenditionFiles memastikan episode yang gagal (mis.
// segmen tidak bisa diunduh karena disk penuh) tidak meninggalkan berkas
// parsial di generation yang di-commit.
func TestIngestRemovesFailedRenditionFiles(t *testing.T) {
	good := newHLSServer(t)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nv360/index.m3u8\n")
		case r.URL.Path == "/v360/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n"+
				"#EXTINF:4.0,\nseg1.ts\n#EXTINF:4.0,\nseg2.ts\n#EXT-X-ENDLIST\n")
		case strings.HasSuffix(r.URL.Path, ".ts"):
			http.Error(w, "disk penuh", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bad.Close)

	const badEpisodeID = "fail-episode-uuid"
	pages := map[string][]catalog.EpisodePage{
		testSeriesSlug: {{
			Items: []catalog.Episode{
				{
					ID: testEpisodeID, Slug: testEpisodeSlug, Season: 1, Number: 1,
					Streams: []catalog.Stream{{Device: "android", URL: good.URL + "/master.m3u8"}},
				},
				{
					ID: badEpisodeID, Slug: "ep-gagal", Season: 1, Number: 2,
					Streams: []catalog.Stream{{Device: "android", URL: bad.URL + "/master.m3u8"}},
				},
			},
		}},
	}

	src := &fakeSource{code: "dramahits", pages: pages}
	env := newEnv(t, src, good.Client())

	runID, err := env.svc.Start(context.Background(), selection(good.URL+"/art/poster.jpg"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed (satu episode sukses)", got)
	}

	failedDir := filepath.Join(env.root, "gen-000001", "dramahits", "series", testSeriesID, badEpisodeID, "360p")
	if _, err := os.Stat(failedDir); !os.IsNotExist(err) {
		t.Fatalf("folder rendition gagal masih ada di %s (err=%v)", failedDir, err)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM renditions WHERE status = 'failed'`); got != 1 {
		t.Errorf("renditions failed = %d, mau 1", got)
	}
}

// ------------------------------------------------------- kapasitas disk ---

// TestStartRejectsInsufficientStorage memastikan Start ditolak dengan
// ErrInsufficientStorage saat pemakaian disk (95%) sudah melewati ambang
// (90%). Penolakan terjadi sebelum baris database dibuat, jadi tidak boleh ada
// generation, sync_run, atau direktori staging yang tertinggal.
func TestStartRejectsInsufficientStorage(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}

	var measured string
	env := newEnv(t, src, srv.Client(), func(c *Config) {
		c.DiskUsage = func(path string) (int64, int64, error) {
			measured = path
			return 95, 100, nil
		}
		c.StorageBlockPercent = 90
	})

	runID, err := env.svc.Start(context.Background(), selection(srv.URL+"/art/poster.jpg"))
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("Start = %v, mau ErrInsufficientStorage", err)
	}
	if runID != 0 {
		t.Errorf("runID = %d, mau 0 saat ditolak", runID)
	}

	// Pesan menyebut persentase pemakaian dan ambangnya.
	msg := err.Error()
	if !strings.Contains(msg, "95") || !strings.Contains(msg, "90") {
		t.Errorf("pesan error = %q, mau menyebut persentase 95 dan ambang 90", msg)
	}

	// Root media yang diukur adalah root yang dipakai staging/generation.
	if measured != env.root {
		t.Errorf("DiskUsage dipanggil dengan %q, mau %q", measured, env.root)
	}

	// Tidak ada baris generation/sync_run maupun staging yang dibuat.
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM generations`); got != 0 {
		t.Errorf("jumlah generation = %d, mau 0", got)
	}
	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM sync_runs`); got != 0 {
		t.Errorf("jumlah sync run = %d, mau 0", got)
	}
	if dirs := stagingDirs(t, env.root); len(dirs) != 0 {
		t.Errorf("staging dibuat walau ditolak: %v", dirs)
	}

	// Server HLS tidak pernah disentuh karena run tidak pernah mulai.
	if n := srv.hit("/v360/index.m3u8"); n != 0 {
		t.Errorf("playlist diunduh %d kali, mau 0", n)
	}
}

// TestStartStorageThreshold memastikan perbandingan persentase memakai
// ">=" (ambang sama dengan pemakaian sudah diblokir) dan bahwa kapasitas total
// yang tidak masuk akal melewati pemeriksaan.
func TestStartStorageThreshold(t *testing.T) {
	cases := []struct {
		name  string
		used  int64
		total int64
		block bool
	}{
		{name: "tepat di ambang", used: 90, total: 100, block: true},
		{name: "di atas ambang", used: 95, total: 100, block: true},
		{name: "sedikit di bawah ambang", used: 89, total: 100, block: false},
		{name: "setengah kapasitas", used: 50, total: 100, block: false},
		{name: "total nol dilewati", used: 100, total: 0, block: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHLSServer(t)
			src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
			env := newEnv(t, src, srv.Client(), func(c *Config) {
				c.DiskUsage = func(string) (int64, int64, error) {
					return tc.used, tc.total, nil
				}
				c.StorageBlockPercent = 90
			})

			runID, err := env.svc.Start(context.Background(), selection(""))
			if tc.block {
				if !errors.Is(err, ErrInsufficientStorage) {
					t.Fatalf("Start = %v, mau ErrInsufficientStorage", err)
				}
				if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM sync_runs`); got != 0 {
					t.Errorf("jumlah sync run = %d, mau 0", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := waitRun(t, env.store, runID); got != "completed" {
				t.Fatalf("sync_runs.status = %q, mau completed", got)
			}
		})
	}
}

// TestStartStorageBelowThreshold memastikan Start berjalan normal saat
// pemakaian disk (50%) masih di bawah ambang (90%).
func TestStartStorageBelowThreshold(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}

	calls := 0
	env := newEnv(t, src, srv.Client(), func(c *Config) {
		c.DiskUsage = func(string) (int64, int64, error) {
			calls++
			return 50, 100, nil
		}
		c.StorageBlockPercent = 90
	})

	runID, err := env.svc.Start(context.Background(), selection(srv.URL+"/art/poster.jpg"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runID == 0 {
		t.Fatal("runID = 0, mau > 0")
	}
	if calls != 1 {
		t.Errorf("DiskUsage dipanggil %d kali, mau 1", calls)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT label FROM generations WHERE status = 'active'`); got != "gen-000001" {
		t.Errorf("generation aktif = %q, mau gen-000001", got)
	}
}

// TestStartSkipsStorageCheck memastikan pemeriksaan kapasitas dilewati saat
// DiskUsage nil atau StorageBlockPercent 0.
func TestStartSkipsStorageCheck(t *testing.T) {
	t.Run("DiskUsage nil", func(t *testing.T) {
		srv := newHLSServer(t)
		src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
		env := newEnv(t, src, srv.Client(), func(c *Config) {
			c.DiskUsage = nil
			c.StorageBlockPercent = 90
		})

		runID, err := env.svc.Start(context.Background(), selection(""))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := waitRun(t, env.store, runID); got != "completed" {
			t.Fatalf("sync_runs.status = %q, mau completed", got)
		}
	})

	t.Run("ambang 0", func(t *testing.T) {
		srv := newHLSServer(t)
		src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}

		calls := 0
		env := newEnv(t, src, srv.Client(), func(c *Config) {
			c.DiskUsage = func(string) (int64, int64, error) {
				calls++
				return 100, 100, nil // akan memblokir bila ambang aktif
			}
			c.StorageBlockPercent = 0
		})

		runID, err := env.svc.Start(context.Background(), selection(""))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if calls != 0 {
			t.Errorf("DiskUsage dipanggil %d kali, mau 0 saat ambang 0", calls)
		}
		if got := waitRun(t, env.store, runID); got != "completed" {
			t.Fatalf("sync_runs.status = %q, mau completed", got)
		}
	})
}

// TestStartToleratesDiskUsageError memastikan kegagalan membaca pemakaian disk
// hanya di-log dan tidak menggagalkan ingest.
func TestStartToleratesDiskUsageError(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}

	calls := 0
	env := newEnv(t, src, srv.Client(), func(c *Config) {
		c.DiskUsage = func(string) (int64, int64, error) {
			calls++
			return 0, 0, errors.New("statfs gagal")
		}
		c.StorageBlockPercent = 90
	})

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v, mau nil karena error DiskUsage hanya di-log", err)
	}
	if calls != 1 {
		t.Errorf("DiskUsage dipanggil %d kali, mau 1", calls)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}
}
