package transfer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mediaRoot := filepath.Join(dir, "media")
	media, err := mediafs.New(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Store: st, Media: media, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}

	// Minimal catalog: one movie with one 360p rendition.
	seed := []string{
		`INSERT INTO providers (id, code, name, category) VALUES (1, 'densgo', 'Densgo', 'vod')`,
		`INSERT INTO generations (id, label, root_path, status) VALUES (1, 'gen-000001', 'gen-000001', 'active')`,
		`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, poster_path)
		 VALUES (1, 1, 'movie-1', 'movie', 'Film Uji', 'film-uji', 'densgo/movie/1/poster.jpg')`,
		`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (1, 1, NULL, 62)`,
		`INSERT INTO renditions (id, playable_id, generation_id, quality, bitrate_kbps, width, height,
		                         container, playlist_path, segment_count, total_bytes, duration_seconds, status)
		 VALUES (1, 1, 1, '360p', 600, 640, 360, 'ts', 'densgo/movie/1/360p/index.m3u8', 2, 12, 62, 'ready')`,
		`INSERT INTO generation_items (id, generation_id, provider_id, category, series_external_id,
		                              series_title, status, total_episodes, episodes_done)
		 VALUES (1, 1, 1, 'movie', 'movie-1', 'Film Uji', 'done', 1, 1)`,
	}
	for _, q := range seed {
		if _, err := st.Write().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	base := filepath.Join(mediaRoot, "gen-000001", "densgo", "movie", "1")
	writeTestFile(t, filepath.Join(base, "poster.jpg"), []byte("poster"))
	writeTestFile(t, filepath.Join(base, "360p", "index.m3u8"), []byte("#EXTM3U\n#EXTINF:31,\nseg-00001.ts\n#EXTINF:31,\nseg-00002.ts\n#EXT-X-ENDLIST\n"))
	writeTestFile(t, filepath.Join(base, "360p", "seg-00001.ts"), []byte("segment-one"))
	writeTestFile(t, filepath.Join(base, "360p", "seg-00002.ts"), []byte("segment-two"))

	items, total, err := svc.ListLocalContent(ctx, 1, 30)
	if err != nil {
		t.Fatalf("ListLocalContent: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("local content = %d/%d, mau 1/1", len(items), total)
	}
	if items[0].PosterURL != "/media/gen-000001/densgo/movie/1/poster.jpg" {
		t.Fatalf("poster_url = %q", items[0].PosterURL)
	}

	dest := filepath.Join(dir, "hdd")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	exportRun, err := svc.StartExport(ctx, []int64{1}, dest, "test")
	if err != nil {
		t.Fatalf("StartExport: %v", err)
	}
	waitRun(t, st, exportRun, "completed")

	packages, err := svc.ListPackages(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0].Error != "" {
		t.Fatalf("packages = %+v, mau satu paket valid", packages)
	}
	if len(packages[0].Items) != 1 || packages[0].Items[0].Title != "Film Uji" {
		t.Fatalf("items = %+v", packages[0].Items)
	}
	m, err := ReadManifest(packages[0].Path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.TotalBytes == 0 || len(m.Files) != 4 {
		t.Fatalf("manifest files/bytes = %d/%d, mau 4/>0", len(m.Files), m.TotalBytes)
	}

	importRun, err := svc.StartImport(ctx, packages[0].Path, nil, "test")
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	waitRun(t, st, importRun, "completed")

	var count int
	if err := st.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM generations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("generations = %d, mau 2", count)
	}
	var label, status string
	if err := st.Read().QueryRowContext(ctx,
		`SELECT label, status FROM generations ORDER BY id DESC LIMIT 1`).Scan(&label, &status); err != nil {
		t.Fatal(err)
	}
	if label != "gen-000002" || status != "active" {
		t.Fatalf("generation baru = %s/%s, mau gen-000002/active", label, status)
	}
	var playlist string
	if err := st.Read().QueryRowContext(ctx,
		`SELECT playlist_path FROM renditions WHERE generation_id = 2`).Scan(&playlist); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mediaRoot, label, filepath.FromSlash(playlist))); err != nil {
		t.Fatalf("playlist import tidak ada: %v", err)
	}
}

func TestImportRejectsCorruptFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write().ExecContext(ctx,
		`INSERT INTO providers (id, code, name) VALUES (1, 'densgo', 'Densgo')`); err != nil {
		t.Fatal(err)
	}
	media, err := mediafs.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Store: st, Media: media, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}

	pkg := filepath.Join(dir, "pkg")
	data := []byte("utuh")
	writeTestFile(t, filepath.Join(pkg, "media", "a.bin"), data)
	hash, size, err := hashFile(filepath.Join(pkg, "media", "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Format: Format, PackageID: "pkg", CreatedAt: "2026-01-01T00:00:00Z",
		Items: []Item{{
			Provider: "densgo", Category: "movie",
			Title: TitleData{ExternalID: "m1", Kind: "movie", Name: "Film"},
			Files: []string{"media/a.bin"},
		}},
		Files: []File{{Path: "media/a.bin", Target: "a.bin", Size: size, SHA256: hash}},
	}
	if err := WriteManifest(pkg, m); err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload after the manifest has been written.
	if err := os.WriteFile(filepath.Join(pkg, "media", "a.bin"), []byte("rusak"), 0o644); err != nil {
		t.Fatal(err)
	}

	runID, err := svc.StartImport(ctx, pkg, nil, "test")
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	waitRun(t, st, runID, "failed")
}

func waitRun(t *testing.T, st *store.Store, runID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var errMsg sql.NullString
		if err := st.Read().QueryRow(
			`SELECT status, error_message FROM sync_runs WHERE id = ?`, runID).Scan(&status, &errMsg); err != nil {
			t.Fatal(err)
		}
		if status == want {
			return
		}
		if status == "failed" || status == "cancelled" {
			t.Fatalf("run %d berakhir %s: %s", runID, status, errMsg.String)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %d tidak selesai", runID)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
