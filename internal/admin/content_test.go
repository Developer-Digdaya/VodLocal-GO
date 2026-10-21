package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vodlocal/internal/catalog"
	"vodlocal/internal/response"
	"vodlocal/internal/syncjob"
)

// contentEnvelope adalah bentuk payload GET /content/short.
type contentEnvelope struct {
	Items []struct {
		Provider     string   `json:"provider"`
		Category     string   `json:"category"`
		ID           string   `json:"id"`
		Slug         string   `json:"slug"`
		Title        string   `json:"title"`
		Description  string   `json:"description"`
		PosterURL    string   `json:"poster_url"`
		Year         int      `json:"year"`
		EpisodeCount int      `json:"episode_count"`
		AgeRating    string   `json:"age_rating"`
		Genres       []string `json:"genres"`
		PublishedAt  string   `json:"published_at"`
		AlreadyAdded bool     `json:"already_added"`
	} `json:"items"`
	Meta struct {
		Page     int `json:"page"`
		PerPage  int `json:"per_page"`
		Total    int `json:"total"`
		LastPage int `json:"last_page"`
	} `json:"meta"`
}

// seriesPage membuat halaman series palsu dengan slug "series-<page>-<n>".
func seriesPage(page, perPage, total, lastPage int) catalog.SeriesPage {
	items := make([]catalog.Series, 0, perPage)
	for i := 1; i <= perPage; i++ {
		slug := fmt.Sprintf("series-%d-%d", page, i)
		items = append(items, catalog.Series{
			Provider:     "dramahits",
			Category:     catalog.CategoryShort,
			ID:           fmt.Sprintf("id-%s", slug),
			Slug:         slug,
			Title:        fmt.Sprintf("Series %d-%d", page, i),
			PosterURL:    "https://cdn.example/" + slug + ".jpg",
			Year:         2025,
			EpisodeCount: 10 + i,
			AgeRating:    "13+",
			Genres:       []string{"drama"},
			PublishedAt:  "2025-01-02T00:00:00Z",
		})
	}
	return catalog.SeriesPage{Items: items, Page: page, PerPage: perPage, Total: total, LastPage: lastPage}
}

// TestContentShortList memastikan GET /content/short menjawab envelope JSON
// dengan item dan meta yang benar, meneruskan nomor halaman ke lister, dan
// mengisi cache slug untuk resolusi saat submit.
func TestContentShortList(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
		return seriesPage(page, 3, 7, 3), nil
	}

	rec := f.do(http.MethodGet, "/content/short?page=2", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, mau application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, mau no-store", cc)
	}

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body bukan envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Code != response.CodeSuccess || env.Msg != response.Label(response.CodeSuccess) {
		t.Fatalf("code/msg = %d/%q, mau 0/Berhasil", env.Code, env.Msg)
	}
	if env.Errors == nil {
		t.Fatalf("errors bernilai null")
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors = %+v, mau kosong", env.Errors)
	}

	var payload contentEnvelope
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(payload.Items) != 3 {
		t.Fatalf("jumlah item = %d, mau 3", len(payload.Items))
	}
	first := payload.Items[0]
	if first.Slug != "series-2-1" || first.Title != "Series 2-1" {
		t.Fatalf("item pertama = %+v, mau series-2-1/Series 2-1", first)
	}
	if first.Provider != "dramahits" || first.Category != catalog.CategoryShort {
		t.Fatalf("provider/kategori = %q/%q, mau dramahits/short", first.Provider, first.Category)
	}
	if first.EpisodeCount != 11 {
		t.Fatalf("episode_count = %d, mau 11", first.EpisodeCount)
	}
	if first.PosterURL != "https://cdn.example/series-2-1.jpg" {
		t.Fatalf("poster_url = %q", first.PosterURL)
	}
	if len(first.Genres) != 1 || first.Genres[0] != "drama" {
		t.Fatalf("genres = %v, mau [drama]", first.Genres)
	}
	if payload.Meta.Page != 2 || payload.Meta.PerPage != 3 || payload.Meta.Total != 7 || payload.Meta.LastPage != 3 {
		t.Fatalf("meta = %+v, mau page 2 / per_page 3 / total 7 / last_page 3", payload.Meta)
	}

	// page diteruskan apa adanya, dan halaman tanpa parameter berarti halaman 1.
	rec = f.do(http.MethodGet, "/content/short", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("tanpa page: status = %d, mau 200", rec.Code)
	}
	pages := f.lister.pagesSeen()
	if len(pages) != 2 || pages[0] != 2 || pages[1] != 1 {
		t.Fatalf("halaman diminta = %v, mau [2 1]", pages)
	}

	// Cache terisi: slug dari halaman 1 dan 2 sama-sama bisa diresolusi.
	rec = f.post("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-1", "series-2-1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("select: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	calls := f.ingest.calls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("Ingest.Start dipanggil %d kali dengan %v, mau sekali berisi 2 series", len(calls), calls)
	}
}

// TestContentShortMarksAlreadyAdded memastikan GET /content/short menandai
// item yang sudah punya rendition di database lewat titles.external_id +
// provider code, dan tidak menandai item lain.
func TestContentShortMarksAlreadyAdded(t *testing.T) {
	f := newFixture(t)
	f.login()

	// Provider yang dipakai modal "List Content".
	f.exec(`INSERT INTO providers (id, code, name) VALUES (2, 'dramahits', 'Dramahits')`)

	// series-1-2: title dramahits dengan rendition pada generation aktif (42).
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (500, 2, 'series-1-2', 'series', 'Series 1-2', 'series-1-2')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
	        VALUES (9500, 500, NULL, 600)`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9500, 42, '480p', 'ready', 'dramahits/series/500/480p/index.m3u8')`)

	// series-1-4: rendition hanya ada pada generation retired (41); tetap
	// dihitung sebagai sudah ada karena generation apa pun berlaku.
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (501, 2, 'series-1-4', 'series', 'Series 1-4', 'series-1-4')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
	        VALUES (9501, 501, NULL, 600)`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9501, 41, '480p', 'ready', 'dramahits/series/501/480p/index.m3u8')`)

	// series-1-3: slug sama tetapi milik provider lain (dens_tv), jadi tidak
	// boleh dianggap sudah ada untuk provider dramahits.
	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	        VALUES (502, 1, 'series-1-3', 'series', 'Series 1-3', 'series-1-3')`)
	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
	        VALUES (9502, 502, NULL, 600)`)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, status, playlist_path)
	        VALUES (9502, 42, '480p', 'ready', 'dens_tv/series/502/480p/index.m3u8')`)

	f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
		return seriesPage(page, 4, 4, 1), nil
	}

	rec := f.do(http.MethodGet, "/content/short?page=1", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body bukan envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Code != response.CodeSuccess {
		t.Fatalf("code = %d, mau 0 (body=%s)", env.Code, rec.Body.String())
	}

	// Semua field lama tetap ada bersama already_added.
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		t.Fatalf("decode data mentah: %v", err)
	}
	if len(raw.Items) != 4 {
		t.Fatalf("jumlah item = %d, mau 4", len(raw.Items))
	}
	wantKeys := []string{
		"provider", "category", "id", "slug", "title", "description",
		"poster_url", "year", "episode_count", "age_rating", "genres",
		"published_at", "already_added",
	}
	for _, key := range wantKeys {
		for i, item := range raw.Items {
			if _, ok := item[key]; !ok {
				t.Fatalf("item %d tidak memuat field %q (item=%v)", i, key, item)
			}
		}
	}

	var payload contentEnvelope
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	got := make(map[string]bool, len(payload.Items))
	for _, item := range payload.Items {
		got[item.Slug] = item.AlreadyAdded
	}
	want := map[string]bool{
		"series-1-1": false, // belum ada title sama sekali
		"series-1-2": true,  // rendition pada generation aktif
		"series-1-3": false, // title milik provider lain
		"series-1-4": true,  // rendition pada generation retired
	}
	for slug, expected := range want {
		value, ok := got[slug]
		if !ok {
			t.Fatalf("item %q tidak ada pada payload", slug)
		}
		if value != expected {
			t.Fatalf("already_added[%s] = %t, mau %t", slug, value, expected)
		}
	}

	// Nilai field lama tidak berubah.
	first := payload.Items[0]
	if first.Slug != "series-1-1" || first.Title != "Series 1-1" || first.ID != "id-series-1-1" {
		t.Fatalf("item pertama = %+v, mau id-series-1-1/Series 1-1", first)
	}
	if first.Provider != "dramahits" || first.Category != catalog.CategoryShort {
		t.Fatalf("provider/kategori = %q/%q, mau dramahits/short", first.Provider, first.Category)
	}
	if first.Year != 2025 || first.AgeRating != "13+" || first.PublishedAt != "2025-01-02T00:00:00Z" {
		t.Fatalf("metadata item pertama tidak lengkap: %+v", first)
	}
	if len(first.Genres) != 1 || first.Genres[0] != "drama" {
		t.Fatalf("genres = %v, mau [drama]", first.Genres)
	}
}

// TestContentShortUpstreamError memastikan kegagalan lister dijawab envelope
// error yang sesuai, bukan 200 dengan daftar kosong.
func TestContentShortUpstreamError(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(string, int) (catalog.SeriesPage, error) {
		return catalog.SeriesPage{}, errors.New("connection refused")
	}
	rec := f.do(http.MethodGet, "/content/short?page=1", nil, "application/json")
	if rec.Code != response.HTTPStatus(response.CodeUpstreamUnavailable) {
		t.Fatalf("status = %d, mau %d", rec.Code, response.HTTPStatus(response.CodeUpstreamUnavailable))
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body bukan envelope: %v", err)
	}
	if env.Code != response.CodeUpstreamUnavailable {
		t.Fatalf("code = %d, mau %d", env.Code, response.CodeUpstreamUnavailable)
	}

	// Timeout jelas dipetakan ke kode upstream timeout.
	f.lister.fn = func(string, int) (catalog.SeriesPage, error) {
		return catalog.SeriesPage{}, fmt.Errorf("ambil daftar: %w", context.DeadlineExceeded)
	}
	rec = f.do(http.MethodGet, "/content/short?page=1", nil, "application/json")
	if rec.Code != response.HTTPStatus(response.CodeUpstreamTimeout) {
		t.Fatalf("timeout: status = %d, mau %d", rec.Code, response.HTTPStatus(response.CodeUpstreamTimeout))
	}

	// Cache tidak terisi oleh kegagalan di atas.
	rec = f.post("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-1"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("select setelah error: status = %d, mau 422", rec.Code)
	}
}

// TestContentSelectQueues memastikan POST /content/select memanggil Ingest.Start
// dengan seluruh series terpilih lalu mengalihkan 303 ke /sync dengan flash.
func TestContentSelectQueues(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
		return seriesPage(page, 3, 3, 1), nil
	}
	if rec := f.get("/content/short"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content/short: status = %d, mau 200", rec.Code)
	}

	rec := f.post("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-3", "series-1-1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/sync" {
		t.Fatalf("Location = %q, mau /sync", loc)
	}

	calls := f.ingest.calls()
	if len(calls) != 1 {
		t.Fatalf("Ingest.Start dipanggil %d kali, mau 1", len(calls))
	}
	if len(calls[0]) != 2 {
		t.Fatalf("jumlah selection = %d, mau 2", len(calls[0]))
	}
	if calls[0][0].Slug != "series-1-3" || calls[0][1].Slug != "series-1-1" {
		t.Fatalf("urutan selection = %q,%q, mau series-1-3,series-1-1",
			calls[0][0].Slug, calls[0][1].Slug)
	}
	if calls[0][0].Provider != "dramahits" || calls[0][0].Title != "Series 1-3" {
		t.Fatalf("series terpilih tidak lengkap: %+v", calls[0][0])
	}

	page := f.get("/sync")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /sync: status = %d, mau 200", page.Code)
	}
	if !strings.Contains(page.Body.String(), "2 series dimasukkan ke antrean (run #77)") {
		t.Fatalf("flash tidak tampil (body=%s)", page.Body.String())
	}
}

// TestContentSelectRejectsUnknownSlug memastikan slug yang tidak ada di cache
// dijawab 422 tanpa memanggil worker.
func TestContentSelectRejectsUnknownSlug(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
		return seriesPage(page, 2, 2, 1), nil
	}
	if rec := f.get("/content/short"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content/short: status = %d, mau 200", rec.Code)
	}

	rec := f.post("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-1", "tidak-ada-di-cache"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "tidak-ada-di-cache") {
		t.Fatalf("pesan tidak menyebut slug yang bermasalah (body=%s)", body)
	}
	if got := f.ingest.calls(); len(got) != 0 {
		t.Fatalf("Ingest.Start dipanggil %v, mau tidak dipanggil", got)
	}

	// Tanpa satu pun slug juga ditolak 422.
	rec = f.post("/content/select", url.Values{"provider": {"dramahits"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tanpa slug: status = %d, mau 422", rec.Code)
	}
}

// TestContentSelectAlreadyRunning memastikan error "masih berjalan" dari worker
// dijawab 409 pada halaman /sync, baik berupa sentinel syncjob maupun error
// bertipe worker lain (ingest.ErrAlreadyRunning tidak diimpor panel).
func TestContentSelectAlreadyRunning(t *testing.T) {
	cases := map[string]error{
		"syncjob": syncjob.ErrAlreadyRunning,
		"ingest":  errors.New("ingest: ingest lain masih berjalan (run 5)"),
	}

	for name, runErr := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.login()

			f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
				return seriesPage(page, 1, 1, 1), nil
			}
			if rec := f.get("/content/short"); rec.Code != http.StatusOK {
				t.Fatalf("GET /content/short: status = %d, mau 200", rec.Code)
			}

			f.ingest.err = runErr
			rec := f.post("/content/select", url.Values{
				"provider": {"dramahits"},
				"slug":     {"series-1-1"},
			})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, mau 409 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Masih ada sinkronisasi") {
				t.Fatalf("pesan konflik tidak tampil (body=%s)", rec.Body.String())
			}
		})
	}
}

// TestContentSelectRequiresCSRF memastikan POST tanpa field _csrf ditolak 403.
func TestContentSelectRequiresCSRF(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(_ string, page int) (catalog.SeriesPage, error) {
		return seriesPage(page, 1, 1, 1), nil
	}
	if rec := f.get("/content/short"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content/short: status = %d, mau 200", rec.Code)
	}

	rec := f.postRaw("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-1"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, mau 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":2007`) {
		t.Fatalf("body = %s, mau envelope code 2007", rec.Body.String())
	}
	if got := f.ingest.calls(); len(got) != 0 {
		t.Fatalf("Ingest.Start dipanggil walau CSRF gagal: %v", got)
	}
}

// TestContentRequiresLogin memastikan route /content tetap di balik RequireAdmin.
func TestContentRequiresLogin(t *testing.T) {
	f := newFixture(t)

	rec := f.get("/content/short?page=1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET tanpa login: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location = %q, mau /login", loc)
	}

	rec = f.do(http.MethodGet, "/content/short?page=1", nil, "application/json")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET JSON tanpa login: status = %d, mau 401 (body=%s)", rec.Code, rec.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body bukan envelope: %v", err)
	}
	if env.Code != response.CodeSessionExpired {
		t.Fatalf("code = %d, mau %d", env.Code, response.CodeSessionExpired)
	}

	// POST: middleware mengalihkan ke /login sebelum handler/CSRF berjalan.
	f.get("/login")
	rec = f.do(http.MethodPost, "/content/select", url.Values{
		"_csrf":    {f.csrf()},
		"provider": {"dramahits"},
		"slug":     {"series-1-1"},
	}, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST tanpa login: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("POST tanpa login: Location = %q, mau /login", loc)
	}
}

// TestContentRoutesNotMountedWithoutDeps memastikan route /content tidak
// dipasang bila Lister atau Ingest nil.
func TestContentRoutesNotMountedWithoutDeps(t *testing.T) {
	f := newFixture(t, withoutContent())
	f.login()

	rec := f.get("/content/short?page=1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /content/short: status = %d, mau 404 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = f.post("/content/select", url.Values{
		"provider": {"dramahits"},
		"slug":     {"series-1-1"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /content/select: status = %d, mau 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSyncPageShowsListContent memastikan halaman /sync memakai tombol dan
// modal "List Content" serta tidak lagi menawarkan resync penuh.
func TestSyncPageShowsListContent(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.get("/sync")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sync: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"List Content",
		"Movie",
		"Series",
		"Short",
		"content-dialog",
		"Simpan pilihan (0)",
		"Batal",
		"Remove all batch",
		"Riwayat 20 run terakhir",
		// Tiga tab kategori, satu panel bersama, tanpa data-provider tunggal.
		`data-category="movie"`,
		`data-category="series"`,
		`data-category="short"`,
		`id="content-panel"`,
		// JavaScript inline tidak boleh hilang/berubah saat dirender.
		"selected = new Map()",
		"currentCategory = 'movie'",
		// Kunci pilihan memakai provider+kategori+slug, bukan slug saja.
		`'\x00'`,
		"/content?category=",
		"form.action = '/content/select'",
		"'data-key'",
		"selections.name = 'selections'",
		// Konten yang sudah ada tidak boleh dicentang lagi.
		"Sudah ada",
		"already_added",
		"checkbox.disabled = added",
		"selected.delete(key)",
		// Film Densgo tidak punya jumlah episode, jadi kartunya berlabel Film.
		"return 'Film';",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman sync tidak memuat %q", want)
		}
	}
	for _, unwanted := range []string{
		"Densgo belum tersedia",
		`data-provider=`,
		"/content/short?page=",
		`name = 'provider'`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("halaman sync masih memuat %q", unwanted)
		}
	}
	if strings.Contains(strings.ToLower(body), "resync penuh") {
		t.Fatalf("halaman sync masih memuat teks resync penuh")
	}
	if strings.Contains(body, `method="post" action="/sync"`) {
		t.Fatalf("halaman sync masih memuat form POST /sync")
	}

	// Riwayat run tetap ada.
	if !strings.Contains(body, "#1") {
		t.Fatalf("riwayat run hilang dari halaman sync")
	}
}

// categoryPage membuat halaman palsu untuk satu kategori dengan satu item.
func categoryPage(category string, page int) catalog.SeriesPage {
	return catalog.SeriesPage{
		Items: []catalog.Series{{
			Provider:     providerForTestCategory(category),
			Category:     category,
			ID:           "test-content",
			Slug:         "test-content",
			Title:        "Konten " + category,
			EpisodeCount: 0,
		}},
		Page:     page,
		PerPage:  1,
		Total:    1,
		LastPage: 1,
	}
}

// providerForTestCategory meniru pemetaan provider per kategori: Densgo untuk
// movie/series, Dramahits untuk short.
func providerForTestCategory(category string) string {
	if category == catalog.CategoryShort {
		return "dramahits"
	}
	return "densgo"
}

// decodeContentPage membaca envelope GET /content menjadi contentEnvelope.
func decodeContentPage(t *testing.T, body []byte) contentEnvelope {
	t.Helper()

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body bukan envelope: %v (body=%s)", err, body)
	}
	if env.Code != response.CodeSuccess {
		t.Fatalf("code = %d, mau 0 (body=%s)", env.Code, body)
	}

	var payload contentEnvelope
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	return payload
}

// TestContentListByCategory memastikan GET /content meneruskan kategori ke
// lister dan mengembalikan item dengan provider/kategori yang sesuai, termasuk
// kategori bawaan movie saat parameter tidak dikirim.
func TestContentListByCategory(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return categoryPage(category, page), nil
	}

	cases := []struct {
		target       string
		wantCategory string
		wantProvider string
		wantPage     int
	}{
		{"/content?category=movie&page=1", catalog.CategoryMovie, "densgo", 1},
		{"/content?category=series&page=3", catalog.CategorySeries, "densgo", 3},
		{"/content?category=short", catalog.CategoryShort, "dramahits", 1},
		{"/content", catalog.CategoryMovie, "densgo", 1},
	}
	for _, tc := range cases {
		rec := f.do(http.MethodGet, tc.target, nil, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, mau 200 (body=%s)", tc.target, rec.Code, rec.Body.String())
		}
		payload := decodeContentPage(t, rec.Body.Bytes())
		if len(payload.Items) != 1 {
			t.Fatalf("GET %s: jumlah item = %d, mau 1", tc.target, len(payload.Items))
		}
		item := payload.Items[0]
		if item.Provider != tc.wantProvider || item.Category != tc.wantCategory {
			t.Fatalf("GET %s: provider/kategori = %q/%q, mau %q/%q",
				tc.target, item.Provider, item.Category, tc.wantProvider, tc.wantCategory)
		}
		if payload.Meta.Page != tc.wantPage {
			t.Fatalf("GET %s: meta.page = %d, mau %d", tc.target, payload.Meta.Page, tc.wantPage)
		}
	}

	if got := strings.Join(f.lister.categoriesSeen(), ","); got != "movie,series,short,movie" {
		t.Fatalf("kategori yang diminta = %q, mau movie,series,short,movie", got)
	}
}

// TestContentShortEndpointIsCompatWrapper memastikan GET /content/short tetap
// bekerja sebagai pembungkus kategori short.
func TestContentShortEndpointIsCompatWrapper(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return categoryPage(category, page), nil
	}

	rec := f.do(http.MethodGet, "/content/short?page=2", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	payload := decodeContentPage(t, rec.Body.Bytes())
	if len(payload.Items) != 1 || payload.Items[0].Category != catalog.CategoryShort {
		t.Fatalf("items = %+v, mau satu item kategori short", payload.Items)
	}
	if payload.Meta.Page != 2 {
		t.Fatalf("meta.page = %d, mau 2", payload.Meta.Page)
	}
	if got := strings.Join(f.lister.categoriesSeen(), ","); got != "short" {
		t.Fatalf("kategori yang diminta = %q, mau short", got)
	}
	if pages := f.lister.pagesSeen(); len(pages) != 1 || pages[0] != 2 {
		t.Fatalf("halaman diminta = %v, mau [2]", pages)
	}
}

// TestContentInvalidCategory memastikan kategori di luar movie/series/short
// ditolak sebagai envelope validasi (422) tanpa memanggil lister.
func TestContentInvalidCategory(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodGet, "/content?category=vod", nil, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body bukan envelope: %v", err)
	}
	if env.Code != response.CodeValidationError {
		t.Fatalf("code = %d, mau %d", env.Code, response.CodeValidationError)
	}
	if got := f.lister.categoriesSeen(); len(got) != 0 {
		t.Fatalf("lister dipanggil dengan %v, mau tidak dipanggil", got)
	}
}

// TestContentFillsMissingProviderCategory memastikan item tanpa provider atau
// kategori dilengkapi dari kategori sumber: densgo/movie, densgo/series, dan
// dramahits/short.
func TestContentFillsMissingProviderCategory(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return catalog.SeriesPage{
			Items:    []catalog.Series{{ID: "test-content", Slug: "test-content", Title: "Tanpa identitas"}},
			Page:     page,
			PerPage:  1,
			Total:    1,
			LastPage: 1,
		}, nil
	}

	cases := map[string][2]string{
		catalog.CategoryMovie:  {"densgo", catalog.CategoryMovie},
		catalog.CategorySeries: {"densgo", catalog.CategorySeries},
		catalog.CategoryShort:  {"dramahits", catalog.CategoryShort},
	}
	for category, want := range cases {
		rec := f.do(http.MethodGet, "/content?category="+category, nil, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("kategori %s: status = %d, mau 200 (body=%s)", category, rec.Code, rec.Body.String())
		}
		payload := decodeContentPage(t, rec.Body.Bytes())
		if len(payload.Items) != 1 {
			t.Fatalf("kategori %s: jumlah item = %d, mau 1", category, len(payload.Items))
		}
		item := payload.Items[0]
		if item.Provider != want[0] || item.Category != want[1] {
			t.Fatalf("kategori %s: provider/kategori = %q/%q, mau %q/%q",
				category, item.Provider, item.Category, want[0], want[1])
		}
	}
}

// TestContentCacheKeyIsolation memastikan cache membedakan slug yang sama pada
// kategori berbeda (Densgo movie dan series bisa berbagi ID) sehingga dua
// pilihan dengan slug identik tetap diresolusi ke konten yang benar.
func TestContentCacheKeyIsolation(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		item := categoryPage(category, page)
		item.Items[0].Title = "Densgo " + category
		return item, nil
	}

	for _, category := range []string{catalog.CategoryMovie, catalog.CategorySeries} {
		rec := f.do(http.MethodGet, "/content?category="+category, nil, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("kategori %s: status = %d, mau 200 (body=%s)", category, rec.Code, rec.Body.String())
		}
	}

	selections, err := json.Marshal([]map[string]string{
		{"provider": "densgo", "category": catalog.CategoryMovie, "slug": "test-content"},
		{"provider": "densgo", "category": catalog.CategorySeries, "slug": "test-content"},
	})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}

	rec := f.post("/content/select", url.Values{"selections": {string(selections)}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("select: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	calls := f.ingest.calls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("Ingest.Start dipanggil %d kali dengan %v, mau sekali berisi 2 konten", len(calls), calls)
	}
	if calls[0][0].Category != catalog.CategoryMovie || calls[0][0].Title != "Densgo movie" {
		t.Fatalf("pilihan pertama = %+v, mau konten movie", calls[0][0])
	}
	if calls[0][1].Category != catalog.CategorySeries || calls[0][1].Title != "Densgo series" {
		t.Fatalf("pilihan kedua = %+v, mau konten series", calls[0][1])
	}
}

// TestContentSelectMixedProvidersJSON memastikan field "selections" boleh
// memuat lebih dari satu provider: panel tidak menolak campuran, keduanya
// diteruskan ke Ingest.Start.
//
// ingest.Service.Start kini memvalidasi tiap pilihan (provider, kategori, dan
// stream_url untuk movie) alih-alih menuntut satu provider; panel sengaja tidak
// menyalin aturan itu agar tetap satu sumber kebenaran. Stub ingest di sini
// hanya mencatat apa yang diterimanya.
func TestContentSelectMixedProvidersJSON(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		if category == catalog.CategoryShort {
			return seriesPage(page, 1, 1, 1), nil
		}
		item := categoryPage(category, page)
		item.Items[0].Title = "Film Densgo"
		return item, nil
	}

	for _, category := range []string{catalog.CategoryMovie, catalog.CategoryShort} {
		rec := f.do(http.MethodGet, "/content?category="+category, nil, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("kategori %s: status = %d, mau 200 (body=%s)", category, rec.Code, rec.Body.String())
		}
	}

	selections := `[` +
		`{"provider":"densgo","category":"movie","slug":"test-content"},` +
		`{"provider":"dramahits","category":"short","slug":"series-1-1"}` +
		`]`
	rec := f.post("/content/select", url.Values{"selections": {selections}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	calls := f.ingest.calls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("Ingest.Start dipanggil %d kali dengan %v, mau sekali berisi 2 konten", len(calls), calls)
	}
	if calls[0][0].Provider != "densgo" || calls[0][0].Category != catalog.CategoryMovie {
		t.Fatalf("pilihan pertama = %+v, mau densgo/movie", calls[0][0])
	}
	if calls[0][1].Provider != "dramahits" || calls[0][1].Category != catalog.CategoryShort {
		t.Fatalf("pilihan kedua = %+v, mau dramahits/short", calls[0][1])
	}
	if calls[0][1].Title != "Series 1-1" {
		t.Fatalf("judul pilihan kedua = %q, mau Series 1-1", calls[0][1].Title)
	}
}

// TestContentSelectRejectsUnknownSelection memastikan pilihan yang tidak ada di
// cache ditolak 422 dan pesannya menyebut identitas yang bermasalah.
func TestContentSelectRejectsUnknownSelection(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return categoryPage(category, page), nil
	}
	if rec := f.get("/content?category=movie"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content: status = %d, mau 200", rec.Code)
	}

	selections := `[{"provider":"densgo","category":"movie","slug":"tidak-ada-di-cache"}]`
	rec := f.post("/content/select", url.Values{"selections": {selections}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tidak-ada-di-cache") {
		t.Fatalf("pesan tidak menyebut pilihan yang bermasalah (body=%s)", rec.Body.String())
	}
	if got := f.ingest.calls(); len(got) != 0 {
		t.Fatalf("Ingest.Start dipanggil %v, mau tidak dipanggil", got)
	}
}

// TestContentSelectRejectsMalformedJSON memastikan field selections yang bukan
// JSON array ditolak 422 tanpa memanggil worker.
func TestContentSelectRejectsMalformedJSON(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return categoryPage(category, page), nil
	}
	if rec := f.get("/content?category=movie"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content: status = %d, mau 200", rec.Code)
	}

	rec := f.post("/content/select", url.Values{"selections": {"{bukan json"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := f.ingest.calls(); len(got) != 0 {
		t.Fatalf("Ingest.Start dipanggil %v, mau tidak dipanggil", got)
	}
}

// TestContentSelectLegacyDensgoDefaultsToMovie memastikan formulir lama
// (provider + slug tanpa kategori) menganggap pilihan Densgo sebagai movie.
func TestContentSelectLegacyDensgoDefaultsToMovie(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		return categoryPage(category, page), nil
	}
	if rec := f.get("/content?category=movie"); rec.Code != http.StatusOK {
		t.Fatalf("GET /content: status = %d, mau 200", rec.Code)
	}

	rec := f.post("/content/select", url.Values{
		"provider": {"densgo"},
		"slug":     {"test-content"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	calls := f.ingest.calls()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("Ingest.Start dipanggil %d kali dengan %v, mau sekali berisi 1 konten", len(calls), calls)
	}
	if calls[0][0].Provider != "densgo" || calls[0][0].Category != catalog.CategoryMovie {
		t.Fatalf("pilihan = %+v, mau densgo/movie", calls[0][0])
	}
}

// TestContentMovieStreamURLNotExposedButKept memastikan stream_url tidak ikut
// pada JSON daftar (modal tidak membutuhkannya), tetapi tetap tersimpan di
// cache sehingga pilihan movie yang dikirim ke ingest masih membawa URL
// streaming — ingest menolak movie tanpa stream_url.
func TestContentMovieStreamURLNotExposedButKept(t *testing.T) {
	f := newFixture(t)
	f.login()

	f.lister.fn = func(category string, page int) (catalog.SeriesPage, error) {
		item := categoryPage(category, page)
		item.Items[0].StreamURL = "https://cdn.example/test-content/index.m3u8"
		return item, nil
	}

	rec := f.do(http.MethodGet, "/content?category=movie", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stream_url") {
		t.Fatalf("payload daftar memuat stream_url: %s", rec.Body.String())
	}

	selections := `[{"provider":"densgo","category":"movie","slug":"test-content"}]`
	rec = f.post("/content/select", url.Values{"selections": {selections}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("select: status = %d, mau 303 (body=%s)", rec.Code, rec.Body.String())
	}

	calls := f.ingest.calls()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("Ingest.Start dipanggil %d kali dengan %v, mau sekali berisi 1 konten", len(calls), calls)
	}
	if got := calls[0][0].StreamURL; got != "https://cdn.example/test-content/index.m3u8" {
		t.Fatalf("stream_url pilihan = %q, mau URL dari cache", got)
	}
}
