package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"vodlocal/internal/catalog"
)

// ---------------------------------------------------------------- helpers ---

// episodePagesN membangun satu halaman berisi n episode untuk series-uji.
func episodePagesN(streamBase string, n int) []catalog.EpisodePage {
	items := make([]catalog.Episode, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, catalog.Episode{
			ID:              fmt.Sprintf("ep%d", i),
			Slug:            fmt.Sprintf("ep-%d", i),
			Title:           fmt.Sprintf("Episode %d", i),
			Season:          1,
			Number:          i,
			DurationSeconds: 12,
			Streams:         []catalog.Stream{{Device: "android", URL: streamBase + "/master.m3u8"}},
		})
	}
	return []catalog.EpisodePage{{Items: items, Page: 1, PerPage: n, Total: n, LastPage: 1}}
}

// twoEpisodePages membangun daftar episode berisi dua episode series-uji.
func twoEpisodePages(streamBase string) map[string][]catalog.EpisodePage {
	return map[string][]catalog.EpisodePage{"series-uji": episodePagesN(streamBase, 2)}
}

// seriesDua adalah selection kedua untuk skenario multi-series.
func seriesDua() catalog.Series {
	return catalog.Series{
		Provider: "dramahits",
		Category: catalog.CategoryShort,
		ID:       "sr2",
		Slug:     "series-dua",
		Title:    "Series Dua",
	}
}

// generationItem adalah satu baris generation_items yang dibaca test.
type generationItem struct {
	status           string
	category         string
	seriesTitle      string
	posterURL        sql.NullString
	currentStep      sql.NullString
	currentEpisode   sql.NullString
	totalEpisodes    int64
	episodesDone     int64
	episodesFailed   int64
	renditionsReady  int64
	renditionsFailed int64
	bytesDownloaded  int64
	progressPercent  int64
	startedAt        sql.NullString
	finishedAt       sql.NullString
	errorMessage     sql.NullString
}

// readGenerationItem membaca baris generation_items satu series milik runID.
func readGenerationItem(t *testing.T, env *testEnv, runID int64, externalID string) generationItem {
	t.Helper()

	genID := queryInt(t, env.store.Read(),
		`SELECT generation_id FROM sync_runs WHERE id = ?`, runID)

	var it generationItem
	err := env.store.Read().QueryRow(`
SELECT status, category, series_title, poster_url, current_step, current_episode,
       total_episodes, episodes_done, episodes_failed, renditions_ready,
       renditions_failed, bytes_downloaded, progress_percent, started_at,
       finished_at, error_message
  FROM generation_items
 WHERE generation_id = ? AND series_external_id = ?`, genID, externalID).
		Scan(&it.status, &it.category, &it.seriesTitle, &it.posterURL, &it.currentStep,
			&it.currentEpisode, &it.totalEpisodes, &it.episodesDone, &it.episodesFailed,
			&it.renditionsReady, &it.renditionsFailed, &it.bytesDownloaded,
			&it.progressPercent, &it.startedAt, &it.finishedAt, &it.errorMessage)
	if err != nil {
		t.Fatalf("baca generation_items %q: %v", externalID, err)
	}
	return it
}

// itemCount menghitung baris generation_items milik runID.
func itemCount(t *testing.T, env *testEnv, runID int64) int64 {
	t.Helper()

	return queryInt(t, env.store.Read(), `
SELECT COUNT(*) FROM generation_items
 WHERE generation_id = (SELECT generation_id FROM sync_runs WHERE id = ?)`, runID)
}

// --------------------------------------------------------------- skenario ---

// TestGenerationItemHappyPath memastikan satu series dua episode menghasilkan
// tepat satu baris generation_items yang selesai dengan progres 100%.
func TestGenerationItemHappyPath(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: twoEpisodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	posterURL := srv.URL + "/art/poster.jpg"
	runID, err := env.svc.Start(context.Background(), selection(posterURL))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got := itemCount(t, env, runID); got != 1 {
		t.Fatalf("jumlah generation_items = %d, mau 1", got)
	}

	it := readGenerationItem(t, env, runID, "series-uji")
	if it.status != "done" {
		t.Errorf("status = %q, mau done", it.status)
	}
	if it.totalEpisodes != 2 {
		t.Errorf("total_episodes = %d, mau 2", it.totalEpisodes)
	}
	if it.episodesDone != 2 {
		t.Errorf("episodes_done = %d, mau 2", it.episodesDone)
	}
	if it.episodesFailed != 0 {
		t.Errorf("episodes_failed = %d, mau 0", it.episodesFailed)
	}
	if it.progressPercent != 100 {
		t.Errorf("progress_percent = %d, mau 100", it.progressPercent)
	}
	if it.renditionsReady != 2 {
		t.Errorf("renditions_ready = %d, mau 2", it.renditionsReady)
	}
	if it.renditionsFailed != 0 {
		t.Errorf("renditions_failed = %d, mau 0", it.renditionsFailed)
	}
	if it.bytesDownloaded <= 0 {
		t.Errorf("bytes_downloaded = %d, mau > 0", it.bytesDownloaded)
	}
	if got := it.currentStep.String; got != "done" {
		t.Errorf("current_step = %q, mau done", got)
	}
	if got := it.currentEpisode.String; got != "S01E02" {
		t.Errorf("current_episode = %q, mau S01E02", got)
	}
	if !it.startedAt.Valid || it.startedAt.String == "" {
		t.Error("started_at kosong, mau terisi")
	}
	if !it.finishedAt.Valid || it.finishedAt.String == "" {
		t.Error("finished_at kosong, mau terisi")
	}
	if it.errorMessage.Valid {
		t.Errorf("error_message = %q, mau NULL", it.errorMessage.String)
	}
	if it.category != catalog.CategoryShort {
		t.Errorf("category = %q, mau %q", it.category, catalog.CategoryShort)
	}
	if it.seriesTitle != "Series Uji" {
		t.Errorf("series_title = %q, mau Series Uji", it.seriesTitle)
	}
	if got := it.posterURL.String; got != posterURL {
		t.Errorf("poster_url = %q, mau %q", got, posterURL)
	}
}

// TestGenerationItemsForTwoSeries memastikan dua selection menghasilkan dua
// baris generation_items yang keduanya selesai.
func TestGenerationItemsForTwoSeries(t *testing.T) {
	srv := newHLSServer(t)
	pages := twoEpisodePages(srv.URL)
	pages["series-dua"] = episodePagesN(srv.URL, 2)

	src := &fakeSource{code: "dramahits", pages: pages}
	env := newEnv(t, src, srv.Client())

	sels := append(selection(""), seriesDua())

	runID, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got := itemCount(t, env, runID); got != 2 {
		t.Fatalf("jumlah generation_items = %d, mau 2", got)
	}

	for _, external := range []string{"series-uji", "series-dua"} {
		it := readGenerationItem(t, env, runID, external)
		if it.status != "done" {
			t.Errorf("%s: status = %q, mau done", external, it.status)
		}
		if it.totalEpisodes != 2 {
			t.Errorf("%s: total_episodes = %d, mau 2", external, it.totalEpisodes)
		}
		if it.episodesDone != 2 {
			t.Errorf("%s: episodes_done = %d, mau 2", external, it.episodesDone)
		}
		if it.progressPercent != 100 {
			t.Errorf("%s: progress_percent = %d, mau 100", external, it.progressPercent)
		}
		if it.finishedAt.String == "" {
			t.Errorf("%s: finished_at kosong, mau terisi", external)
		}
	}
}

// TestGenerationItemFailsWhenAllStreamsFail memastikan series yang seluruh
// episodenya gagal ditandai 'failed' dengan error_message terisi.
func TestGenerationItemFailsWhenAllStreamsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	src := &fakeSource{code: "dramahits", pages: twoEpisodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "failed" {
		t.Fatalf("sync_runs.status = %q, mau failed", got)
	}

	it := readGenerationItem(t, env, runID, "series-uji")
	if it.status != "failed" {
		t.Errorf("status = %q, mau failed", it.status)
	}
	if !it.errorMessage.Valid || it.errorMessage.String == "" {
		t.Error("error_message kosong, mau terisi")
	}
	if !it.finishedAt.Valid || it.finishedAt.String == "" {
		t.Error("finished_at kosong, mau terisi")
	}
	if it.episodesDone != 0 {
		t.Errorf("episodes_done = %d, mau 0", it.episodesDone)
	}
	if it.episodesFailed != 2 {
		t.Errorf("episodes_failed = %d, mau 2", it.episodesFailed)
	}
	// Progres mengikuti rumus (done+failed)*100/total, jadi seluruh episode
	// yang gagal tetap menghasilkan 100%; status 'failed' yang menandai
	// kegagalannya.
	if it.progressPercent != 100 {
		t.Errorf("progress_percent = %d, mau 100", it.progressPercent)
	}
	if it.renditionsFailed != 2 {
		t.Errorf("renditions_failed = %d, mau 2", it.renditionsFailed)
	}
}

// TestGenerationItemOneSeriesFailsOthersContinue memastikan kegagalan satu
// series tidak menghentikan series lain: barisnya 'failed' dengan error_message,
// sementara series yang sehat tetap 'done' dan run tetap completed.
func TestGenerationItemOneSeriesFailsOthersContinue(t *testing.T) {
	srv := newHLSServer(t)

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(badSrv.Close)

	pages := twoEpisodePages(badSrv.URL)
	pages["series-dua"] = episodePagesN(srv.URL, 2)

	src := &fakeSource{code: "dramahits", pages: pages}
	env := newEnv(t, src, srv.Client())

	sels := append(selection(""), seriesDua())

	runID, err := env.svc.Start(context.Background(), sels)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	failed := readGenerationItem(t, env, runID, "series-uji")
	if failed.status != "failed" {
		t.Errorf("series-uji: status = %q, mau failed", failed.status)
	}
	if !failed.errorMessage.Valid || failed.errorMessage.String == "" {
		t.Error("series-uji: error_message kosong, mau terisi")
	}
	if failed.episodesFailed != 2 {
		t.Errorf("series-uji: episodes_failed = %d, mau 2", failed.episodesFailed)
	}

	ok := readGenerationItem(t, env, runID, "series-dua")
	if ok.status != "done" {
		t.Errorf("series-dua: status = %q, mau done", ok.status)
	}
	if ok.episodesDone != 2 {
		t.Errorf("series-dua: episodes_done = %d, mau 2", ok.episodesDone)
	}
	if ok.progressPercent != 100 {
		t.Errorf("series-dua: progress_percent = %d, mau 100", ok.progressPercent)
	}
}

// TestGenerationItemMaxEpisodes memastikan Config.MaxEpisodes membatasi jumlah
// episode yang diproses dan bahwa total_episodes melaporkan jumlah yang benar-
// benar diproses (bukan seluruh katalog), sehingga progres mencapai 100%.
func TestGenerationItemMaxEpisodes(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: map[string][]catalog.EpisodePage{
		"series-uji": episodePagesN(srv.URL, 3),
	}}
	env := newEnv(t, src, srv.Client(), func(cfg *Config) { cfg.MaxEpisodes = 1 })

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got := queryInt(t, env.store.Read(), `SELECT COUNT(*) FROM episodes`); got != 1 {
		t.Errorf("episode diproses = %d, mau 1", got)
	}

	it := readGenerationItem(t, env, runID, "series-uji")
	if it.status != "done" {
		t.Errorf("status = %q, mau done", it.status)
	}
	if it.totalEpisodes != 1 {
		t.Errorf("total_episodes = %d, mau 1 (jumlah yang diproses)", it.totalEpisodes)
	}
	if it.episodesDone != 1 {
		t.Errorf("episodes_done = %d, mau 1", it.episodesDone)
	}
	if it.progressPercent != 100 {
		t.Errorf("progress_percent = %d, mau 100", it.progressPercent)
	}
	if got := it.currentEpisode.String; got != "S01E01" {
		t.Errorf("current_episode = %q, mau S01E01", got)
	}
}

// TestGenerationItemWritesNeverFailRun memastikan kegagalan menulis progres
// (di sini tabel generation_items dihapus) tidak pernah menggagalkan run yang
// sebenarnya sukses.
func TestGenerationItemWritesNeverFailRun(t *testing.T) {
	srv := newHLSServer(t)
	src := &fakeSource{code: "dramahits", pages: episodePages(srv.URL)}
	env := newEnv(t, src, srv.Client())

	// Tanpa tabel generation_items semua insert/update progres gagal, tetapi
	// hanya boleh menghasilkan log Warn.
	if _, err := env.store.Write().Exec(`DROP TABLE generation_items`); err != nil {
		t.Fatalf("drop generation_items: %v", err)
	}

	runID, err := env.svc.Start(context.Background(), selection(""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitRun(t, env.store, runID); got != "completed" {
		t.Fatalf("sync_runs.status = %q, mau completed", got)
	}

	if got := queryInt(t, env.store.Read(),
		`SELECT renditions_ready FROM sync_runs WHERE id = ?`, runID); got != 1 {
		t.Errorf("renditions_ready = %d, mau 1", got)
	}
	if got := queryString(t, env.store.Read(), `SELECT status FROM generations`); got != "active" {
		t.Errorf("generations.status = %q, mau active", got)
	}
}

// TestProgressPercent memastikan persentase dijepit ke 0..100 dan tidak pernah
// membagi dengan nol.
func TestProgressPercent(t *testing.T) {
	cases := []struct {
		processed int
		total     int
		want      int
	}{
		{processed: 0, total: 0, want: 0},
		{processed: 0, total: 2, want: 0},
		{processed: 1, total: 2, want: 50},
		{processed: 2, total: 2, want: 100},
		{processed: 3, total: 2, want: 100},
		{processed: -1, total: 2, want: 0},
	}

	for _, tc := range cases {
		if got := progressPercent(tc.processed, tc.total); got != tc.want {
			t.Errorf("progressPercent(%d, %d) = %d, mau %d", tc.processed, tc.total, got, tc.want)
		}
	}
}
