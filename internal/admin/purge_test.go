package admin_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vodlocal/internal/admin"
)

// bareMedia hanya memenuhi admin.MediaManager (tanpa Root), seperti pengelola
// media lain yang tidak menyediakan akar direktori.
type bareMedia struct {
	mu      sync.Mutex
	removed []string
}

func (m *bareMedia) Remove(label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, label)
	return nil
}

func (m *bareMedia) removedLabels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// withMedia mengganti pengelola media panel pada fixture.
func withMedia(media admin.MediaManager) fixtureOption {
	return func(cfg *admin.Config) {
		cfg.Media = media
	}
}

// tableCount menghitung seluruh baris sebuah tabel.
func tableCount(f *fixture, t *testing.T, table string) int {
	t.Helper()

	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("hitung %s: %v", table, err)
	}
	return count
}

// makeArtworkFile membuat satu berkas artwork di bawah root dan mengembalikan
// path-nya.
func makeArtworkFile(t *testing.T, root, rel string) string {
	t.Helper()

	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("buat direktori artwork %s: %v", filepath.Dir(file), err)
	}
	if err := os.WriteFile(file, []byte("poster"), 0o644); err != nil {
		t.Fatalf("tulis artwork %s: %v", file, err)
	}
	return file
}

// TestContentPurge memastikan aksi "Remove all batch" menghapus seluruh
// storage dan katalog: folder generation (termasuk yang tidak tercatat),
// baris generations beserta katalog turunannya, dan cache artwork — tanpa
// menyentuh provider, perangkat, atau pengguna.
func TestContentPurge(t *testing.T) {
	f := newFixture(t)
	f.login()

	// Sisakan dua generation: retired 41 dan active 42.
	f.exec(`DELETE FROM generations WHERE id = 40`)

	generationDirs := make([]string, 0, 2)
	for _, label := range []string{"gen-000041", "gen-000042"} {
		generationDirs = append(generationDirs, makeMediaDir(t, f.mediaRoot, label))
	}
	leftovers := []string{
		makeMediaDir(t, f.mediaRoot, ".building-gen-000077"),
		makeMediaDir(t, f.mediaRoot, "gen-000099"),
	}
	// Direktori lain di media root bukan milik generation, jadi tidak disapu.
	foreign := makeMediaDir(t, f.mediaRoot, "artwork-cache")

	artwork := makeArtworkFile(t, f.artworkRoot, "dens_tv/series/1/poster.jpg")

	// Tabel di luar storage/katalog harus tetap utuh setelah purge.
	untouched := make(map[string]int, 5)
	for _, table := range []string{"providers", "devices", "auth_users", "sync_runs", "app_settings"} {
		untouched[table] = tableCount(f, t, table)
	}
	if untouched["providers"] != 1 || untouched["devices"] != 2 {
		t.Fatalf("data awal tidak sesuai: providers=%d devices=%d", untouched["providers"], untouched["devices"])
	}

	rec := f.post("/content/purge", url.Values{"confirm": {"PURGE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("purge: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/sync" {
		t.Fatalf("Location = %q, mau /sync", loc)
	}

	// Folder generation dan sisa direktori hilang dari disk.
	for _, dir := range append(generationDirs, leftovers...) {
		requireNotExist(t, dir)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("direktori non-generation ikut terhapus: %v", err)
	}

	// Database: seluruh batch dan katalog turunannya kosong.
	for _, table := range []string{"generations", "titles", "episodes", "playables", "renditions"} {
		if got := tableCount(f, t, table); got != 0 {
			t.Errorf("%s = %d baris, mau 0 setelah purge", table, got)
		}
	}

	// Tabel di luar storage/katalog tidak boleh tersentuh.
	for table, before := range untouched {
		if got := tableCount(f, t, table); got != before {
			t.Errorf("%s = %d baris, mau %d (tidak boleh berubah)", table, got, before)
		}
	}

	// Artwork root dikosongkan tetapi tetap ada.
	requireNotExist(t, artwork)
	entries, err := os.ReadDir(f.artworkRoot)
	if err != nil {
		t.Fatalf("artwork root tidak ada setelah purge: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("artwork root masih berisi %d entri, mau kosong", len(entries))
	}

	// Flash sukses menyebut jumlah generation dan baris katalog.
	page := f.get("/sync")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /sync: status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{"Semua batch dihapus", "2 generation", "baris katalog"} {
		if !strings.Contains(body, want) {
			t.Fatalf("flash purge tidak memuat %q (body=%s)", want, body)
		}
	}
}

// TestContentPurgeMenolakRunAktif memastikan purge ditolak 409 selama masih ada
// sync run non-terminal dan tidak ada satu pun media/database yang dihapus.
func TestContentPurgeMenolakRunAktif(t *testing.T) {
	f := newFixture(t)
	f.login()

	dir := makeMediaDir(t, f.mediaRoot, "gen-000042")
	artwork := makeArtworkFile(t, f.artworkRoot, "dens_tv/series/1/poster.jpg")

	f.exec(`INSERT INTO sync_runs (id, trigger_source, scope, status, phase, created_at)
	        VALUES (9, 'manual', 'full', 'running', 'segments', '2026-10-20T11:00:00Z')`)

	rec := f.post("/content/purge", url.Values{"confirm": {"PURGE"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, mau 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Masih ada proses sync/ingest berjalan.") {
		t.Fatalf("pesan konflik tidak tampil (body=%s)", rec.Body.String())
	}

	// Tidak ada yang terhapus.
	if got := f.media.removedLabels(); len(got) != 0 {
		t.Fatalf("Media.Remove dipanggil walau run aktif: %v", got)
	}
	if got := tableCount(f, t, "generations"); got != 3 {
		t.Errorf("generations = %d, mau 3 (tidak ada yang dihapus)", got)
	}
	if got := tableCount(f, t, "titles"); got != 2 {
		t.Errorf("titles = %d, mau 2 (tidak ada yang dihapus)", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("folder generation ikut terhapus: %v", err)
	}
	if _, err := os.Stat(artwork); err != nil {
		t.Errorf("artwork ikut terhapus: %v", err)
	}
}

// TestContentPurgeKonfirmasiSalah memastikan field confirm wajib bernilai PURGE.
func TestContentPurgeKonfirmasiSalah(t *testing.T) {
	f := newFixture(t)
	f.login()

	for name, form := range map[string]url.Values{
		"nilai lain":  {"confirm": {"hapus"}},
		"tanpa field": {},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.post("/content/purge", form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, mau 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "confirm=PURGE") {
				t.Fatalf("pesan konfirmasi tidak tampil (body=%s)", rec.Body.String())
			}
			if got := tableCount(f, t, "generations"); got != 3 {
				t.Fatalf("generations = %d, mau 3 (tidak ada yang dihapus)", got)
			}
		})
	}
	if got := f.media.removedLabels(); len(got) != 0 {
		t.Fatalf("Media.Remove dipanggil walau konfirmasi salah: %v", got)
	}
}

// TestContentPurgePerluCSRFDanLogin memastikan purge tetap di balik middleware
// login dan token CSRF double-submit.
func TestContentPurgePerluCSRFDanLogin(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.postRaw("/content/purge", url.Values{"confirm": {"PURGE"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tanpa CSRF: status = %d, mau 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":2007`) {
		t.Fatalf("tanpa CSRF: body = %s, mau envelope code 2007", rec.Body.String())
	}
	if got := tableCount(f, t, "generations"); got != 3 {
		t.Fatalf("tanpa CSRF: generations = %d, mau 3", got)
	}

	// Tanpa login: middleware mengalihkan ke /login sebelum handler berjalan.
	anon := newFixture(t)
	anon.get("/login")
	rec = anon.do(http.MethodPost, "/content/purge", url.Values{
		"_csrf":   {anon.csrf()},
		"confirm": {"PURGE"},
	}, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tanpa login: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("tanpa login: Location = %q, mau /login", loc)
	}
	if got := tableCount(anon, t, "generations"); got != 3 {
		t.Fatalf("tanpa login: generations = %d, mau 3", got)
	}
}

// TestContentPurgeTanpaArtworkRoot memastikan ArtworkRoot opsional: purge tetap
// berhasil walau panel tidak dikonfigurasi dengan akar artwork.
func TestContentPurgeTanpaArtworkRoot(t *testing.T) {
	f := newFixture(t, withoutArtworkRoot())
	f.login()

	rec := f.post("/content/purge", url.Values{"confirm": {"PURGE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := tableCount(f, t, "generations"); got != 0 {
		t.Fatalf("generations = %d, mau 0", got)
	}
}

// TestContentPurgeTanpaMediaRooter memastikan purge tetap berjalan pada
// pengelola media yang tidak menyediakan Root(): semua generation dihapus lewat
// Remove, hanya sapuan direktori sisa yang dilewati.
func TestContentPurgeTanpaMediaRooter(t *testing.T) {
	bare := &bareMedia{}
	f := newFixture(t, withMedia(bare))
	f.login()

	rec := f.post("/content/purge", url.Values{"confirm": {"PURGE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := bare.removedLabels(); len(got) != 3 {
		t.Fatalf("Media.Remove dipanggil dengan %v, mau 3 label generation", got)
	}
	if got := tableCount(f, t, "generations"); got != 0 {
		t.Fatalf("generations = %d, mau 0", got)
	}
	if got := tableCount(f, t, "titles"); got != 0 {
		t.Fatalf("titles = %d, mau 0", got)
	}
}

// TestSyncPageShowsRemoveAllBatch memastikan modal "List Content" memuat tombol
// "Remove all batch" yang mengirim POST /content/purge dengan confirm=PURGE.
func TestSyncPageShowsRemoveAllBatch(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.get("/sync")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sync: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Remove all batch",
		`id="purge-content"`,
		"/content/purge",
		"window.confirm('Hapus SEMUA batch dan katalog? Tindakan ini tidak bisa dibatalkan.')",
		"form.method = 'post'",
		"confirmation.value = 'PURGE'",
		"content-dialog",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman sync tidak memuat %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "<script src") {
		t.Fatalf("halaman sync memuat skrip dari CDN/eksternal")
	}
}
