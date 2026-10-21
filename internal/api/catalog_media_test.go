package api_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"vodlocal/internal/response"
)

// mediaTitlesData adalah bentuk data GET /titles yang menyertakan poster dan
// backdrop.
type mediaTitlesData struct {
	Items []struct {
		ID          int64   `json:"id"`
		Title       string  `json:"title"`
		PosterURL   *string `json:"poster_url"`
		BackdropURL *string `json:"backdrop_url"`
	} `json:"items"`
	Meta listMeta `json:"meta"`
}

// mediaTitleSeed adalah satu judul tambahan untuk test URL poster/backdrop.
// PosterPath/BackdropPath bernilai nil berarti kolomnya NULL; string dipakai
// apa adanya (termasuk string kosong dan spasi).
type mediaTitleSeed struct {
	ID           int64
	PlayableID   int64
	Kind         string
	Title        string
	Slug         string
	PosterPath   any
	BackdropPath any
	Stills       []any // satu still_path per episode; nil berarti still NULL
}

// seedMediaTitle menyisipkan judul siap tayang pada generation aktif: playable,
// satu rendisi ready, dan (bila Stills tidak kosong) episode dengan still_path.
func (f *fixture) seedMediaTitle(seed mediaTitleSeed) {
	f.t.Helper()

	f.exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug, synopsis,
	            release_year, content_rating, language, poster_path, backdrop_path,
	            duration_seconds, is_published, created_at, updated_at)
	        VALUES (?, 1, ?, ?, ?, ?, NULL, 2025, '13+', 'id', ?, ?, 5400, 1,
	            '2026-10-20T09:00:00Z', '2026-10-20T13:00:00Z')`,
		seed.ID, "ext-"+seed.Slug, seed.Kind, seed.Title, seed.Slug,
		seed.PosterPath, seed.BackdropPath)

	f.exec(`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
	        VALUES (?, ?, NULL, 5400)`, seed.PlayableID, seed.ID)
	f.exec(`INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps,
	            width, height, playlist_path, status)
	        VALUES (?, 1, '480p', 900, 854, 480, ?, 'ready')`,
		seed.PlayableID, seed.Slug+"/480p/index.m3u8")

	for i, still := range seed.Stills {
		episodeID := seed.ID*100 + int64(i) + 1
		f.exec(`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number,
		            name, synopsis, duration_seconds, air_date, still_path, created_at, updated_at)
		        VALUES (?, ?, ?, 1, ?, ?, NULL, 1200, '2025-02-01', ?,
		            '2026-10-20T09:00:00Z', '2026-10-20T09:00:00Z')`,
			episodeID, seed.ID, "ep-"+strconv.FormatInt(episodeID, 10), i+1,
			"Episode "+strconv.Itoa(i+1), still)
	}
}

// mediaItemURL adalah pasangan poster/backdrop yang dibaca dari response.
type mediaItemURL struct {
	Poster   *string
	Backdrop *string
}

// TestTitleMediaURLs memastikan poster_url, backdrop_url, dan still_url menunjuk
// ke /media/<generation>/<path>: artwork kini tersimpan di dalam folder
// generation (satu folder dengan m3u8/ts), bukan lagi di /artwork.
func TestTitleMediaURLs(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 15", "AA:BB:CC:DD:EE:15")

	// Judul series dengan poster/backdrop/still di dalam folder generation.
	f.seedMediaTitle(mediaTitleSeed{
		ID:           10,
		PlayableID:   9100,
		Kind:         "series",
		Title:        "Poster Dalam Generation",
		Slug:         "poster-dalam-generation",
		PosterPath:   "dramahits/series/9d1c-4f5a/poster.jpg",
		BackdropPath: "dramahits/series/9d1c-4f5a/backdrop.jpg",
		Stills:       []any{"dramahits/series/9d1c-4f5a/still-01.jpg", nil},
	})
	// Poster/backdrop NULL dan string kosong → null, bukan URL /artwork.
	f.seedMediaTitle(mediaTitleSeed{
		ID:         11,
		PlayableID: 9101,
		Kind:       "movie",
		Title:      "Tanpa Poster",
		Slug:       "tanpa-poster",
		PosterPath: nil,
	})
	f.seedMediaTitle(mediaTitleSeed{
		ID:           12,
		PlayableID:   9102,
		Kind:         "movie",
		Title:        "Poster Spasi",
		Slug:         "poster-spasi",
		PosterPath:   "   ",
		BackdropPath: "",
	})
	// Path dengan ".." tidak boleh lolos keluar dari root generation.
	f.seedMediaTitle(mediaTitleSeed{
		ID:           13,
		PlayableID:   9103,
		Kind:         "movie",
		Title:        "Poster Menanjak",
		Slug:         "poster-menanjak",
		PosterPath:   "../dramahits/../poster.jpg",
		BackdropPath: "/dramahits/series/x/backdrop.jpg",
	})

	const (
		mediaPrefix  = "/media/gen-000001/"
		wantPoster   = "/media/gen-000001/dramahits/series/9d1c-4f5a/poster.jpg"
		wantBackdrop = "/media/gen-000001/dramahits/series/9d1c-4f5a/backdrop.jpg"
		wantStill    = "/media/gen-000001/dramahits/series/9d1c-4f5a/still-01.jpg"
	)

	// checkURL memastikan satu URL: null bila kosong, selain itu selalu berada di
	// dalam generation aktif dan tidak memuat "..".
	checkURL := func(label string, raw *string) {
		t.Helper()
		if raw == nil {
			return
		}
		if *raw == "" || strings.Contains(*raw, "..") || !strings.HasPrefix(*raw, mediaPrefix) {
			t.Fatalf("%s = %q, mau null atau berawalan %q tanpa \"..\"", label, *raw, mediaPrefix)
		}
	}

	// 1. GET /titles: poster dan backdrop memakai label generation aktif.
	env, rec := f.do(http.MethodGet, "/titles?per_page=200", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /titles: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/artwork/") {
		t.Fatalf("GET /titles masih memuat URL /artwork: %s", body)
	}

	var data mediaTitlesData
	f.decode(env, &data)

	got := make(map[int64]mediaItemURL, len(data.Items))
	for _, item := range data.Items {
		got[item.ID] = mediaItemURL{Poster: item.PosterURL, Backdrop: item.BackdropURL}
		checkURL("poster_url", item.PosterURL)
		checkURL("backdrop_url", item.BackdropURL)
	}

	if item, found := got[10]; !found {
		t.Fatalf("judul 10 tidak muncul di GET /titles: %+v", data.Items)
	} else {
		if item.Poster == nil || *item.Poster != wantPoster {
			t.Fatalf("poster_url judul 10 = %v, mau %s", item.Poster, wantPoster)
		}
		if item.Backdrop == nil || *item.Backdrop != wantBackdrop {
			t.Fatalf("backdrop_url judul 10 = %v, mau %s", item.Backdrop, wantBackdrop)
		}
	}
	// Judul bawaan fixture (poster dens_tv/1/poster.jpg) ikut aturan baru.
	if item, found := got[1]; !found {
		t.Fatalf("judul 1 tidak muncul di GET /titles: %+v", data.Items)
	} else if item.Poster == nil || *item.Poster != "/media/gen-000001/dens_tv/1/poster.jpg" {
		t.Fatalf("poster_url judul 1 = %v, mau /media/gen-000001/dens_tv/1/poster.jpg", item.Poster)
	}

	// 2 & 3. Poster/backdrop kosong menjadi null.
	for _, id := range []int64{11, 12} {
		item, found := got[id]
		if !found {
			t.Fatalf("judul %d tidak muncul di GET /titles: %+v", id, data.Items)
		}
		if item.Poster != nil {
			t.Fatalf("poster_url judul %d = %v, mau null", id, *item.Poster)
		}
		if item.Backdrop != nil {
			t.Fatalf("backdrop_url judul %d = %v, mau null", id, *item.Backdrop)
		}
	}

	// Path dengan ".." dinormalisasi ke root generation, bukan dibiarkan.
	if item, found := got[13]; !found {
		t.Fatalf("judul 13 tidak muncul di GET /titles: %+v", data.Items)
	} else {
		if item.Poster == nil || *item.Poster != "/media/gen-000001/poster.jpg" {
			t.Fatalf("poster_url judul 13 = %v, mau /media/gen-000001/poster.jpg", item.Poster)
		}
		if item.Backdrop == nil || *item.Backdrop != "/media/gen-000001/dramahits/series/x/backdrop.jpg" {
			t.Fatalf("backdrop_url judul 13 = %v, mau /media/gen-000001/dramahits/series/x/backdrop.jpg", item.Backdrop)
		}
	}

	// Detail judul memakai aturan yang sama.
	env, rec = f.do(http.MethodGet, "/titles/10", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /titles/10: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/artwork/") {
		t.Fatalf("GET /titles/10 masih memuat URL /artwork: %s", body)
	}
	var detail struct {
		ID          int64   `json:"id"`
		PosterURL   *string `json:"poster_url"`
		BackdropURL *string `json:"backdrop_url"`
	}
	f.decode(env, &detail)
	if detail.PosterURL == nil || *detail.PosterURL != wantPoster {
		t.Fatalf("detail poster_url = %v, mau %s", detail.PosterURL, wantPoster)
	}
	if detail.BackdropURL == nil || *detail.BackdropURL != wantBackdrop {
		t.Fatalf("detail backdrop_url = %v, mau %s", detail.BackdropURL, wantBackdrop)
	}

	// Episode: still di dalam generation, dan still kosong tetap null.
	env, rec = f.do(http.MethodGet, "/titles/10/episodes", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /titles/10/episodes: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/artwork/") {
		t.Fatalf("GET /titles/10/episodes masih memuat URL /artwork: %s", body)
	}
	var episodes struct {
		Items []struct {
			ID       int64   `json:"id"`
			StillURL *string `json:"still_url"`
		} `json:"items"`
	}
	f.decode(env, &episodes)
	if len(episodes.Items) != 2 {
		t.Fatalf("episodes = %d, mau 2", len(episodes.Items))
	}
	if still := episodes.Items[0].StillURL; still == nil || *still != wantStill {
		t.Fatalf("still_url episode pertama = %v, mau %s", still, wantStill)
	}
	if still := episodes.Items[1].StillURL; still != nil {
		t.Fatalf("still_url episode tanpa still = %v, mau null (tanpa fallback /artwork)", *still)
	}

	// Hasil search: poster judul dan still episode memakai aturan yang sama.
	env, rec = f.do(http.MethodGet, "/search?q=poster", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /search?q=poster: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/artwork/") {
		t.Fatalf("GET /search masih memuat URL /artwork: %s", body)
	}
	var searchResult struct {
		Titles []struct {
			ID        int64   `json:"id"`
			PosterURL *string `json:"poster_url"`
		} `json:"titles"`
	}
	f.decode(env, &searchResult)
	var searchPoster *string
	for _, item := range searchResult.Titles {
		checkURL("search poster_url", item.PosterURL)
		if item.ID == 10 {
			searchPoster = item.PosterURL
		}
	}
	if searchPoster == nil || *searchPoster != wantPoster {
		t.Fatalf("search poster_url judul 10 = %v, mau %s", searchPoster, wantPoster)
	}

	env, rec = f.do(http.MethodGet, "/search?q=episode", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /search?q=episode: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	var searchEpisodes struct {
		Episodes []struct {
			ID       int64   `json:"id"`
			StillURL *string `json:"still_url"`
		} `json:"episodes"`
	}
	f.decode(env, &searchEpisodes)
	if len(searchEpisodes.Episodes) != 2 {
		t.Fatalf("search episodes = %d, mau 2", len(searchEpisodes.Episodes))
	}
	// Urutan hasil search mengikuti updated_at/id, jadi dicocokkan per ID.
	searchStill := make(map[int64]*string, len(searchEpisodes.Episodes))
	for _, item := range searchEpisodes.Episodes {
		searchStill[item.ID] = item.StillURL
	}
	if still := searchStill[1001]; still == nil || *still != wantStill {
		t.Fatalf("search still_url episode 1001 = %v, mau %s", still, wantStill)
	}
	if still := searchStill[1002]; still != nil {
		t.Fatalf("search still_url episode 1002 = %v, mau null", *still)
	}

	// Generation aktif tanpa label: tidak ada prefix yang bisa dipakai, jadi
	// seluruh URL artwork menjadi null — bukan fallback ke /artwork.
	f.exec(`UPDATE generations SET label = '' WHERE id = 1`)
	env, rec = f.do(http.MethodGet, "/titles", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /titles tanpa label: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/artwork/") {
		t.Fatalf("GET /titles tanpa label memakai fallback /artwork: %s", body)
	}
	var noLabel mediaTitlesData
	f.decode(env, &noLabel)
	if len(noLabel.Items) == 0 {
		t.Fatalf("GET /titles tanpa label kosong, mau item tetap dilayani")
	}
	for _, item := range noLabel.Items {
		if item.PosterURL != nil || item.BackdropURL != nil {
			t.Fatalf("poster_url/backdrop_url judul %d = %v/%v, mau null tanpa label generation",
				item.ID, item.PosterURL, item.BackdropURL)
		}
	}
}
