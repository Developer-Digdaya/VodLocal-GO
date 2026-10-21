package devseed_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"vodlocal/internal/devseed"
	"vodlocal/internal/hls"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

// testConfig menyusun Config kecil yang seluruh path-nya berada di temp dir.
func testConfig(t *testing.T) devseed.Config {
	t.Helper()
	dir := t.TempDir()
	return devseed.Config{
		DBPath:      filepath.Join(dir, "app.db"),
		MediaRoot:   filepath.Join(dir, "media"),
		Titles:      1,
		Episodes:    2,
		Segments:    3,
		SegmentSize: 1024,
		Quality:     "480p",
		Provider:    "dev",
	}
}

// openStore membuka database hasil seed dan mendaftarkan Close ke Cleanup.
func openStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// queryInt menjalankan query yang menghasilkan satu angka.
func queryInt(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Read().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// queryString menjalankan query yang menghasilkan satu string.
func queryString(t *testing.T, st *store.Store, query string, args ...any) string {
	t.Helper()
	var s string
	if err := st.Read().QueryRowContext(context.Background(), query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// mediaManager menyiapkan mediafs.Manager untuk root hasil seed.
func mediaManager(t *testing.T, root string) *mediafs.Manager {
	t.Helper()
	m, err := mediafs.New(root)
	if err != nil {
		t.Fatalf("mediafs.New(%q): %v", root, err)
	}
	return m
}

// resolvePlaylist memetakan playlist_path relatif ke path absolut di disk.
func resolvePlaylist(t *testing.T, m *mediafs.Manager, label, rel string) string {
	t.Helper()
	abs, err := m.Resolve(label, rel)
	if err != nil {
		t.Fatalf("Resolve(%q, %q): %v", label, rel, err)
	}
	return abs
}

// countSegmentFiles menghitung berkas seg-*.ts di dir.
func countSegmentFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	n := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".ts") {
			continue
		}
		n++
	}
	return n
}

// TestRunCreatesActiveGeneration memeriksa hasil seed pertama: generation aktif
// di database, jumlah berkas segmen sesuai, playlist valid dan rendition ready.
func TestRunCreatesActiveGeneration(t *testing.T) {
	cfg := testConfig(t)

	res, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Generation != "gen-000001" {
		t.Errorf("Generation = %q, want %q", res.Generation, "gen-000001")
	}
	if res.ProviderID <= 0 {
		t.Errorf("ProviderID = %d, want > 0", res.ProviderID)
	}
	if len(res.TitleIDs) != 1 {
		t.Errorf("len(TitleIDs) = %d, want 1", len(res.TitleIDs))
	}
	if len(res.PlayableIDs) != 2 {
		t.Errorf("len(PlayableIDs) = %d, want 2", len(res.PlayableIDs))
	}
	if len(res.PlaylistPaths) != 2 {
		t.Errorf("len(PlaylistPaths) = %d, want 2", len(res.PlaylistPaths))
	}

	st := openStore(t, cfg.DBPath)

	if got := queryInt(t, st, `SELECT COUNT(*) FROM generations`); got != 1 {
		t.Errorf("jumlah generation = %d, want 1", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM generations WHERE status = 'active'`); got != 1 {
		t.Errorf("jumlah generation aktif = %d, want 1", got)
	}
	if got := queryString(t, st, `SELECT label FROM generations WHERE status = 'active'`); got != res.Generation {
		t.Errorf("label generation aktif = %q, want %q", got, res.Generation)
	}

	wantRenditions := 2
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions`); got != wantRenditions {
		t.Fatalf("jumlah rendition = %d, want %d", got, wantRenditions)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE status = 'ready'`); got != wantRenditions {
		t.Errorf("jumlah rendition ready = %d, want %d", got, wantRenditions)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE segment_count <> ? OR segments_done <> ?`, cfg.Segments, cfg.Segments); got != 0 {
		t.Errorf("%d rendition dengan segment_count/segments_done tidak sesuai", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE playlist_path IS NULL OR playlist_path = ''`); got != 0 {
		t.Errorf("%d rendition tanpa playlist_path", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE duration_seconds <> ?`, cfg.Segments*4); got != 0 {
		t.Errorf("%d rendition dengan duration_seconds salah", got)
	}

	m := mediaManager(t, cfg.MediaRoot)
	for _, rel := range res.PlaylistPaths {
		abs := resolvePlaylist(t, m, res.Generation, rel)

		raw, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("baca playlist %q: %v", abs, err)
		}
		if !strings.HasPrefix(string(raw), "#EXTM3U") {
			t.Errorf("playlist %q tidak dimulai #EXTM3U:\n%s", rel, raw)
		}
		if !strings.Contains(string(raw), "#EXT-X-ENDLIST") {
			t.Errorf("playlist %q tidak diakhiri #EXT-X-ENDLIST", rel)
		}
		if !strings.Contains(string(raw), "#EXTINF:4.0,") {
			t.Errorf("playlist %q tidak memuat #EXTINF:4.0,", rel)
		}

		file, err := os.Open(abs)
		if err != nil {
			t.Fatalf("buka playlist %q: %v", abs, err)
		}
		playlist, err := hls.Parse(file)
		closeErr := file.Close()
		if err != nil {
			t.Fatalf("hls.Parse(%q): %v", rel, err)
		}
		if closeErr != nil {
			t.Fatalf("tutup playlist %q: %v", rel, closeErr)
		}

		if playlist.IsMaster {
			t.Errorf("playlist %q tidak boleh master", rel)
		}
		if playlist.Version != 3 {
			t.Errorf("playlist %q Version = %d, want 3", rel, playlist.Version)
		}
		if playlist.TargetDuration != 4 {
			t.Errorf("playlist %q TargetDuration = %d, want 4", rel, playlist.TargetDuration)
		}
		if len(playlist.Segments) != cfg.Segments {
			t.Errorf("playlist %q punya %d segmen, want %d", rel, len(playlist.Segments), cfg.Segments)
		}

		dir := filepath.Dir(abs)
		for _, segment := range playlist.Segments {
			segPath := filepath.Join(dir, filepath.FromSlash(segment.URI))
			info, err := os.Stat(segPath)
			if err != nil {
				t.Fatalf("segmen %q dari playlist %q tidak ada: %v", segment.URI, rel, err)
			}
			if info.Size() != int64(cfg.SegmentSize) {
				t.Errorf("ukuran segmen %q = %d, want %d", segment.URI, info.Size(), cfg.SegmentSize)
			}
			if segment.Duration != 4 {
				t.Errorf("durasi segmen %q = %v, want 4", segment.URI, segment.Duration)
			}
		}

		if got := countSegmentFiles(t, dir); got != cfg.Segments {
			t.Errorf("jumlah berkas segmen di %q = %d, want %d", dir, got, cfg.Segments)
		}
	}
}

// TestPlaylistPathsExistOnDisk memastikan setiap path yang dicatat database
// benar-benar menunjuk berkas di dalam direktori generation.
func TestPlaylistPathsExistOnDisk(t *testing.T) {
	cfg := testConfig(t)

	res, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	st := openStore(t, cfg.DBPath)
	m := mediaManager(t, cfg.MediaRoot)

	rows, err := st.Read().QueryContext(context.Background(),
		`SELECT g.label, r.playlist_path
		   FROM renditions r
		   JOIN generations g ON g.id = r.generation_id
		  ORDER BY r.id`)
	if err != nil {
		t.Fatalf("query rendition: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var (
			label        string
			playlistPath string
		)
		if err := rows.Scan(&label, &playlistPath); err != nil {
			t.Fatalf("scan rendition: %v", err)
		}

		if label != res.Generation {
			t.Errorf("label rendition = %q, want %q", label, res.Generation)
		}
		if path.Ext(playlistPath) != ".m3u8" {
			t.Errorf("playlist_path %q bukan .m3u8", playlistPath)
		}

		info, err := os.Stat(resolvePlaylist(t, m, label, playlistPath))
		if err != nil {
			t.Fatalf("playlist_path %q tidak ada di disk: %v", playlistPath, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			t.Errorf("playlist_path %q bukan berkas biasa yang tidak kosong", playlistPath)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterasi rendition: %v", err)
	}
	if checked != len(res.PlaylistPaths) {
		t.Errorf("%d rendition diperiksa, want %d", checked, len(res.PlaylistPaths))
	}

	// Subtitle juga harus ada di disk.
	subRows, err := st.Read().QueryContext(context.Background(),
		`SELECT g.label, s.file_path FROM subtitles s JOIN generations g ON g.id = s.generation_id`)
	if err != nil {
		t.Fatalf("query subtitle: %v", err)
	}
	defer subRows.Close()

	subtitles := 0
	for subRows.Next() {
		var (
			label    string
			filePath string
		)
		if err := subRows.Scan(&label, &filePath); err != nil {
			t.Fatalf("scan subtitle: %v", err)
		}
		if _, err := os.Stat(resolvePlaylist(t, m, label, filePath)); err != nil {
			t.Fatalf("file_path subtitle %q tidak ada di disk: %v", filePath, err)
		}
		subtitles++
	}
	if err := subRows.Err(); err != nil {
		t.Fatalf("iterasi subtitle: %v", err)
	}
	if subtitles != len(res.PlayableIDs) {
		t.Errorf("%d subtitle, want %d", subtitles, len(res.PlayableIDs))
	}
}

// TestRunTwiceRetiresOlderGeneration memastikan seed ulang membuat generation
// baru, me-retire yang lama, dan tetap menyisakan tepat satu generation aktif.
func TestRunTwiceRetiresOlderGeneration(t *testing.T) {
	cfg := testConfig(t)

	first, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run pertama: %v", err)
	}
	second, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run kedua: %v", err)
	}

	if first.Generation == second.Generation {
		t.Fatalf("label generation tidak berubah: %q", first.Generation)
	}
	if first.Generation != "gen-000001" {
		t.Errorf("Generation pertama = %q, want %q", first.Generation, "gen-000001")
	}
	if second.Generation != "gen-000002" {
		t.Errorf("Generation kedua = %q, want %q", second.Generation, "gen-000002")
	}

	st := openStore(t, cfg.DBPath)

	if got := queryInt(t, st, `SELECT COUNT(*) FROM generations`); got != 2 {
		t.Errorf("jumlah generation = %d, want 2", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM generations WHERE status = 'active'`); got != 1 {
		t.Errorf("jumlah generation aktif = %d, want 1", got)
	}
	if got := queryString(t, st, `SELECT label FROM generations WHERE status = 'active'`); got != second.Generation {
		t.Errorf("generation aktif = %q, want %q", got, second.Generation)
	}
	if got := queryString(t, st, `SELECT label FROM generations WHERE status = 'retired'`); got != first.Generation {
		t.Errorf("generation retired = %q, want %q", got, first.Generation)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM generations WHERE retired_at IS NULL AND status = 'retired'`); got != 0 {
		t.Errorf("%d generation retired tanpa retired_at", got)
	}

	// Kedua direktori generation tetap ada dan bersi playlist masing-masing.
	m := mediaManager(t, cfg.MediaRoot)
	for _, label := range []string{first.Generation, second.Generation} {
		if !m.Exists(label) {
			t.Errorf("direktori generation %q tidak ada", label)
		}
	}
	if _, err := os.Stat(resolvePlaylist(t, m, second.Generation, second.PlaylistPaths[0])); err != nil {
		t.Errorf("playlist generation kedua tidak ada: %v", err)
	}

	// Rendition generation baru tidak menimpa rendition generation lama.
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE generation_id = (SELECT id FROM generations WHERE label = ?)`, second.Generation); got != len(second.PlayableIDs) {
		t.Errorf("rendition generation aktif = %d, want %d", got, len(second.PlayableIDs))
	}
}

// TestSegmentSizeControlsSegmentFileSize memastikan SegmentSize benar-benar
// menentukan ukuran berkas segmen di disk.
func TestSegmentSizeControlsSegmentFileSize(t *testing.T) {
	for _, size := range []int{512, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.SegmentSize = size

			res, err := devseed.Run(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			m := mediaManager(t, cfg.MediaRoot)
			abs := resolvePlaylist(t, m, res.Generation, res.PlaylistPaths[0])

			info, err := os.Stat(filepath.Join(filepath.Dir(abs), "seg-00001.ts"))
			if err != nil {
				t.Fatalf("stat segmen pertama: %v", err)
			}
			if info.Size() != int64(size) {
				t.Errorf("ukuran segmen = %d, want %d", info.Size(), size)
			}

			// total_bytes rendition = playlist + seluruh segmen.
			st := openStore(t, cfg.DBPath)
			var totalBytes int64
			if err := st.Read().QueryRowContext(context.Background(),
				`SELECT total_bytes FROM renditions WHERE playlist_path = ?`,
				res.PlaylistPaths[0]).Scan(&totalBytes); err != nil {
				t.Fatalf("query total_bytes: %v", err)
			}
			if want := int64(size * cfg.Segments); totalBytes <= want {
				t.Errorf("total_bytes = %d, want > %d", totalBytes, want)
			}
			if got := queryInt(t, st, `SELECT bytes_done FROM renditions WHERE playlist_path = ?`, res.PlaylistPaths[0]); int64(got) != totalBytes {
				t.Errorf("bytes_done = %d, want %d", got, totalBytes)
			}
		})
	}
}

// TestDefaultsAppliedWhenConfigEmpty memastikan field Config yang kosong memakai
// nilai default yang didokumentasikan.
func TestDefaultsAppliedWhenConfigEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := devseed.Config{
		DBPath:    filepath.Join(dir, "app.db"),
		MediaRoot: filepath.Join(dir, "media"),
	}

	res, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.TitleIDs) != 2 {
		t.Errorf("len(TitleIDs) = %d, want 2 (default Titles)", len(res.TitleIDs))
	}
	if len(res.PlayableIDs) != 4 {
		t.Errorf("len(PlayableIDs) = %d, want 4 (default Titles x Episodes)", len(res.PlayableIDs))
	}
	if len(res.PlaylistPaths) != 4 {
		t.Errorf("len(PlaylistPaths) = %d, want 4", len(res.PlaylistPaths))
	}

	st := openStore(t, cfg.DBPath)

	if got := queryInt(t, st, `SELECT COUNT(*) FROM titles`); got != 2 {
		t.Errorf("jumlah title = %d, want 2", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM episodes`); got != 4 {
		t.Errorf("jumlah episode = %d, want 4", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM playables`); got != 4 {
		t.Errorf("jumlah playable = %d, want 4", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE quality = '480p'`); got != 4 {
		t.Errorf("rendition 480p = %d, want 4 (default Quality)", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM renditions WHERE segment_count = 3`); got != 4 {
		t.Errorf("rendition dengan 3 segmen = %d, want 4 (default Segments)", got)
	}
	if got := queryInt(t, st, `SELECT COUNT(*) FROM subtitles WHERE lang = 'id' AND status = 'ready'`); got != 4 {
		t.Errorf("subtitle id ready = %d, want 4", got)
	}
	if got := queryString(t, st, `SELECT code FROM providers`); got != "dev" {
		t.Errorf("kode provider = %q, want %q (default Provider)", got, "dev")
	}
	if got := queryString(t, st, `SELECT name FROM providers`); got != "Development Provider" {
		t.Errorf("nama provider = %q, want %q", got, "Development Provider")
	}

	// Segmen default berukuran 64 KiB.
	m := mediaManager(t, cfg.MediaRoot)
	abs := resolvePlaylist(t, m, res.Generation, res.PlaylistPaths[0])
	info, err := os.Stat(filepath.Join(filepath.Dir(abs), "seg-00001.ts"))
	if err != nil {
		t.Fatalf("stat segmen: %v", err)
	}
	if info.Size() != 64*1024 {
		t.Errorf("ukuran segmen default = %d, want %d", info.Size(), 64*1024)
	}

	// Provider default juga menjadi komponen path pertama.
	if _, err := os.Stat(filepath.Join(cfg.MediaRoot, res.Generation, "dev")); err != nil {
		t.Errorf("direktori provider default tidak ada: %v", err)
	}
}

// TestRunRejectsEmptyPaths memastikan Run menolak konfigurasi tanpa tujuan tulis.
func TestRunRejectsEmptyPaths(t *testing.T) {
	if _, err := devseed.Run(context.Background(), devseed.Config{}); err == nil {
		t.Fatal("Run dengan Config kosong harus mengembalikan error")
	}
}

// TestRunPropagatesCanceledContext memastikan context yang dibatalkan tidak
// menghasilkan generation.
func TestRunPropagatesCanceledContext(t *testing.T) {
	cfg := testConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := devseed.Run(ctx, cfg); err == nil {
		t.Fatal("Run dengan context batal harus mengembalikan error")
	}
}
