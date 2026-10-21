package admin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// generationItemSeed adalah satu baris generation_items untuk test progres per
// series pada halaman /sync/{id}.
type generationItemSeed struct {
	GenerationID     int64
	Title            string
	ExternalID       string
	Category         string
	Status           string
	TotalEpisodes    int64
	EpisodesDone     int64
	EpisodesFailed   int64
	RenditionsReady  int64
	RenditionsFailed int64
	Bytes            int64
	Percent          int
	Step             string
	Episode          string
	ErrorMessage     string
}

// nullableText mengirim NULL ke SQLite bila nilainya kosong, sehingga test juga
// mencakup kolom generation_items yang boleh NULL.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// seedGenerationItem menambahkan satu baris generation_items pada fixture.
func (f *fixture) seedGenerationItem(item generationItemSeed) {
	f.t.Helper()
	f.exec(`INSERT INTO generation_items (generation_id, provider_id, category, series_external_id,
	                                      series_title, status, total_episodes, episodes_done,
	                                      episodes_failed, renditions_ready, renditions_failed,
	                                      bytes_downloaded, progress_percent, current_step,
	                                      current_episode, error_message)
	        VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.GenerationID, item.Category, item.ExternalID, item.Title, item.Status,
		item.TotalEpisodes, item.EpisodesDone, item.EpisodesFailed,
		item.RenditionsReady, item.RenditionsFailed, item.Bytes, item.Percent,
		nullableText(item.Step), nullableText(item.Episode), nullableText(item.ErrorMessage))
}

// seedTwoGenerationItems mengisi generation 42 (milik run #1) dengan dua series:
// satu selesai dan satu masih berjalan. Totalnya 16 dari 20 episode selesai,
// sehingga persentase berbobot episode = 80%.
func seedTwoGenerationItems(f *fixture) {
	f.seedGenerationItem(generationItemSeed{
		GenerationID:    42,
		Title:           "Cinta Pertama",
		ExternalID:      "cinta-pertama",
		Category:        "short",
		Status:          "done",
		TotalEpisodes:   12,
		EpisodesDone:    12,
		RenditionsReady: 36,
		Bytes:           1048576,
		Percent:         100,
		Step:            "done",
		Episode:         "E12",
	})
	f.seedGenerationItem(generationItemSeed{
		GenerationID:     42,
		Title:            "Rahasia Malam",
		ExternalID:       "rahasia-malam",
		Category:         "short",
		Status:           "running",
		TotalEpisodes:    8,
		EpisodesDone:     4,
		EpisodesFailed:   1,
		RenditionsReady:  12,
		RenditionsFailed: 1,
		Bytes:            524288,
		Percent:          50,
		Step:             "segments",
		Episode:          "E05",
		ErrorMessage:     "sumber tidak tersedia",
	})
}

// generationItemsEnvelope adalah envelope JSON GET /sync/{id}/items.
type generationItemsEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Items []struct {
			SeriesTitle      string `json:"series_title"`
			SeriesExternalID string `json:"series_external_id"`
			Category         string `json:"category"`
			Status           string `json:"status"`
			TotalEpisodes    int64  `json:"total_episodes"`
			EpisodesDone     int64  `json:"episodes_done"`
			EpisodesFailed   int64  `json:"episodes_failed"`
			RenditionsReady  int64  `json:"renditions_ready"`
			RenditionsFailed int64  `json:"renditions_failed"`
			BytesDownloaded  int64  `json:"bytes_downloaded"`
			ProgressPercent  int    `json:"progress_percent"`
			CurrentStep      string `json:"current_step"`
			CurrentEpisode   string `json:"current_episode"`
			ErrorMessage     string `json:"error_message"`
		} `json:"items"`
		Summary struct {
			SeriesTotal   int   `json:"series_total"`
			SeriesDone    int   `json:"series_done"`
			EpisodesDone  int64 `json:"episodes_done"`
			EpisodesTotal int64 `json:"episodes_total"`
			Percent       int   `json:"progress_percent"`
		} `json:"summary"`
	} `json:"data"`
}

// TestSyncDetailGenerationItems memastikan halaman /sync/{id} menampilkan tabel
// progres per series beserta ringkasannya.
func TestSyncDetailGenerationItems(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedTwoGenerationItems(f)

	rec := f.get("/sync/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, mau text/html", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"<h2>Konten</h2>",
		`<div id="sync-items" data-run-id="1" data-active="0" data-status="completed">`,
		"Cinta Pertama",
		"cinta-pertama",
		"Rahasia Malam",
		"rahasia-malam",
		"short",
		"Kategori",
		"Langkah",
		"<div class=\"table-wrap\">",
		`<progress max="100" value="100">`,
		`<progress max="100" value="50">`,
		"1/2 series selesai",
		"16/20 episode selesai",
		"80% keseluruhan",
		"segments",
		"E05",
		"sumber tidak tersedia",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman /sync/1 tidak memuat %q", want)
		}
	}

	// Run #1 sudah completed: tidak ada lagi polling aktif pada halaman.
	if strings.Contains(body, "window.setInterval") {
		t.Fatalf("run terminal tidak boleh memasang polling")
	}
}

// TestSyncItemsJSON memastikan GET /sync/{id}/items menjawab envelope sukses
// berisi items dan summary saat klien meminta JSON.
func TestSyncItemsJSON(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedTwoGenerationItems(f)

	// Run #1 berstatus completed (terminal) dan tetap dijawab 200.
	rec := f.do(http.MethodGet, "/sync/1/items", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, mau application/json", ct)
	}

	var body generationItemsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body bukan JSON: %v (body=%s)", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code = %d, mau 0 (body=%s)", body.Code, rec.Body.String())
	}
	if len(body.Data.Items) != 2 {
		t.Fatalf("jumlah item = %d, mau 2", len(body.Data.Items))
	}

	first := body.Data.Items[0]
	if first.SeriesTitle != "Cinta Pertama" || first.SeriesExternalID != "cinta-pertama" {
		t.Fatalf("item pertama = (%q, %q), mau (Cinta Pertama, cinta-pertama)",
			first.SeriesTitle, first.SeriesExternalID)
	}
	if first.Category != "short" || first.Status != "done" {
		t.Fatalf("item pertama category/status = (%q, %q), mau (short, done)", first.Category, first.Status)
	}
	if first.TotalEpisodes != 12 || first.EpisodesDone != 12 || first.RenditionsReady != 36 {
		t.Fatalf("item pertama episode/rendition = (%d, %d, %d), mau (12, 12, 36)",
			first.TotalEpisodes, first.EpisodesDone, first.RenditionsReady)
	}
	if first.ProgressPercent != 100 {
		t.Fatalf("item pertama progress = %d, mau 100", first.ProgressPercent)
	}

	second := body.Data.Items[1]
	if second.SeriesTitle != "Rahasia Malam" || second.EpisodesFailed != 1 {
		t.Fatalf("item kedua = (%q, gagal %d), mau (Rahasia Malam, gagal 1)",
			second.SeriesTitle, second.EpisodesFailed)
	}
	if second.CurrentStep != "segments" || second.CurrentEpisode != "E05" {
		t.Fatalf("item kedua langkah = (%q, %q), mau (segments, E05)",
			second.CurrentStep, second.CurrentEpisode)
	}
	if second.ErrorMessage != "sumber tidak tersedia" {
		t.Fatalf("item kedua error = %q", second.ErrorMessage)
	}

	summary := body.Data.Summary
	if summary.SeriesTotal != 2 || summary.SeriesDone != 1 {
		t.Fatalf("summary series = %d/%d, mau 1/2", summary.SeriesDone, summary.SeriesTotal)
	}
	if summary.EpisodesDone != 16 || summary.EpisodesTotal != 20 {
		t.Fatalf("summary episode = %d/%d, mau 16/20", summary.EpisodesDone, summary.EpisodesTotal)
	}
	if summary.Percent != 80 {
		t.Fatalf("summary persen = %d, mau 80", summary.Percent)
	}
}

// TestSyncItemsHTMLPartial memastikan permintaan tanpa Accept JSON menerima
// fragmen HTML (tanpa layout) yang siap menggantikan container.
func TestSyncItemsHTMLPartial(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedTwoGenerationItems(f)

	rec := f.do(http.MethodGet, "/sync/1/items", nil, "text/html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, mau text/html", ct)
	}

	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<header") {
		t.Fatalf("fragmen tidak boleh memuat layout (body=%s)", body)
	}
	for _, want := range []string{
		`<div id="sync-items" data-run-id="1" data-active="0" data-status="completed">`,
		"<div class=\"table-wrap\">",
		"Cinta Pertama",
		"Rahasia Malam",
		"80% keseluruhan",
		`<progress max="100" value="50">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragmen item tidak memuat %q", want)
		}
	}
}

// TestSyncItemsActiveRun menandai container dengan data-active="1" saat run
// masih berjalan, dan halaman tetap memasang skrip polling lokal.
func TestSyncItemsActiveRun(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedTwoGenerationItems(f)
	f.exec(`INSERT INTO sync_runs (id, generation_id, trigger_source, scope, status, phase, created_at)
	        VALUES (6, 42, 'manual', 'full', 'running', 'segments', '2026-10-20T11:00:00Z')`)

	page := f.get("/sync/6")
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, `<div id="sync-items" data-run-id="6" data-active="1" data-status="running">`) {
		t.Fatalf("container item tidak menandai run aktif (body=%s)", body)
	}
	for _, want := range []string{"window.setInterval", "/items", "'Accept': 'text/html'"} {
		if !strings.Contains(body, want) {
			t.Fatalf("skrip polling tidak memuat %q", want)
		}
	}
	if strings.Contains(body, "//cdn.") || strings.Contains(body, "https://") {
		t.Fatalf("halaman tidak boleh memuat sumber eksternal/CDN")
	}

	// Endpoint partial untuk run aktif juga menandai data-active="1".
	partial := f.do(http.MethodGet, "/sync/6/items", nil, "text/html")
	if partial.Code != http.StatusOK {
		t.Fatalf("partial status = %d, mau 200", partial.Code)
	}
	if !strings.Contains(partial.Body.String(), `data-active="1"`) {
		t.Fatalf("fragmen item run aktif tidak memuat data-active=1")
	}
}

// TestSyncItemsEmpty memastikan halaman dan endpoint partial tetap 200 dengan
// pesan "Belum ada item konten" bila generation belum punya item.
func TestSyncItemsEmpty(t *testing.T) {
	f := newFixture(t)
	f.login()

	// Run #1 punya generation (42) tetapi belum ada baris generation_items.
	page := f.get("/sync/1")
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Belum ada item konten") {
		t.Fatalf("halaman tidak memuat pesan kosong (body=%s)", page.Body.String())
	}

	partial := f.do(http.MethodGet, "/sync/1/items", nil, "text/html")
	if partial.Code != http.StatusOK {
		t.Fatalf("partial status = %d, mau 200", partial.Code)
	}
	if !strings.Contains(partial.Body.String(), "Belum ada item konten") {
		t.Fatalf("fragmen tidak memuat pesan kosong (body=%s)", partial.Body.String())
	}

	rec := f.do(http.MethodGet, "/sync/1/items", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("json status = %d, mau 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("items harus array kosong, bukan null (body=%s)", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"series_total":0`) {
		t.Fatalf("summary kosong tidak dikirim (body=%s)", rec.Body.String())
	}
}

// TestSyncDetailWithoutGeneration memastikan run tanpa generation_id tetap
// menampilkan halaman 200 dengan pesan kosong, bukan 500.
func TestSyncDetailWithoutGeneration(t *testing.T) {
	f := newFixture(t)
	f.login()
	f.exec(`INSERT INTO sync_runs (id, generation_id, trigger_source, scope, status, created_at)
	        VALUES (5, NULL, 'manual', 'full', 'failed', '2026-10-20T11:00:00Z')`)

	page := f.get("/sync/5")
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, "Belum ada item konten") {
		t.Fatalf("halaman tanpa generation tidak memuat pesan kosong (body=%s)", body)
	}
	if !strings.Contains(body, `<div id="sync-items" data-run-id="5" data-active="0" data-status="failed">`) {
		t.Fatalf("container item tidak dirender untuk run tanpa generation (body=%s)", body)
	}

	rec := f.do(http.MethodGet, "/sync/5/items", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("json status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("items harus array kosong (body=%s)", rec.Body.String())
	}
}

// TestSyncItemsRequiresSession memastikan kedua endpoint progres per series
// dilindungi middleware admin: 303 ke /login untuk navigasi HTML dan 401 untuk
// klien JSON.
func TestSyncItemsRequiresSession(t *testing.T) {
	f := newFixture(t)
	seedTwoGenerationItems(f)

	page := f.get("/sync/1/items")
	if page.Code != http.StatusSeeOther {
		t.Fatalf("tanpa sesi (HTML): status = %d, mau 303 (body=%s)", page.Code, page.Body.String())
	}
	if loc := page.Header().Get("Location"); loc != "/login" {
		t.Fatalf("tanpa sesi (HTML): Location = %q, mau /login", loc)
	}

	detail := f.get("/sync/1")
	if detail.Code != http.StatusSeeOther {
		t.Fatalf("tanpa sesi (detail): status = %d, mau 303", detail.Code)
	}

	rec := f.do(http.MethodGet, "/sync/1/items", nil, "application/json")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa sesi (JSON): status = %d, mau 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":2005`) {
		t.Fatalf("tanpa sesi (JSON): body = %s, mau envelope CodeSessionExpired", rec.Body.String())
	}
}
