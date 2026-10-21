package admin_test

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// countByID menghitung baris satu tabel dengan id tertentu.
func countByID(f *fixture, t *testing.T, table string, id int64) int {
	t.Helper()

	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("hitung %s %d: %v", table, id, err)
	}
	return count
}

// makeMediaDir membuat direktori media berisi satu berkas contoh dan
// mengembalikan path direktori tersebut.
func makeMediaDir(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "dens_tv", "series", "1"), 0o755); err != nil {
		t.Fatalf("buat direktori media %s: %v", name, err)
	}
	file := filepath.Join(dir, "dens_tv", "series", "1", "index.m3u8")
	if err := os.WriteFile(file, []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatalf("tulis berkas media %s: %v", file, err)
	}
	return dir
}

// requireNotExist memastikan path sudah tidak ada di disk.
func requireNotExist(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s masih ada di disk", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

// TestStorageDeleteMembersihkanKatalogTanpaMedia memastikan POST delete
// generation retired ikut membuang title/episode/playable yang tidak lagi punya
// rendition, tanpa menyentuh katalog yang masih dipakai generation lain.
func TestStorageDeleteMembersihkanKatalogTanpaMedia(t *testing.T) {
	f := newFixture(t)
	f.login()

	// Katalog yang medianya hanya ada di generation retired 41.
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (3, 1, 'ext-3', 'series', 'Series Lama', 'series-lama')`)
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
	        VALUES (303, 3, 'ep-303', 1, 1, 'Satu')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (9004, 3, 303, 1200)`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9004, 41, '480p', 'ready', 'dens_tv/series/3/s01e01/480p/index.m3u8')`)

	rec := f.post("/storage/generations/41/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus retired: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := generationCount(f, t, 41); got != 0 {
		t.Fatalf("baris generation 41 masih ada")
	}

	// Title yang medianya tidak tersisa lagi ikut terhapus beserta episode dan
	// playable-nya.
	for _, tc := range []struct {
		table string
		id    int64
	}{
		{"titles", 3},
		{"episodes", 303},
		{"playables", 9004},
	} {
		if got := countByID(f, t, tc.table, tc.id); got != 0 {
			t.Errorf("%s %d masih ada setelah generation dihapus", tc.table, tc.id)
		}
	}

	// Title yang rendition-nya masih ada di generation aktif 42 tetap ada.
	for _, id := range []int64{1, 2} {
		if got := countByID(f, t, "titles", id); got != 1 {
			t.Errorf("titles %d ikut terhapus padahal masih dipakai generation aktif", id)
		}
	}

	var renditions int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM renditions WHERE generation_id = 42`).Scan(&renditions); err != nil {
		t.Fatalf("hitung rendition generation aktif: %v", err)
	}
	if renditions != 4 {
		t.Errorf("rendition generation aktif = %d, mau 4", renditions)
	}
}

// TestStorageDeleteGenerationAktif memastikan generation berstatus active juga
// bisa dihapus (tidak ada lagi aturan "last batch"): baris database dan folder
// di disk hilang, lalu katalog yang medianya hanya ada di generation itu ikut
// dibersihkan CleanupOrphans.
func TestStorageDeleteGenerationAktif(t *testing.T) {
	f := newFixture(t)
	f.login()

	dir := makeMediaDir(t, f.mediaRoot, "gen-000042")

	// confirm wajib DELETE, juga untuk generation aktif.
	rec := f.post("/storage/generations/42/delete", url.Values{"confirm": {"hapus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm salah: status = %d, mau 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := generationCount(f, t, 42); got != 1 {
		t.Fatalf("generation aktif terhapus tanpa konfirmasi")
	}
	if got := f.media.removedLabels(); len(got) != 0 {
		t.Fatalf("Media.Remove dipanggil walau konfirmasi salah: %v", got)
	}

	rec = f.post("/storage/generations/42/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus active: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/storage" {
		t.Fatalf("Location = %q, mau /storage", loc)
	}
	if got := generationCount(f, t, 42); got != 0 {
		t.Fatalf("baris generation aktif 42 masih ada")
	}
	if got := f.media.removedLabels(); len(got) != 1 || got[0] != "gen-000042" {
		t.Fatalf("Media.Remove dipanggil dengan %v, mau [gen-000042]", got)
	}
	requireNotExist(t, dir)

	// Seluruh rendition generation aktif hilang lewat cascade, sehingga
	// playable/episode/title yang tidak punya media lain ikut dibersihkan.
	for _, table := range []string{"titles", "episodes", "playables", "renditions"} {
		var count int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("hitung %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s = %d baris, mau 0 setelah generation aktif dihapus", table, count)
		}
	}
}
