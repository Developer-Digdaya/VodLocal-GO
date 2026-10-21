package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStorageContentPaginationAndFilters(t *testing.T) {
	f := newFixture(t)
	f.login()

	for i := 1; i <= 60; i++ {
		category := "short"
		if i%2 == 0 {
			category = "movie"
		}
		status := "done"
		if i%3 == 0 {
			status = "running"
		}
		f.exec(`INSERT INTO generation_items
		          (id, generation_id, provider_id, category, series_external_id, series_title,
		           status, total_episodes, episodes_done, progress_percent)
		        VALUES (?, 42, 1, ?, ?, ?, ?, 1, 1, 100)`,
			5000+i, category, fmt.Sprintf("bulk-%02d", i), fmt.Sprintf("Bulk %02d", i), status)
	}

	rec := f.get("/storage?per_page=10&page=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Bulk 11", "Bulk 20", "Halaman 2 / 6", "dari 60 konten"} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman storage tidak memuat %q", want)
		}
	}
	if strings.Contains(body, "Bulk 01") {
		t.Fatalf("halaman 2 tidak boleh memuat item halaman 1")
	}

	// Filter pencarian.
	filtered := f.get("/storage?q=Bulk%2005&per_page=10")
	if filtered.Code != http.StatusOK {
		t.Fatalf("filter q: status = %d", filtered.Code)
	}
	if !strings.Contains(filtered.Body.String(), "Bulk 05") {
		t.Fatalf("filter q tidak menampilkan Bulk 05")
	}
	if strings.Contains(filtered.Body.String(), "Bulk 06") {
		t.Fatalf("filter q masih menampilkan Bulk 06")
	}

	// Filter kategori.
	movieOnly := f.get("/storage?category=movie&per_page=100")
	if !strings.Contains(movieOnly.Body.String(), "Bulk 02") {
		t.Fatalf("filter movie tidak menampilkan item movie")
	}
	if strings.Contains(movieOnly.Body.String(), "Bulk 01") {
		t.Fatalf("filter movie masih menampilkan item short")
	}
}

func TestStorageGenerationPagination(t *testing.T) {
	f := newFixture(t)
	f.login()

	for i := 0; i < 12; i++ {
		f.exec(`INSERT INTO generations (id, label, root_path, status, file_count, total_bytes, created_at)
		        VALUES (?, ?, ?, 'retired', 0, 0, ?)`,
			1000+i, fmt.Sprintf("gen-%06d", 1000+i), fmt.Sprintf("/srv/media/generations/gen-%06d", 1000+i),
			fmt.Sprintf("2026-09-%02dT00:00:00Z", i+1))
	}

	rec := f.get("/storage?gen_per_page=5&gen_page=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Halaman 3 / 3", "gen-001004", "dari 15 generation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("pagination generation tidak memuat %q", want)
		}
	}
}

func TestSyncHistoryPagination(t *testing.T) {
	f := newFixture(t)
	f.login()

	for i := 0; i < 24; i++ {
		f.exec(`INSERT INTO sync_runs (id, generation_id, trigger_source, scope, status, created_at)
		        VALUES (?, 42, 'manual', 'selection', 'completed', ?)`,
			1000+i, fmt.Sprintf("2026-10-%02dT00:00:00Z", i+1))
	}

	rec := f.get("/sync?per_page=10&page=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Halaman 2 / 3", "#1013", "25 run"} {
		if !strings.Contains(body, want) {
			t.Fatalf("riwayat sync tidak memuat %q", want)
		}
	}
}

func TestSyncDetailPagination(t *testing.T) {
	f := newFixture(t)
	f.login()
	seedTwoGenerationItems(f)

	for i := 3; i <= 60; i++ {
		f.seedGenerationItem(generationItemSeed{
			GenerationID:  42,
			Title:         fmt.Sprintf("Bulk %02d", i),
			ExternalID:    fmt.Sprintf("bulk-%02d", i),
			Category:      "short",
			Status:        "done",
			TotalEpisodes: 1,
			EpisodesDone:  1,
			Percent:       100,
		})
	}

	rec := f.get("/sync/1?per_page=10&page=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-page="2"`, `data-per-page="10"`,
		"Halaman 2 / 6", "menampilkan 11–20 dari 60 series",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail sync tidak memuat %q", want)
		}
	}

	partial := f.do(http.MethodGet, "/sync/1/items?per_page=10&page=2", nil, "text/html")
	if partial.Code != http.StatusOK {
		t.Fatalf("partial status = %d", partial.Code)
	}
	for _, want := range []string{`data-page="2"`, "Halaman 2 / 6"} {
		if !strings.Contains(partial.Body.String(), want) {
			t.Fatalf("fragmen detail tidak memuat %q", want)
		}
	}

	jsonRec := f.do(http.MethodGet, "/sync/1/items?per_page=10&page=2", nil, "application/json")
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("json status = %d", jsonRec.Code)
	}
	for _, want := range []string{`"page":2`, `"per_page":10`, `"total":60`, `"last_page":6`} {
		if !strings.Contains(jsonRec.Body.String(), want) {
			t.Fatalf("meta JSON tidak memuat %q (body=%s)", want, jsonRec.Body.String())
		}
	}
}
