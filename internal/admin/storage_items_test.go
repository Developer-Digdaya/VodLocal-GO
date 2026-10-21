package admin_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Folder dan playlist contoh untuk konten yang diuji. Bentuk path mengikuti
// media nyata: dramahits/series/<uuid>/<episode>/<quality>/index.m3u8.
const (
	seriesAUUID     = "019eaaaa-1111-7222-8333-444455556666"
	seriesAEpisode  = "019ebbbb-2222-7333-8444-555566667777"
	seriesAPlaylist = "dramahits/series/" + seriesAUUID + "/" + seriesAEpisode + "/480p/index.m3u8"
	// seriesAPlaylistOld adalah playlist generation lama (41) untuk konten yang
	// sama, sehingga title-nya tidak boleh ikut terhapus saat konten di
	// generation aktif dihapus.
	seriesAPlaylistOld = "dramahits/series/" + seriesAUUID + "/" + seriesAEpisode + "/360p/index.m3u8"
)

// seedStorageContent menambahkan satu series Dramahits beserta dua baris
// generation_items pada fixture:
//
//   - item 500: "Series A" di generation 42 (provider dramahits), rendisinya ada
//     di generation 42 dan 41.
//   - item 501: "Cinta Pertama" di generation 42 (provider dens_tv), memakai
//     katalog contoh fixture.
func seedStorageContent(f *fixture) {
	f.exec(`INSERT INTO providers (id, code, name, category)
	        VALUES (2, 'dramahits', 'Dramahits', 'short')`)
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (10, 2, 'series-a', 'series', 'Series A', 'series-a')`)
	f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
	        VALUES (401, 10, 'ep-a1', 1, 1, 'Pertama')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
	        VALUES (9100, 10, 401, 900)`)

	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path, total_bytes)
	        VALUES (9100, 42, '480p', 'ready', ?, 4096)`, seriesAPlaylist)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path, total_bytes)
	        VALUES (9100, 41, '480p', 'ready', ?, 2048)`, seriesAPlaylistOld)

	// Satu rendition generation aktif diberi ukuran agar total_bytes generation
	// bisa diperiksa setelah penghapusan.
	f.exec(`UPDATE renditions SET total_bytes = 1024
	         WHERE playable_id = 9001 AND generation_id = 42 AND quality = '480p'`)

	f.exec(`INSERT INTO generation_items
	          (id, generation_id, provider_id, category, series_external_id, series_title,
	           status, total_episodes, episodes_done, progress_percent)
	        VALUES (500, 42, 2, 'short', 'series-a', 'Series A', 'done', 1, 1, 100)`)
	f.exec(`INSERT INTO generation_items
	          (id, generation_id, provider_id, category, series_external_id, series_title,
	           status, total_episodes, episodes_done, progress_percent)
	        VALUES (501, 42, 1, 'vod', 'ext-1', 'Cinta Pertama', 'running', 2, 1, 50)`)
}

// makeSeriesMediaDir membuat folder series di bawah media root dan
// mengembalikan path folder series (level <uuid>).
func makeSeriesMediaDir(t *testing.T, mediaRoot, label, seriesUUID, episodeUUID string) string {
	t.Helper()

	seriesDir := filepath.Join(mediaRoot, label, "dramahits", "series", seriesUUID)
	qualityDir := filepath.Join(seriesDir, episodeUUID, "480p")
	if err := os.MkdirAll(qualityDir, 0o755); err != nil {
		t.Fatalf("buat folder series %s: %v", seriesDir, err)
	}
	if err := os.WriteFile(filepath.Join(qualityDir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatalf("tulis playlist %s: %v", qualityDir, err)
	}
	return seriesDir
}

// countQuery menghitung hasil satu query COUNT.
func countQuery(f *fixture, t *testing.T, query string, args ...any) int {
	t.Helper()

	var count int
	if err := f.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("hitung %q: %v", query, err)
	}
	return count
}

// TestStoragePageShowsContentItems memastikan /storage menampilkan daftar konten
// (judul, generation, ukuran, progres), bukan tabel generation.
func TestStoragePageShowsContentItems(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedStorageContent(f)

	rec := f.get("/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /storage: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"2 konten",                  // ringkasan jumlah konten
		"Series A",                  // judul konten
		"series-a",                  // slug konten
		"Cinta Pertama",             // konten kedua
		"gen-000042",                // label generation
		"4.0 KB",                    // ukuran konten Series A (4096 B)
		"<progress",                 // elemen progress
		"1/1",                       // episode selesai/total
		`/storage/items/500/delete`, // aksi hapus per konten
		"table-wrap",                // tabel dibungkus .table-wrap
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman /storage tidak memuat %q (body=%s)", want, body)
		}
	}

	// Tabel generation sebagai daftar utama (dengan tombol hapus per batch)
	// sudah tidak ada.
	if strings.Contains(body, "/storage/generations/") {
		t.Fatalf("halaman /storage masih memuat aksi hapus per batch")
	}
	for _, unwanted := range []string{"Hapus generation", "Generation aktif juga bisa dihapus"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("halaman /storage masih memuat %q", unwanted)
		}
	}
}

// TestStorageItemDelete memastikan penghapusan per konten hanya menghapus folder
// series, renditions, dan baris generation_items milik konten tersebut.
func TestStorageItemDelete(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedStorageContent(f)

	seriesDir := makeSeriesMediaDir(t, f.mediaRoot, "gen-000042", seriesAUUID, seriesAEpisode)
	// Folder konten lain pada generation yang sama tidak boleh tersentuh.
	otherDir := makeMediaDir(t, f.mediaRoot, "gen-000042")

	rec := f.post("/storage/items/500/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus konten: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/storage" {
		t.Fatalf("Location = %q, mau /storage", loc)
	}

	// Folder series hilang; folder konten lain tetap ada.
	requireNotExist(t, seriesDir)
	if _, err := os.Stat(otherDir); err != nil {
		t.Fatalf("folder konten lain ikut terhapus: %v", err)
	}

	// Rendition generation aktif untuk title itu hilang, rendition generation
	// lama tetap ada, dan konten lain tidak terpengaruh.
	if got := countQuery(f, t,
		`SELECT COUNT(*) FROM renditions WHERE generation_id = 42 AND playable_id = 9100`); got != 0 {
		t.Errorf("rendition generation 42 untuk title 10 = %d baris, mau 0", got)
	}
	if got := countQuery(f, t,
		`SELECT COUNT(*) FROM renditions WHERE generation_id = 41 AND playable_id = 9100`); got != 1 {
		t.Errorf("rendition generation 41 untuk title 10 = %d baris, mau 1 (tidak boleh terhapus)", got)
	}
	if got := countQuery(f, t,
		`SELECT COUNT(*) FROM renditions WHERE generation_id = 42`); got != 4 {
		t.Errorf("rendition generation 42 = %d baris, mau 4 (konten lain tetap utuh)", got)
	}

	// Title yang masih punya rendition di generation lain tidak terhapus.
	if got := countByID(f, t, "titles", 10); got != 1 {
		t.Errorf("title 10 terhapus padahal masih punya rendition di generation 41")
	}
	if got := countByID(f, t, "playables", 9100); got != 1 {
		t.Errorf("playable 9100 terhapus padahal masih punya rendition di generation 41")
	}

	// Baris generation_items: hanya item 500 yang hilang.
	if got := countByID(f, t, "generation_items", 500); got != 0 {
		t.Errorf("generation_items 500 masih ada")
	}
	if got := countByID(f, t, "generation_items", 501); got != 1 {
		t.Errorf("generation_items 501 (konten lain) ikut terhapus")
	}

	// total_bytes generation dihitung ulang dari rendition yang tersisa
	// (satu rendition generation 42 bernilai 1024 B).
	var totalBytes int64
	if err := f.db.QueryRow(`SELECT total_bytes FROM generations WHERE id = 42`).Scan(&totalBytes); err != nil {
		t.Fatalf("baca total_bytes generation 42: %v", err)
	}
	if totalBytes != 1024 {
		t.Errorf("total_bytes generation 42 = %d, mau 1024", totalBytes)
	}

	// Flash menyebut judul konten dan label generation.
	page := f.get("/storage")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /storage: status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, "Konten Series A dihapus dari gen-000042.") {
		t.Fatalf("flash penghapusan konten tidak tampil (body=%s)", body)
	}
	if !strings.Contains(body, "1 konten") {
		t.Fatalf("ringkasan setelah penghapusan tidak menyebut 1 konten (body=%s)", body)
	}
	// Baris konten yang dihapus tidak lagi punya aksi hapus di tabel.
	if strings.Contains(body, "/storage/items/500/delete") {
		t.Fatalf("konten yang dihapus masih tampil di halaman /storage")
	}
	if !strings.Contains(body, "/storage/items/501/delete") {
		t.Fatalf("konten lain hilang dari halaman /storage")
	}
}

// TestStorageItemDeleteTanpaRendition memastikan konten yang tidak punya
// rendition sama sekali tetap bisa dihapus: hanya baris database yang hilang dan
// tidak ada error.
func TestStorageItemDeleteTanpaRendition(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedStorageContent(f)

	f.exec(`INSERT INTO generation_items
	          (id, generation_id, provider_id, category, series_external_id, series_title,
	           status, total_episodes, episodes_done, progress_percent)
	        VALUES (502, 42, 2, 'short', 'series-tanpa-media', 'Tanpa Media', 'failed', 0, 0, 0)`)

	rec := f.post("/storage/items/502/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus konten tanpa rendition: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := countByID(f, t, "generation_items", 502); got != 0 {
		t.Fatalf("generation_items 502 masih ada")
	}
	// Konten lain tidak terpengaruh.
	if got := countByID(f, t, "generation_items", 500); got != 1 {
		t.Fatalf("generation_items 500 ikut terhapus")
	}
	if got := countByID(f, t, "titles", 10); got != 1 {
		t.Fatalf("title 10 terhapus padahal tidak ada yang dihapus untuknya")
	}
}

// TestStorageItemDeleteValidasi memastikan 404/400/403/redirect-login berlaku
// untuk route hapus per konten.
func TestStorageItemDeleteValidasi(t *testing.T) {
	t.Run("id tidak ada", func(t *testing.T) {
		f := newFixture(t)
		f.login()
		seedStorageContent(f)

		rec := f.post("/storage/items/9999/delete", url.Values{"confirm": {"DELETE"}})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, mau 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("confirm salah", func(t *testing.T) {
		f := newFixture(t)
		f.login()
		seedStorageContent(f)

		rec := f.post("/storage/items/500/delete", url.Values{"confirm": {"hapus"}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, mau 400 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "confirm=DELETE") {
			t.Fatalf("pesan konfirmasi tidak tampil (body=%s)", rec.Body.String())
		}
		if got := countByID(f, t, "generation_items", 500); got != 1 {
			t.Fatalf("generation_items 500 terhapus walau konfirmasi salah")
		}
	})

	t.Run("tanpa CSRF", func(t *testing.T) {
		f := newFixture(t)
		f.login()
		seedStorageContent(f)

		rec := f.postRaw("/storage/items/500/delete", url.Values{"confirm": {"DELETE"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, mau 403 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":2007`) {
			t.Fatalf("body = %s, mau envelope code 2007", rec.Body.String())
		}
		if got := countByID(f, t, "generation_items", 500); got != 1 {
			t.Fatalf("generation_items 500 terhapus tanpa CSRF")
		}
	})

	t.Run("tanpa login", func(t *testing.T) {
		f := newFixture(t)
		seedStorageContent(f)
		f.get("/login")

		rec := f.do(http.MethodPost, "/storage/items/500/delete", url.Values{
			"_csrf":   {f.csrf()},
			"confirm": {"DELETE"},
		}, "")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Fatalf("Location = %q, mau /login", loc)
		}
		if got := countByID(f, t, "generation_items", 500); got != 1 {
			t.Fatalf("generation_items 500 terhapus tanpa login")
		}
	})
}

// TestStoragePageShowsPurgeAllButton memastikan halaman /storage memuat tombol
// "Hapus semua konten" yang mengirim POST /content/purge dengan confirm=PURGE.
func TestStoragePageShowsPurgeAllButton(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.get("/storage")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /storage: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Hapus semua konten",
		`action="/content/purge"`,
		`value="PURGE"`,
		"Menghapus semua konten dan storage. Tindakan ini tidak bisa dibatalkan.",
		"return confirm(",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman /storage tidak memuat %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "<script src") {
		t.Fatalf("halaman /storage memuat skrip dari CDN/eksternal")
	}
}
