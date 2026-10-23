package syncjob

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
	"testing"
	"time"

	"vodlocal/internal/download"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/provider"
	"vodlocal/internal/store"
)

// ---------------------------------------------------------------- helpers ---

// newTestStore membuka database SQLite sungguhan di t.TempDir() dan
// menjalankan migrasi.
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

// testRig mengumpulkan komponen yang dipakai satu skenario uji.
type testRig struct {
	store  *store.Store
	media  *mediafs.Manager
	syncer *Syncer
	root   string
}

// newRig menyiapkan Syncer lengkap dengan media root di t.TempDir().
func newRig(t *testing.T, st *store.Store, client *http.Client, provs ...provider.Provider) *testRig {
	t.Helper()

	root := filepath.Join(t.TempDir(), "media")
	media, err := mediafs.New(root)
	if err != nil {
		t.Fatalf("mediafs.New: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	syncer, err := New(Config{
		Store:      st,
		Media:      media,
		Downloader: download.New(download.Options{Client: client, Retries: 0}),
		Providers:  provs,
		Logger:     logger,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("syncjob.New: %v", err)
	}

	return &testRig{store: st, media: media, syncer: syncer, root: root}
}

// mediaServer menjalankan httptest.Server yang menyajikan playlist HLS dengan
// n segmen. Playlist yang tidak dikenal dijawab 404.
func mediaServer(t *testing.T, n int) *httptest.Server {
	t.Helper()

	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&pl, "#EXTINF:4.0,\nseg%d.ts\n", i)
	}
	pl.WriteString("#EXT-X-ENDLIST\n")

	mux := http.NewServeMux()
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, pl.String())
	})
	for i := 1; i <= n; i++ {
		body := bytes.Repeat([]byte{byte('a' + i)}, 64)
		mux.HandleFunc(fmt.Sprintf("/hls/seg%d.ts", i), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(body)
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// movieProvider membangun provider.Fake dengan satu movie dan satu varian.
func movieProvider(playlistURL string) *provider.Fake {
	titles := []provider.Title{{
		ExternalID:  "mv1",
		Kind:        provider.KindMovie,
		Title:       "Film Uji",
		Slug:        "film-uji",
		Synopsis:    "Film untuk pengujian.",
		ReleaseYear: 2024,
	}}
	playables := map[string][]provider.Playable{
		"mv1": {{
			ExternalID:      "mv1",
			TitleExternalID: "mv1",
			DurationSeconds: 12,
			Variants: []provider.Variant{{
				Quality:     "720p",
				BitrateKbps: 2200,
				Width:       1280,
				Height:      720,
				Container:   provider.ContainerTS,
				PlaylistURL: playlistURL,
			}},
		}},
	}
	return provider.NewFake(titles, nil, playables)
}

// scanString membaca satu kolom string.
func scanString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// scanInt membaca satu kolom integer.
func scanInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()

	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// waitActiveRun menunggu sampai ada sync_runs aktif dan mengembalikan id-nya.
func waitActiveRun(t *testing.T, st *store.Store) int64 {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var id int64
		err := st.Read().QueryRow(
			`SELECT id FROM sync_runs WHERE is_active = 1 ORDER BY id DESC LIMIT 1`).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("cari run aktif: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tidak ada sync run aktif dalam 5 detik")
	return 0
}

// stagingDirs mengembalikan nama direktori staging yang tersisa di root media.
func stagingDirs(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("baca root media: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// --------------------------------------------------------------- skenario ---

// TestRunHappyPath memastikan satu movie dengan satu varian dan playlist tiga
// segmen menghasilkan generation aktif yang lengkap di disk dan di database.
func TestRunHappyPath(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)
	rig := newRig(t, st, srv.Client(), movieProvider(srv.URL+"/hls/index.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.RunID == 0 {
		t.Errorf("RunID = 0, mau > 0")
	}
	if res.GenerationLabel != "gen-000001" {
		t.Errorf("GenerationLabel = %q, mau gen-000001", res.GenerationLabel)
	}
	if res.Titles != 1 || res.Playables != 1 {
		t.Errorf("Titles/Playables = %d/%d, mau 1/1", res.Titles, res.Playables)
	}
	if res.RenditionsReady != 1 || res.RenditionsFailed != 0 {
		t.Errorf("ready/failed = %d/%d, mau 1/0", res.RenditionsReady, res.RenditionsFailed)
	}
	if res.Partial {
		t.Errorf("Partial = true, mau false")
	}
	if res.BytesDownloaded <= 0 {
		t.Errorf("BytesDownloaded = %d, mau > 0", res.BytesDownloaded)
	}

	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE label = ?`, "gen-000001"); got != "active" {
		t.Errorf("generations.status = %q, mau active", got)
	}
	if got := scanString(t, st.Read(), `SELECT label FROM generations WHERE status = 'active'`); got != "gen-000001" {
		t.Errorf("label generation aktif = %q, mau gen-000001", got)
	}

	dir := filepath.Join(rig.root, "gen-000001", "fake", "movie", "mv1", "720p")
	if _, err := os.Stat(filepath.Join(dir, playlistFileName)); err != nil {
		t.Fatalf("playlist tidak ada di %s: %v", dir, err)
	}
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("seg-%05d.ts", i)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("segmen %s tidak ada: %v", name, err)
		}
	}

	if got := scanString(t, st.Read(), `SELECT status FROM renditions`); got != "ready" {
		t.Errorf("renditions.status = %q, mau ready", got)
	}
	if got := scanInt(t, st.Read(), `SELECT segment_count FROM renditions`); got != 3 {
		t.Errorf("renditions.segment_count = %d, mau 3", got)
	}
	if got := scanInt(t, st.Read(), `SELECT segments_done FROM renditions`); got != 3 {
		t.Errorf("renditions.segments_done = %d, mau 3", got)
	}
	if got := scanString(t, st.Read(), `SELECT playlist_path FROM renditions`); got != "fake/movie/mv1/720p/index.m3u8" {
		t.Errorf("renditions.playlist_path = %q, mau fake/movie/mv1/720p/index.m3u8", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
	if got := scanInt(t, st.Read(), `SELECT renditions_ready FROM sync_runs WHERE id = ?`, res.RunID); got != 1 {
		t.Errorf("sync_runs.renditions_ready = %d, mau 1", got)
	}
	if dirs := stagingDirs(t, rig.root); len(dirs) != 0 {
		t.Errorf("staging tersisa: %v, mau kosong", dirs)
	}
}

// TestRunRejectsConcurrent memastikan run kedua ditolak selama run pertama
// masih berjalan.
func TestRunRejectsConcurrent(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)

	fake := movieProvider(srv.URL + "/hls/index.m3u8")
	fake.Delay = 500 * time.Millisecond
	rig := newRig(t, st, srv.Client(), fake)

	type outcome struct {
		res Result
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		res, err := rig.syncer.Run(context.Background(), "manual")
		first <- outcome{res: res, err: err}
	}()

	waitActiveRun(t, st)

	if _, err := rig.syncer.Run(context.Background(), "manual"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run kedua: err = %v, mau ErrAlreadyRunning", err)
	}

	select {
	case got := <-first:
		if got.err != nil {
			t.Fatalf("Run pertama: %v", got.err)
		}
		if got.res.RenditionsReady != 1 {
			t.Errorf("run pertama: ready = %d, mau 1", got.res.RenditionsReady)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run pertama tidak selesai")
	}

	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM sync_runs`); got != 1 {
		t.Errorf("jumlah sync_runs = %d, mau 1", got)
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM generations`); got != 1 {
		t.Errorf("jumlah generations = %d, mau 1", got)
	}
}

// TestRunPlaylistNotFound memastikan satu rendition gagal tidak menggagalkan
// run: status tetap 'completed' dengan Partial=true dan generation tetap aktif.
func TestRunPlaylistNotFound(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)

	titles := []provider.Title{{
		ExternalID: "mv1",
		Kind:       provider.KindMovie,
		Title:      "Film Uji",
		Slug:       "film-uji",
	}}
	playables := map[string][]provider.Playable{
		"mv1": {{
			ExternalID:      "mv1",
			TitleExternalID: "mv1",
			Variants: []provider.Variant{
				{
					Quality:     "360p",
					BitrateKbps: 800,
					Width:       640,
					Height:      360,
					Container:   provider.ContainerTS,
					PlaylistURL: srv.URL + "/hls/tidak-ada.m3u8",
				},
				{
					Quality:     "720p",
					BitrateKbps: 2200,
					Width:       1280,
					Height:      720,
					Container:   provider.ContainerTS,
					PlaylistURL: srv.URL + "/hls/index.m3u8",
				},
			},
		}},
	}

	rig := newRig(t, st, srv.Client(), provider.NewFake(titles, nil, playables))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Partial {
		t.Errorf("Partial = false, mau true")
	}
	if res.RenditionsReady != 1 || res.RenditionsFailed != 1 {
		t.Errorf("ready/failed = %d/%d, mau 1/1", res.RenditionsReady, res.RenditionsFailed)
	}

	if got := scanString(t, st.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE label = ?`, res.GenerationLabel); got != "active" {
		t.Errorf("generations.status = %q, mau active", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM renditions WHERE quality = '360p'`); got != "failed" {
		t.Errorf("rendition 360p = %q, mau failed", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM renditions WHERE quality = '720p'`); got != "ready" {
		t.Errorf("rendition 720p = %q, mau ready", got)
	}
	if got := scanString(t, st.Read(), `SELECT last_error FROM renditions WHERE quality = '360p'`); got == "" {
		t.Errorf("last_error rendition gagal kosong, mau berisi sebab")
	}
}

// TestRunAllRenditionsFail memastikan run tanpa satu pun rendition siap
// membatalkan generation dan menandai run gagal.
func TestRunAllRenditionsFail(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)
	rig := newRig(t, st, srv.Client(), movieProvider(srv.URL+"/hls/tidak-ada.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if !errors.Is(err, ErrNoRenditions) {
		t.Fatalf("Run: err = %v, mau ErrNoRenditions", err)
	}
	if res.GenerationLabel == "" {
		t.Errorf("GenerationLabel kosong, mau terisi")
	}

	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE label = ?`, res.GenerationLabel); got != "failed" {
		t.Errorf("generations.status = %q, mau failed", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "failed" {
		t.Errorf("sync_runs.status = %q, mau failed", got)
	}
	if _, err := os.Stat(filepath.Join(rig.root, res.GenerationLabel)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("direktori generation seharusnya tidak ada, err = %v", err)
	}
	if dirs := stagingDirs(t, rig.root); len(dirs) != 0 {
		t.Errorf("staging tersisa: %v, mau kosong", dirs)
	}
}

// TestCancelStopsRun memastikan Cancel menghentikan run yang sedang berjalan,
// mengembalikan context.Canceled dan membersihkan staging.
func TestCancelStopsRun(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)

	fake := movieProvider(srv.URL + "/hls/index.m3u8")
	fake.Delay = 2 * time.Second
	rig := newRig(t, st, srv.Client(), fake)

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := rig.syncer.Run(context.Background(), "manual")
		done <- outcome{res: res, err: err}
	}()

	runID := waitActiveRun(t, st)
	if err := rig.syncer.Cancel(runID); err != nil {
		t.Fatalf("Cancel(%d): %v", runID, err)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run tidak berhenti setelah Cancel")
	}

	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, mau context.Canceled", got.err)
	}

	status := scanString(t, st.Read(), `SELECT status FROM sync_runs WHERE id = ?`, runID)
	if status != "cancelled" && status != "failed" {
		t.Errorf("sync_runs.status = %q, mau cancelled atau failed", status)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE label = ?`, got.res.GenerationLabel); got != "failed" {
		t.Errorf("generations.status = %q, mau failed", got)
	}
	if dirs := stagingDirs(t, rig.root); len(dirs) != 0 {
		t.Errorf("staging tersisa: %v, mau kosong", dirs)
	}

	if err := rig.syncer.Cancel(runID); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Cancel untuk run selesai: err = %v, mau ErrRunNotFound", err)
	}
	if err := rig.syncer.Cancel(424242); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Cancel untuk run tak dikenal: err = %v, mau ErrRunNotFound", err)
	}
}

// TestRecoverMembersihkanSisa meniru kondisi listrik padam: run 'running',
// generation 'building' dan direktori staging yang tertinggal.
func TestRecoverMembersihkanSisa(t *testing.T) {
	st := newTestStore(t)
	rig := newRig(t, st, http.DefaultClient)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Write().Exec(`
INSERT INTO generations (id, label, root_path, status, created_at) VALUES (1, 'gen-000001', 'gen-000001', 'building', ?)`, now); err != nil {
		t.Fatalf("sisip generation: %v", err)
	}
	if _, err := st.Write().Exec(`
INSERT INTO sync_runs (id, generation_id, trigger_source, scope, status, phase, started_at, created_at)
VALUES (1, 1, 'manual', 'full', 'running', 'segments', ?, ?)`, now, now); err != nil {
		t.Fatalf("sisip sync run: %v", err)
	}

	staging := filepath.Join(rig.root, stagingPrefix+"gen-000001")
	if err := os.MkdirAll(filepath.Join(staging, "fake", "movie"), 0o755); err != nil {
		t.Fatalf("buat staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "fake", "movie", "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatalf("tulis berkas staging: %v", err)
	}

	if err := rig.syncer.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := scanString(t, st.Read(), `SELECT status FROM sync_runs WHERE id = 1`); got != "failed" {
		t.Errorf("sync_runs.status = %q, mau failed", got)
	}
	if got := scanString(t, st.Read(), `SELECT error_message FROM sync_runs WHERE id = 1`); got != "power loss" {
		t.Errorf("sync_runs.error_message = %q, mau power loss", got)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE id = 1`); got != "failed" {
		t.Errorf("generations.status = %q, mau failed", got)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("direktori staging masih ada, err = %v", err)
	}

	// Idempoten: pemanggilan kedua tidak error.
	if err := rig.syncer.Recover(context.Background()); err != nil {
		t.Fatalf("Recover kedua: %v", err)
	}
}

// TestRetiredGenerationTetapAda memastikan generation 'retired' tidak pernah
// dihapus otomatis: setelah run berikutnya baris dan direktorinya tetap utuh
// (walau retired_at sudah lama), karena penghapusan hanya dilakukan admin.
func TestRetiredGenerationTetapAda(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)
	rig := newRig(t, st, srv.Client(), movieProvider(srv.URL+"/hls/index.m3u8"))

	first, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run pertama: %v", err)
	}
	if first.GenerationLabel != "gen-000001" {
		t.Fatalf("label pertama = %q, mau gen-000001", first.GenerationLabel)
	}

	// Beri retired_at yang sudah sangat tua: generation ini tetap tidak boleh
	// dihapus run berikutnya karena tidak ada lagi masa tenggang.
	old := "2000-01-01T00:00:00Z"
	if _, err := st.Write().Exec(
		`UPDATE generations SET status = 'retired', retired_at = ? WHERE label = ?`,
		old, first.GenerationLabel); err != nil {
		t.Fatalf("retire generation pertama: %v", err)
	}

	second, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run kedua: %v", err)
	}
	if second.GenerationLabel != "gen-000002" {
		t.Fatalf("label kedua = %q, mau gen-000002", second.GenerationLabel)
	}

	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM generations WHERE label = ?`, first.GenerationLabel); got != 1 {
		t.Errorf("baris %s hilang, mau tetap ada", first.GenerationLabel)
	}
	if got := scanString(t, st.Read(), `SELECT status FROM generations WHERE label = ?`, first.GenerationLabel); got != "retired" {
		t.Errorf("status %s = %q, mau retired", first.GenerationLabel, got)
	}
	if got := scanString(t, st.Read(), `SELECT COALESCE(retired_at, '') FROM generations WHERE label = ?`, first.GenerationLabel); got == "" {
		t.Errorf("retired_at %s kosong, mau terisi", first.GenerationLabel)
	}
	if _, err := os.Stat(filepath.Join(rig.root, first.GenerationLabel, "fake", "movie", "mv1", "720p", playlistFileName)); err != nil {
		t.Errorf("playlist generation retired hilang: %v", err)
	}
	if got := scanString(t, st.Read(), `SELECT label FROM generations WHERE status = 'active'`); got != second.GenerationLabel {
		t.Errorf("generation aktif = %q, mau %q", got, second.GenerationLabel)
	}
	if _, err := os.Stat(filepath.Join(rig.root, second.GenerationLabel, "fake", "movie", "mv1", "720p", playlistFileName)); err != nil {
		t.Errorf("playlist generation aktif tidak ada: %v", err)
	}
}

// TestSyncTerhadapDatabaseKosong memastikan provider tanpa data tidak
// menggagalkan Run (hanya berakhir dengan ErrNoRenditions) dan path yang
// tersimpan relatif.
func TestSyncTerhadapDatabaseKosong(t *testing.T) {
	srv := mediaServer(t, 3)
	st := newTestStore(t)
	empty := provider.NewFake([]provider.Title{{
		ExternalID: "kosong",
		Kind:       provider.KindMovie,
		Title:      "Tanpa Playable",
		Slug:       "tanpa-playable",
	}}, nil, nil)
	rig := newRig(t, st, srv.Client(), empty)

	res, err := rig.syncer.Run(context.Background(), "boot")
	if !errors.Is(err, ErrNoRenditions) {
		t.Fatalf("Run: err = %v, mau ErrNoRenditions", err)
	}

	if got := scanString(t, st.Read(), `SELECT trigger_source FROM sync_runs WHERE id = ?`, res.RunID); got != "boot" {
		t.Errorf("trigger_source = %q, mau boot", got)
	}
	if got := scanString(t, st.Read(), `SELECT root_path FROM generations WHERE label = ?`, res.GenerationLabel); got != res.GenerationLabel {
		t.Errorf("generations.root_path = %q, mau relatif %q", got, res.GenerationLabel)
	}
	if got := scanString(t, st.Read(), `SELECT slug FROM titles`); got != "tanpa-playable" {
		t.Errorf("titles.slug = %q, mau tanpa-playable", got)
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM renditions`); got != 0 {
		t.Errorf("jumlah renditions = %d, mau 0", got)
	}
}
