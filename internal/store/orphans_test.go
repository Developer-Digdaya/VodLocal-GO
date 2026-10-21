package store

import (
	"context"
	"path/filepath"
	"testing"
)

// mustExec menjalankan satu perintah SQL seed dan menghentikan test bila gagal.
func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()

	if _, err := s.Write().Exec(query, args...); err != nil {
		t.Fatalf("exec gagal: %v\nquery: %s", err, query)
	}
}

// countRows menghitung seluruh baris satu tabel.
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()

	return scalarInt(t, s, `SELECT COUNT(*) FROM `+table)
}

// seedOrphanProvider menyisipkan satu provider yang dipakai seluruh seed katalog.
func seedOrphanProvider(t *testing.T, s *Store) {
	t.Helper()

	mustExec(t, s, `INSERT INTO providers (id, code, name) VALUES (1, 'dens_tv', 'Dens TV')`)
}

// seedOrphanGeneration menyisipkan satu generation dan mengembalikan id-nya.
func seedOrphanGeneration(t *testing.T, s *Store, label, status string) int64 {
	t.Helper()

	var id int64
	if err := s.Write().QueryRow(
		`INSERT INTO generations (label, root_path, status) VALUES (?, ?, ?) RETURNING id`,
		label, label, status,
	).Scan(&id); err != nil {
		t.Fatalf("insert generation %s: %v", label, err)
	}
	return id
}

// seedOrphanTitle menyisipkan satu title milik provider 1 dan mengembalikan id-nya.
func seedOrphanTitle(t *testing.T, s *Store, external, kind string) int64 {
	t.Helper()

	var id int64
	if err := s.Write().QueryRow(`
INSERT INTO titles (provider_id, external_id, kind, title, slug)
VALUES (1, ?, ?, ?, ?) RETURNING id`,
		external, kind, external, external,
	).Scan(&id); err != nil {
		t.Fatalf("insert title %s: %v", external, err)
	}
	return id
}

// seedOrphanEpisode menyisipkan satu episode milik title dan mengembalikan id-nya.
func seedOrphanEpisode(t *testing.T, s *Store, titleID int64, external string, number int) int64 {
	t.Helper()

	var id int64
	if err := s.Write().QueryRow(`
INSERT INTO episodes (title_id, external_id, season_number, episode_number, name)
VALUES (?, ?, 1, ?, ?) RETURNING id`,
		titleID, external, number, external,
	).Scan(&id); err != nil {
		t.Fatalf("insert episode %s: %v", external, err)
	}
	return id
}

// seedOrphanPlayable menyisipkan satu playable; episodeID nil berarti movie.
func seedOrphanPlayable(t *testing.T, s *Store, titleID int64, episodeID any) int64 {
	t.Helper()

	var id int64
	if err := s.Write().QueryRow(`
INSERT INTO playables (title_id, episode_id, duration_seconds) VALUES (?, ?, 600) RETURNING id`,
		titleID, episodeID,
	).Scan(&id); err != nil {
		t.Fatalf("insert playable title %d: %v", titleID, err)
	}
	return id
}

// seedOrphanRendition menyisipkan satu rendition pada generation.
func seedOrphanRendition(t *testing.T, s *Store, playableID, generationID int64) {
	t.Helper()

	mustExec(t, s, `
INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
VALUES (?, ?, '480p', 'ready', 'dens_tv/index.m3u8')`, playableID, generationID)
}

// deleteGeneration menghapus satu baris generation seperti yang dilakukan panel
// admin dan proses sync (renditions ikut terhapus lewat ON DELETE CASCADE).
func deleteGeneration(t *testing.T, s *Store, generationID int64) {
	t.Helper()

	mustExec(t, s, `DELETE FROM generations WHERE id = ?`, generationID)
}

// TestCleanupOrphansMenghapusKatalogTanpaMedia memastikan setelah generation
// (beserta rendition-nya) dihapus, seluruh playable/episode/title yang tidak
// punya media lagi ikut terhapus, sedangkan provider tetap ada.
func TestCleanupOrphansMenghapusKatalogTanpaMedia(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	seedOrphanProvider(t, s)
	generation := seedOrphanGeneration(t, s, "gen-000001", "active")

	// Series: title + episode + playable episode + rendition.
	seriesID := seedOrphanTitle(t, s, "series-uji", "series")
	episodeID := seedOrphanEpisode(t, s, seriesID, "ep-1", 1)
	episodePlayable := seedOrphanPlayable(t, s, seriesID, episodeID)
	seedOrphanRendition(t, s, episodePlayable, generation)

	// Movie: title + playable tanpa episode + rendition.
	movieID := seedOrphanTitle(t, s, "film-uji", "movie")
	moviePlayable := seedOrphanPlayable(t, s, movieID, nil)
	seedOrphanRendition(t, s, moviePlayable, generation)

	if got := countRows(t, s, "renditions"); got != 2 {
		t.Fatalf("jumlah renditions = %d, mau 2", got)
	}

	// File media hilang: generation dihapus, rendition ikut cascade.
	deleteGeneration(t, s, generation)
	if got := countRows(t, s, "renditions"); got != 0 {
		t.Fatalf("jumlah renditions setelah generation dihapus = %d, mau 0", got)
	}
	if got := countRows(t, s, "playables"); got != 2 {
		t.Fatalf("jumlah playables sebelum cleanup = %d, mau 2", got)
	}

	deleted, err := s.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans() error: %v", err)
	}

	// 2 playable + 1 episode + 2 title.
	const want int64 = 5
	if deleted != want {
		t.Errorf("baris terhapus = %d, mau %d", deleted, want)
	}
	for _, table := range []string{"playables", "episodes", "titles"} {
		if got := countRows(t, s, table); got != 0 {
			t.Errorf("jumlah %s setelah cleanup = %d, mau 0", table, got)
		}
	}

	// Provider bukan bagian dari pembersihan.
	if got := countRows(t, s, "providers"); got != 1 {
		t.Errorf("jumlah providers = %d, mau 1", got)
	}
}

// TestCleanupOrphansMempertahankanKatalogYangMasihPunyaMedia memastikan title
// yang rendition-nya masih ada di generation lain tidak ikut terhapus saat satu
// generation dihapus.
func TestCleanupOrphansMempertahankanKatalogYangMasihPunyaMedia(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	seedOrphanProvider(t, s)
	oldGeneration := seedOrphanGeneration(t, s, "gen-000001", "retired")
	newGeneration := seedOrphanGeneration(t, s, "gen-000002", "active")

	// Title yang tetap tayang: rendition tersedia di generation lama dan baru.
	sharedID := seedOrphanTitle(t, s, "film-tetap", "movie")
	sharedPlayable := seedOrphanPlayable(t, s, sharedID, nil)
	seedOrphanRendition(t, s, sharedPlayable, oldGeneration)
	seedOrphanRendition(t, s, sharedPlayable, newGeneration)

	// Title yang hanya ada di generation lama.
	staleID := seedOrphanTitle(t, s, "film-lama", "movie")
	stalePlayable := seedOrphanPlayable(t, s, staleID, nil)
	seedOrphanRendition(t, s, stalePlayable, oldGeneration)

	deleteGeneration(t, s, oldGeneration)

	deleted, err := s.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans() error: %v", err)
	}

	// 1 playable + 1 title milik generation yang hilang.
	const want int64 = 2
	if deleted != want {
		t.Errorf("baris terhapus = %d, mau %d", deleted, want)
	}

	if got := scalarInt(t, s, `SELECT COUNT(*) FROM titles WHERE id = ?`, sharedID); got != 1 {
		t.Errorf("title yang masih punya rendition ikut terhapus")
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM playables WHERE id = ?`, sharedPlayable); got != 1 {
		t.Errorf("playable yang masih punya rendition ikut terhapus")
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM renditions WHERE generation_id = ?`, newGeneration); got != 1 {
		t.Errorf("rendition generation aktif ikut terhapus")
	}

	if got := scalarInt(t, s, `SELECT COUNT(*) FROM titles WHERE id = ?`, staleID); got != 0 {
		t.Errorf("title tanpa media tersisa di database")
	}
	if got := scalarInt(t, s, `SELECT COUNT(*) FROM playables WHERE id = ?`, stalePlayable); got != 0 {
		t.Errorf("playable tanpa media tersisa di database")
	}
}

// TestCleanupOrphansIdempotent memastikan pemanggilan berulang tidak error dan
// panggilan kedua tidak menghapus apa pun.
func TestCleanupOrphansIdempotent(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	seedOrphanProvider(t, s)
	generation := seedOrphanGeneration(t, s, "gen-000001", "active")

	titleID := seedOrphanTitle(t, s, "film-uji", "movie")
	playableID := seedOrphanPlayable(t, s, titleID, nil)
	seedOrphanRendition(t, s, playableID, generation)

	deleteGeneration(t, s, generation)

	first, err := s.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans() pertama error: %v", err)
	}
	if first == 0 {
		t.Fatal("CleanupOrphans() pertama tidak menghapus apa pun")
	}

	second, err := s.CleanupOrphans(ctx)
	if err != nil {
		t.Fatalf("CleanupOrphans() kedua error: %v", err)
	}
	if second != 0 {
		t.Errorf("CleanupOrphans() kedua menghapus %d baris, mau 0", second)
	}

	// Database kosong pun bukan error.
	empty := newMigratedStore(t)
	if n, err := empty.CleanupOrphans(ctx); err != nil || n != 0 {
		t.Errorf("CleanupOrphans() pada database kosong = (%d, %v), mau (0, nil)", n, err)
	}

	// Store yang sudah ditutup dilaporkan sebagai error, bukan panic.
	closed, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if err := closed.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if _, err := closed.CleanupOrphans(ctx); err == nil {
		t.Error("CleanupOrphans() pada Store tertutup tidak error")
	}
}
