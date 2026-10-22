package admin_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"vodlocal/internal/admin"
	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

// envelope adalah bentuk response JSON standar.
type envelope struct {
	Code   int               `json:"code"`
	Msg    string            `json:"msg"`
	Data   json.RawMessage   `json:"data"`
	Errors []response.Detail `json:"errors"`
}

// TestSeedAdmin memastikan pengguna admin awal dibuat dengan hash (bukan
// plaintext), idempoten, dan menolak kata sandi kosong.
func TestSeedAdmin(t *testing.T) {
	f := newFixture(t)

	var (
		username, hash, role string
		isActive             int
	)
	err := f.db.QueryRow(`SELECT username, password_hash, role, is_active FROM auth_users`).
		Scan(&username, &hash, &role, &isActive)
	if err != nil {
		t.Fatalf("baca auth_users: %v", err)
	}
	if username != testUsername || role != "admin" || isActive != 1 {
		t.Fatalf("user = (%q, %q, %d), mau (%q, admin, 1)", username, role, isActive, testUsername)
	}
	if hash == testPassword {
		t.Fatalf("password_hash sama dengan plaintext")
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$210000$") {
		t.Fatalf("password_hash = %q, mau berformat pbkdf2-sha256$210000$...", hash)
	}
	if !admin.VerifyPassword(hash, testPassword) {
		t.Fatalf("VerifyPassword menolak kata sandi yang benar")
	}
	if admin.VerifyPassword(hash, "salah") {
		t.Fatalf("VerifyPassword menerima kata sandi salah")
	}

	// Idempoten: pemanggilan kedua tidak menambah baris.
	if err := f.admin.SeedAdmin(t.Context(), "admin-lain", "sandi-lain"); err != nil {
		t.Fatalf("SeedAdmin kedua: %v", err)
	}
	var total int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM auth_users`).Scan(&total); err != nil {
		t.Fatalf("hitung auth_users: %v", err)
	}
	if total != 1 {
		t.Fatalf("jumlah user = %d, mau 1 (SeedAdmin harus idempoten)", total)
	}

	// Kata sandi kosong selalu error, walaupun tabel sudah berisi.
	if err := f.admin.SeedAdmin(t.Context(), "admin-baru", ""); err == nil {
		t.Fatalf("SeedAdmin dengan kata sandi kosong tidak error")
	} else if !strings.Contains(err.Error(), "kata sandi") {
		t.Fatalf("error = %v, mau menyebut kata sandi", err)
	}
}

// TestLogin memeriksa login sukses (303 + cookie sesi) dan login gagal (401
// tanpa cookie sesi, pesan tidak membocorkan keberadaan pengguna).
func TestLogin(t *testing.T) {
	f := newFixture(t)

	f.get("/login")
	rec := f.post("/login", url.Values{
		"username": {testUsername},
		"password": {testPassword},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("login: Location = %q, mau /", loc)
	}
	if !f.hasSession() {
		t.Fatalf("login: cookie %s tidak dikirim", httpserver.SessionCookieName)
	}

	var lastLogin sql.NullString
	if err := f.db.QueryRow(`SELECT last_login_at FROM auth_users WHERE username = ?`, testUsername).
		Scan(&lastLogin); err != nil {
		t.Fatalf("baca last_login_at: %v", err)
	}
	if !lastLogin.Valid || lastLogin.String == "" {
		t.Fatalf("last_login_at = %v, mau terisi setelah login", lastLogin)
	}

	// Kata sandi salah.
	bad := newFixture(t)
	bad.get("/login")
	rec = bad.post("/login", url.Values{
		"username": {testUsername},
		"password": {"kata-sandi-salah"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login salah: status = %d, mau 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if bad.hasSession() {
		t.Fatalf("login salah: cookie sesi tidak boleh dikirim")
	}
	if !strings.Contains(rec.Body.String(), "Nama pengguna atau kata sandi salah") {
		t.Fatalf("login salah: pesan error tidak tampil (body=%s)", rec.Body.String())
	}

	// Pengguna tidak ada: pesan dan status harus sama.
	rec = bad.post("/login", url.Values{
		"username": {"tidak-ada"},
		"password": {"apa-saja"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("user tidak ada: status = %d, mau 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nama pengguna atau kata sandi salah") {
		t.Fatalf("user tidak ada: pesan error membocorkan perbedaan (body=%s)", rec.Body.String())
	}
}

// TestDashboardRequiresSession memastikan halaman admin mengalihkan ke /login
// tanpa sesi.
func TestDashboardRequiresSession(t *testing.T) {
	f := newFixture(t)

	rec := f.get("/")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location = %q, mau /login", loc)
	}
}

// TestDashboard memastikan dashboard menampilkan statistik katalog tanpa
// section ringkasan batch media.
func TestDashboard(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.get("/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, mau text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, mau no-store", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"Rendition siap", // ringkasan rendition siap
		"Katalog",        // bagian katalog
		"Sinkronisasi terakhir",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard tidak memuat %q", want)
		}
	}
	for _, unwanted := range []string{"Batch media terbaru", "gen-000042"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dashboard masih memuat section batch %q", unwanted)
		}
	}
}

// TestPostWithoutCSRF memastikan POST tanpa field _csrf ditolak 403.
func TestPostWithoutCSRF(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.postRaw("/logout", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, mau 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":2007`) {
		t.Fatalf("body = %s, mau envelope code 2007", rec.Body.String())
	}
	if !f.hasSession() {
		t.Fatalf("sesi tidak boleh dihapus saat CSRF gagal")
	}

	// Dengan token CSRF yang benar, logout berhasil.
	rec = f.post("/logout", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: status = %d, mau 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("logout: Location = %q, mau /login", loc)
	}
	if f.hasSession() {
		t.Fatalf("logout: cookie sesi masih ada")
	}
}

// TestSyncStart memastikan POST /sync menjalankan Syncer di goroutine dan
// menolak 409 bila sudah ada run aktif di database.
func TestSyncStart(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.post("/sync", url.Values{"scope": {"full"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /sync: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/sync" {
		t.Fatalf("POST /sync: Location = %q, mau /sync", loc)
	}

	select {
	case trigger := <-f.syncer.started:
		if trigger != "manual" {
			t.Fatalf("trigger = %q, mau manual", trigger)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Syncer.Run tidak dipanggil")
	}

	// Run aktif di database menolak permintaan berikutnya.
	f.exec(`INSERT INTO sync_runs (id, trigger_source, scope, status, phase, created_at)
	        VALUES (7, 'manual', 'full', 'running', 'segments', '2026-10-20T11:00:00Z')`)

	rec = f.post("/sync", url.Values{"scope": {"full"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("run aktif: status = %d, mau 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Masih ada sinkronisasi") {
		t.Fatalf("run aktif: pesan tidak tampil (body=%s)", rec.Body.String())
	}
	if got := f.syncer.triggersSeen(); len(got) != 1 {
		t.Fatalf("Syncer.Run dipanggil %d kali, mau 1", len(got))
	}
}

// TestSyncCancel memastikan POST /sync/{id}/cancel meneruskan ID ke Syncer.
func TestSyncCancel(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.post("/sync/1/cancel", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/sync/1" {
		t.Fatalf("Location = %q, mau /sync/1", loc)
	}
	if got := f.syncer.cancelledRuns(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Cancel dipanggil dengan %v, mau [1]", got)
	}
}

var deviceTokenPattern = regexp.MustCompile(`mc_[A-Za-z0-9_-]{43}`)

// TestDeviceActions memeriksa disable/enable dan rotasi token perangkat.
func TestDeviceActions(t *testing.T) {
	f := newFixture(t)
	f.login()

	// Disable.
	rec := f.post("/devices/1/disable", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := deviceActive(f, t, 1); got != 0 {
		t.Fatalf("is_active = %d, mau 0 setelah disable", got)
	}

	// Enable.
	rec = f.post("/devices/1/enable", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("enable: status = %d, mau 303", rec.Code)
	}
	if got := deviceActive(f, t, 1); got != 1 {
		t.Fatalf("is_active = %d, mau 1 setelah enable", got)
	}

	// Rotate token.
	var before string
	if err := f.db.QueryRow(`SELECT token_hash FROM devices WHERE id = 1`).Scan(&before); err != nil {
		t.Fatalf("baca token_hash: %v", err)
	}
	rec = f.post("/devices/1/rotate-token", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	var after string
	if err := f.db.QueryRow(`SELECT token_hash FROM devices WHERE id = 1`).Scan(&after); err != nil {
		t.Fatalf("baca token_hash baru: %v", err)
	}
	if after == before {
		t.Fatalf("token_hash tidak berubah setelah rotasi")
	}

	// Token baru hanya ditampilkan sekali lewat flash message.
	page := f.get("/devices")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /devices: status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	token := deviceTokenPattern.FindString(page.Body.String())
	if token == "" {
		t.Fatalf("token baru tidak ditampilkan di halaman perangkat")
	}
	if hashToken(token) != after {
		t.Fatalf("token yang ditampilkan bukan token tersimpan")
	}
	if strings.Contains(f.get("/devices").Body.String(), token) {
		t.Fatalf("token ditampilkan lebih dari sekali")
	}

	// Filter dan pencarian tetap berfungsi.
	list := f.get("/devices?q=Kursi&status=online&sort=name")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /devices?q: status = %d", list.Code)
	}
	if !strings.Contains(list.Body.String(), "Tablet Kursi 12") {
		t.Fatalf("pencarian perangkat tidak menemukan data")
	}
}

// deviceActive membaca devices.is_active.
func deviceActive(f *fixture, t *testing.T, id int64) int {
	t.Helper()
	var active int
	if err := f.db.QueryRow(`SELECT is_active FROM devices WHERE id = ?`, id).Scan(&active); err != nil {
		t.Fatalf("baca is_active: %v", err)
	}
	return active
}

// TestStorageDelete memastikan generation berstatus apa pun boleh dihapus —
// termasuk yang dulu ditolak seperti active — selama confirm=DELETE dikirim,
// dan Media.Remove dipanggil untuk labelnya.
func TestStorageDelete(t *testing.T) {
	f := newFixture(t)
	f.login()

	// retired → dihapus.
	rec := f.post("/storage/generations/41/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus retired: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if count := generationCount(f, t, 41); count != 0 {
		t.Fatalf("baris generation 41 masih ada")
	}
	if got := f.media.removedLabels(); len(got) != 1 || got[0] != "gen-000041" {
		t.Fatalf("Media.Remove dipanggil dengan %v, mau [gen-000041]", got)
	}

	// confirm bukan DELETE → 400 dan baris tetap ada (berlaku juga untuk active).
	rec = f.post("/storage/generations/42/delete", url.Values{"confirm": {"hapus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm salah: status = %d, mau 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if count := generationCount(f, t, 42); count != 1 {
		t.Fatalf("generation aktif terhapus tanpa konfirmasi")
	}
	if got := f.media.removedLabels(); len(got) != 1 {
		t.Fatalf("Media.Remove dipanggil tanpa konfirmasi: %v", got)
	}

	// failed → tetap boleh dihapus seperti sebelumnya.
	rec = f.post("/storage/generations/40/delete", url.Values{"confirm": {"DELETE"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hapus failed: status = %d, mau 303", rec.Code)
	}

	// Halaman /storage sekarang mendaftar konten, bukan generation: label
	// generation masih tampil sebagai ringkasan, tetapi tombol hapus per batch
	// sudah tidak ada.
	page := f.get("/storage")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /storage: status = %d (body=%s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, "gen-000042") {
		t.Fatalf("halaman storage tidak memuat generation aktif")
	}
	if strings.Contains(body, "/storage/generations/42/delete") {
		t.Fatalf("halaman storage masih memuat tombol hapus per batch")
	}
	if !strings.Contains(body, "/content/purge") {
		t.Fatalf("halaman storage tidak memuat tombol Hapus semua konten")
	}
	if strings.Contains(body, "tidak dapat dihapus") {
		t.Fatalf("halaman storage masih memuat teks 'tidak dapat dihapus'")
	}
	if strings.Contains(body, "Hanya generation retired") {
		t.Fatalf("halaman storage masih memuat aturan generation retired/failed")
	}
}

// generationCount menghitung baris generation dengan ID tertentu.
func generationCount(f *fixture, t *testing.T, id int64) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM generations WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("hitung generation: %v", err)
	}
	return count
}

// TestSettingsSave memastikan pengaturan tersimpan di app_settings dan validasi
// silang menolak kombinasi yang salah.
func TestSettingsSave(t *testing.T) {
	f := newFixture(t)
	f.login()

	page := f.get("/settings")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /settings: status = %d (body=%s)", page.Code, page.Body.String())
	}
	pageBody := page.Body.String()
	for _, want := range []string{`name="sync.max_short_side"`, "<select", "720p"} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("halaman settings tidak memuat %q", want)
		}
	}
	for _, unwanted := range []string{"sync.schedule_enabled", "sync.schedule_cron"} {
		if strings.Contains(pageBody, unwanted) {
			t.Fatalf("halaman settings masih memuat setting jadwal %q", unwanted)
		}
	}

	form := url.Values{
		"sync.max_short_side":                {"720"},
		"sync.max_parallel_segments":         {"4"},
		"storage.warn_percent":               {"70"},
		"storage.block_sync_percent":         {"85"},
		"playback.progress_debounce_seconds": {"15"},
	}

	rec := f.post("/settings", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	want := map[string]string{
		"sync.max_short_side":                "720",
		"sync.max_parallel_segments":         "4",
		"storage.warn_percent":               "70",
		"storage.block_sync_percent":         "85",
		"playback.progress_debounce_seconds": "15",
	}
	for key, expected := range want {
		var got string
		if err := f.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("baca setting %s: %v", key, err)
		}
		if got != expected {
			t.Fatalf("setting %s = %q, mau %q", key, got, expected)
		}
	}

	// Validasi silang: ambang blokir harus di atas ambang peringatan.
	form.Set("storage.block_sync_percent", "70")
	rec = f.post("/settings", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validasi silang: status = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var value string
	if err := f.db.QueryRow(`SELECT value FROM app_settings WHERE key = 'storage.block_sync_percent'`).Scan(&value); err != nil {
		t.Fatalf("baca setting: %v", err)
	}
	if value != "85" {
		t.Fatalf("nilai tersimpan berubah menjadi %q padahal validasi gagal", value)
	}
}

// TestSyncStatus memeriksa bentuk JSON dan HTML dari endpoint status.
func TestSyncStatus(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodGet, "/sync/1/status", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("JSON: Content-Type = %q", ct)
	}

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("JSON: body bukan envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Code != response.CodeSuccess || env.Msg != response.Label(response.CodeSuccess) {
		t.Fatalf("JSON: code/msg = %d/%q, mau 0/Berhasil", env.Code, env.Msg)
	}
	if env.Errors == nil {
		t.Fatalf("JSON: errors bernilai null")
	}

	var payload struct {
		ID              int64  `json:"id"`
		Status          string `json:"status"`
		Phase           string `json:"phase"`
		ProgressPercent int    `json:"progress_percent"`
		TotalRenditions int64  `json:"total_renditions"`
		RenditionsReady int64  `json:"renditions_ready"`
		ETASeconds      int64  `json:"eta_seconds"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("JSON: decode data: %v", err)
	}
	if payload.ID != 1 || payload.Status != "completed" {
		t.Fatalf("JSON: data = %+v, mau id=1 status=completed", payload)
	}
	if payload.ProgressPercent != 40 || payload.RenditionsReady != 4 || payload.TotalRenditions != 10 {
		t.Fatalf("JSON: progress = %+v, mau 40%% (4/10)", payload)
	}

	// Tanpa Accept JSON → HTML partial.
	rec = f.get("/sync/1/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTML: status = %d, mau 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("HTML: Content-Type = %q, mau text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="sync-progress"`) {
		t.Fatalf("HTML: partial tanpa elemen progress (body=%s)", body)
	}
	if !strings.Contains(body, "40%") {
		t.Fatalf("HTML: partial tanpa persentase 40%% (body=%s)", body)
	}
	if strings.Contains(body, "<html") {
		t.Fatalf("HTML: partial tidak boleh memuat layout penuh")
	}

	// Run dengan kegagalan sebagian tetap memakai code 1. sync_tasks sudah
	// dihapus, jadi tidak ada detail unit gagal yang dilaporkan.
	f.exec(`INSERT INTO sync_runs (id, trigger_source, scope, status, phase, total_renditions,
	                               renditions_ready, renditions_failed, started_at, finished_at, created_at)
	        VALUES (2, 'manual', 'full', 'completed', 'done', 10, 3, 2,
	                '2026-10-18T01:00:00Z', '2026-10-18T02:00:00Z', '2026-10-18T01:00:00Z')`)

	rec = f.do(http.MethodGet, "/sync/2/status", nil, "application/json")
	env = envelope{}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("partial: body bukan envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Code != response.CodePartialFailure {
		t.Fatalf("partial: code = %d, mau %d", env.Code, response.CodePartialFailure)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("partial: errors = %+v, mau kosong", env.Errors)
	}

	// Run tidak ada → 404 code 4000.
	rec = f.do(http.MethodGet, "/sync/999/status", nil, "application/json")
	env = envelope{}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not found: body bukan envelope: %v (body=%s)", err, rec.Body.String())
	}
	if rec.Code != http.StatusNotFound || env.Code != response.CodeSyncNotFound {
		t.Fatalf("not found: status/code = %d/%d, mau 404/4000", rec.Code, env.Code)
	}
}

// TestPagesSmoke memastikan seluruh halaman admin merender tanpa error template.
func TestPagesSmoke(t *testing.T) {
	f := newFixture(t)
	f.login()

	pages := []string{"/", "/sync", "/sync/1", "/devices", "/storage", "/settings"}
	for _, page := range pages {
		rec := f.get(page)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, mau 200 (body=%s)", page, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s: Content-Type = %q", page, ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, "_csrf") {
			t.Fatalf("GET %s: tidak memuat token CSRF", page)
		}
	}

	// Halaman yang tidak ada → 404 HTML.
	rec := f.get("/sync/999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /sync/999: status = %d, mau 404", rec.Code)
	}

	// /logs sudah dihapus bersama tabel sync_tasks → 404.
	if rec := f.get("/logs"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /logs: status = %d, mau 404", rec.Code)
	}
}
