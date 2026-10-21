package densgo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"vodlocal/internal/catalog"
	"vodlocal/internal/provider"
)

// basePath adalah path yang dipakai di sebagian besar tes untuk memastikan
// BaseURL yang sudah berisi path tidak dipotong.
const basePath = "/index.php/v3070/adps_list_movies_v1"

// moviesBody meniru respons GET /index?type=movie.
const moviesBody = `{
  "current_page": "1",
  "per_page": "10",
  "total_data": 343,
  "total_pages": 35,
  "data": [
    {
      "id": "10160",
      "movie_code": "MVNSL00005",
      "title": "Hujan di Bulan Juni",
      "description": "Sinopsis film pertama.",
      "poster_url": "https://picture.dens.tv/10160.jpg",
      "stream_url": "https://op-svod.dens.tv/04/MVNSL00005/index5.m3u8",
      "duration": "1"
    },
    {
      "id": "10161",
      "movie_code": "MVMCE10006",
      "title": "Film Kedua",
      "description": "Sinopsis film kedua.",
      "poster_url": "https://picture.dens.tv/10161.jpg",
      "stream_url": "https://op-svod.dens.tv/04/MVMCE10006/index5.m3u8",
      "duration": "2"
    }
  ]
}`

// seriesBody meniru respons GET /index?type=series: stream_url dan duration
// bernilai null.
const seriesBody = `{
  "current_page": "1",
  "per_page": "10",
  "total_data": 17,
  "total_pages": 2,
  "data": [
    {
      "id": "10144",
      "movie_code": "MVMCE10006",
      "title": "Stockholm Requiem",
      "description": "Serial tanpa URL playback di daftar.",
      "poster_url": "https://picture.dens.tv/10144.jpg",
      "stream_url": null,
      "duration": null
    }
  ]
}`

// episodesBody meniru respons GET /episodes?seriesId=10144.
const episodesBody = `{
  "data": [
    {
      "id": "10145",
      "movie_code": "MVMCE00121",
      "title": "Stockholm Requiem Eps.1",
      "description": "Episode pertama.",
      "type": "episode",
      "poster_url": "https://picture.dens.tv/10144.jpg",
      "stream_url": "https://op-svod.dens.tv/04/MVMCE00110/index5.m3u8",
      "duration": "89"
    },
    {
      "id": "10146",
      "movie_code": "MVMCE00122",
      "title": "Stockholm Requiem Eps.2",
      "description": "Episode kedua.",
      "type": "episode",
      "poster_url": "https://picture.dens.tv/10144.jpg",
      "stream_url": "https://op-svod.dens.tv/04/MVMCE00111/index5.m3u8",
      "duration": "87"
    }
  ]
}`

// recorded adalah satu permintaan yang diterima server tes.
type recorded struct {
	path      string
	query     map[string]string
	userAgent string
}

// newServer menjalankan server tes yang membalas body/status untuk path apa pun
// dan mencatat permintaan terakhir di last.
func newServer(t *testing.T, status int, body string) (*httptest.Server, *recorded) {
	t.Helper()

	last := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = recorded{
			path:      r.URL.Path,
			query:     singleValues(r.URL.Query()),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Logf("menulis body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, last
}

// singleValues meratakan url.Values supaya assertion lebih ringkas.
func singleValues(values map[string][]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, list := range values {
		if len(list) == 0 {
			out[key] = ""
			continue
		}
		out[key] = list[0]
	}
	return out
}

// The movie list request must carry q/type/page/limit and the mapped items must
// expose StreamURL.
func TestListSeriesMovieRequestAndMapping(t *testing.T) {
	srv, last := newServer(t, http.StatusOK, moviesBody)
	c := NewClient(srv.URL+basePath+"/", 2*time.Second, catalog.CategoryMovie)

	page, err := c.ListSeries(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListSeries() error = %v", err)
	}

	if want := basePath + "/index"; last.path != want {
		t.Errorf("path = %q, want %q", last.path, want)
	}
	wantQuery := map[string]string{"q": "", "type": "movie", "page": "1", "limit": "10"}
	if !reflect.DeepEqual(last.query, wantQuery) {
		t.Errorf("query = %v, want %v", last.query, wantQuery)
	}
	if last.userAgent != userAgent {
		t.Errorf("User-Agent = %q, want %q", last.userAgent, userAgent)
	}

	wantItems := []catalog.Series{
		{
			Provider:    ProviderCode,
			Category:    catalog.CategoryMovie,
			ID:          "10160",
			Slug:        "10160",
			Title:       "Hujan di Bulan Juni",
			Description: "Sinopsis film pertama.",
			PosterURL:   "https://picture.dens.tv/10160.jpg",
			StreamURL:   "https://op-svod.dens.tv/04/MVNSL00005/index5.m3u8",
		},
		{
			Provider:    ProviderCode,
			Category:    catalog.CategoryMovie,
			ID:          "10161",
			Slug:        "10161",
			Title:       "Film Kedua",
			Description: "Sinopsis film kedua.",
			PosterURL:   "https://picture.dens.tv/10161.jpg",
			StreamURL:   "https://op-svod.dens.tv/04/MVMCE10006/index5.m3u8",
		},
	}
	if !reflect.DeepEqual(page.Items, wantItems) {
		t.Errorf("Items = %+v, want %+v", page.Items, wantItems)
	}
}

// A series item has stream_url:null, so StreamURL must stay empty.
func TestListSeriesSeriesStreamURLEmpty(t *testing.T) {
	srv, last := newServer(t, http.StatusOK, seriesBody)
	c := NewClient(srv.URL+basePath, 2*time.Second, catalog.CategorySeries)

	page, err := c.ListSeries(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListSeries() error = %v", err)
	}

	wantQuery := map[string]string{"q": "", "type": "series", "page": "1", "limit": "10"}
	if !reflect.DeepEqual(last.query, wantQuery) {
		t.Errorf("query = %v, want %v", last.query, wantQuery)
	}
	if len(page.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(page.Items))
	}

	got := page.Items[0]
	if got.StreamURL != "" {
		t.Errorf("StreamURL = %q, want empty for a series", got.StreamURL)
	}
	if got.Category != catalog.CategorySeries || got.Provider != ProviderCode {
		t.Errorf("item = %+v, want provider %q category %q", got, ProviderCode, catalog.CategorySeries)
	}
	if got.ID != "10144" || got.Slug != "10144" || got.EpisodeCount != 0 {
		t.Errorf("item = %+v, want ID/Slug 10144 and EpisodeCount 0", got)
	}
	if got.Title != "Stockholm Requiem" || got.PosterURL != "https://picture.dens.tv/10144.jpg" {
		t.Errorf("item = %+v, want title and poster mapped", got)
	}
}

// Pagination must be read from strings (current_page/per_page) and numbers
// (total_data/total_pages) at the same time, with sane fallbacks.
func TestListSeriesPaginationParsing(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		page     int
		wantPage int
		wantPer  int
		wantTot  int
		wantLast int
	}{
		{
			name:     "string dan angka",
			body:     `{"current_page":"2","per_page":"25","total_data":343,"total_pages":35,"data":[]}`,
			page:     2,
			wantPage: 2,
			wantPer:  25,
			wantTot:  343,
			wantLast: 35,
		},
		{
			name:     "semua angka",
			body:     `{"current_page":3,"per_page":10,"total_data":12,"total_pages":4,"data":[]}`,
			page:     3,
			wantPage: 3,
			wantPer:  10,
			wantTot:  12,
			wantLast: 4,
		},
		{
			name:     "current_page nol pakai halaman yang diminta",
			body:     `{"current_page":"0","per_page":"10","total_data":100,"total_pages":10,"data":[]}`,
			page:     4,
			wantPage: 4,
			wantPer:  10,
			wantTot:  100,
			wantLast: 10,
		},
		{
			name:     "meta hilang",
			body:     `{"data":[]}`,
			page:     3,
			wantPage: 3,
			wantPer:  0,
			wantTot:  0,
			wantLast: 3,
		},
		{
			name:     "total_pages lebih kecil dari halaman yang diminta",
			body:     `{"current_page":"5","per_page":"10","total_data":20,"total_pages":2,"data":[]}`,
			page:     5,
			wantPage: 5,
			wantPer:  10,
			wantTot:  20,
			wantLast: 5,
		},
		{
			name:     "angka tidak terbaca jadi nol",
			body:     `{"current_page":"?","per_page":"","total_data":null,"total_pages":"","data":[]}`,
			page:     1,
			wantPage: 1,
			wantPer:  0,
			wantTot:  0,
			wantLast: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t, http.StatusOK, tc.body)
			c := NewClient(srv.URL, 2*time.Second, catalog.CategorySeries)

			page, err := c.ListSeries(context.Background(), tc.page)
			if err != nil {
				t.Fatalf("ListSeries() error = %v", err)
			}
			if page.Page != tc.wantPage || page.PerPage != tc.wantPer || page.Total != tc.wantTot || page.LastPage != tc.wantLast {
				t.Errorf("page = {Page:%d PerPage:%d Total:%d LastPage:%d}, want {%d %d %d %d}",
					page.Page, page.PerPage, page.Total, page.LastPage,
					tc.wantPage, tc.wantPer, tc.wantTot, tc.wantLast)
			}
			if page.Items == nil {
				t.Error("Items = nil, want an empty non-nil slice")
			}
		})
	}
}

// A requested page below 1 must be sent as page=1, and a real page number must
// reach the API untouched.
func TestListSeriesRequestedPage(t *testing.T) {
	srv, last := newServer(t, http.StatusOK, moviesBody)

	c := NewClient(srv.URL, 2*time.Second, catalog.CategoryMovie)
	if _, err := c.ListSeries(context.Background(), 0); err != nil {
		t.Fatalf("ListSeries(0) error = %v", err)
	}
	if got := last.query["page"]; got != "1" {
		t.Errorf("page = %q, want %q", got, "1")
	}

	if _, err := c.ListSeries(context.Background(), 7); err != nil {
		t.Fatalf("ListSeries(7) error = %v", err)
	}
	if got := last.query["page"]; got != "7" {
		t.Errorf("page = %q, want %q", got, "7")
	}
	if got := last.query["limit"]; got != "10" {
		t.Errorf("limit = %q, want %q", got, "10")
	}
}

// The episode call must only send seriesId, and every item must be mapped to a
// numbered episode with its stream URL and no guessed duration.
func TestListEpisodesMapping(t *testing.T) {
	srv, last := newServer(t, http.StatusOK, episodesBody)
	c := NewClient(srv.URL+basePath, 2*time.Second, catalog.CategorySeries)

	page, err := c.ListEpisodes(context.Background(), "10144", 3)
	if err != nil {
		t.Fatalf("ListEpisodes() error = %v", err)
	}

	if want := basePath + "/episodes"; last.path != want {
		t.Errorf("path = %q, want %q", last.path, want)
	}
	wantQuery := map[string]string{"seriesId": "10144"}
	if !reflect.DeepEqual(last.query, wantQuery) {
		t.Errorf("query = %v, want %v (page is ignored by the API)", last.query, wantQuery)
	}

	wantItems := []catalog.Episode{
		{
			ID:              "10145",
			Slug:            "10145",
			Title:           "Stockholm Requiem Eps.1",
			Season:          1,
			Number:          1,
			DurationSeconds: 0,
			Streams: []catalog.Stream{
				{Device: "android", URL: "https://op-svod.dens.tv/04/MVMCE00110/index5.m3u8"},
			},
			PreviewURL: "",
		},
		{
			ID:              "10146",
			Slug:            "10146",
			Title:           "Stockholm Requiem Eps.2",
			Season:          1,
			Number:          2,
			DurationSeconds: 0,
			Streams: []catalog.Stream{
				{Device: "android", URL: "https://op-svod.dens.tv/04/MVMCE00111/index5.m3u8"},
			},
			PreviewURL: "",
		},
	}
	if !reflect.DeepEqual(page.Items, wantItems) {
		t.Errorf("Items = %+v, want %+v", page.Items, wantItems)
	}

	if page.Page != 1 || page.LastPage != 1 || page.PerPage != 2 || page.Total != 2 {
		t.Errorf("page = {Page:%d PerPage:%d Total:%d LastPage:%d}, want single page of 2",
			page.Page, page.PerPage, page.Total, page.LastPage)
	}
}

// An episode item without stream_url still maps, but with no stream.
func TestListEpisodesWithoutStreamURL(t *testing.T) {
	body := `{"data":[{"id":"900","movie_code":"MVMCE00900","title":"Eps.1","description":"","type":"episode","poster_url":"","stream_url":null,"duration":null}]}`
	srv, _ := newServer(t, http.StatusOK, body)
	c := NewClient(srv.URL, 2*time.Second, catalog.CategorySeries)

	page, err := c.ListEpisodes(context.Background(), "10144", 1)
	if err != nil {
		t.Fatalf("ListEpisodes() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(page.Items))
	}
	if got := page.Items[0].Streams; len(got) != 0 {
		t.Errorf("Streams = %+v, want empty", got)
	}
}

// A movie client has no episodes to fetch: the result is an empty page and the
// API is never contacted.
func TestListEpisodesMovieSkipsAPI(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, 2*time.Second, catalog.CategoryMovie)
	page, err := c.ListEpisodes(context.Background(), "10160", 1)
	if err != nil {
		t.Fatalf("ListEpisodes() error = %v", err)
	}
	if called != 0 {
		t.Errorf("server dipanggil %d kali, want 0", called)
	}
	if len(page.Items) != 0 {
		t.Errorf("Items = %+v, want empty", page.Items)
	}
	if page.Items == nil {
		t.Error("Items = nil, want an empty non-nil slice")
	}
	if page.Page != 1 || page.LastPage != 1 || page.Total != 0 {
		t.Errorf("page = %+v, want an empty page 1", page)
	}
}

// Upstream failures must surface as errors, both for the list and the episode
// endpoints.
func TestErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"list status 500", http.StatusInternalServerError, `{"error":"gagal"}`, provider.ErrUpstream},
		{"list status 404", http.StatusNotFound, `{"error":"tidak ada"}`, provider.ErrNotFound},
		{"list json rusak", http.StatusOK, `{"current_page":"1","data":[`, provider.ErrBadResponse},
		{"list tipe salah", http.StatusOK, `{"data":"bukan array"}`, provider.ErrBadResponse},
		{"episodes json rusak", http.StatusOK, `<html>oops</html>`, provider.ErrBadResponse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t, tc.status, tc.body)

			movie := NewClient(srv.URL, 2*time.Second, catalog.CategoryMovie)
			if _, err := movie.ListSeries(context.Background(), 1); err == nil {
				t.Fatal("ListSeries() error = nil, want an error")
			} else if !errors.Is(err, tc.want) {
				t.Errorf("ListSeries() error = %v, want %v", err, tc.want)
			}

			series := NewClient(srv.URL, 2*time.Second, catalog.CategorySeries)
			if _, err := series.ListEpisodes(context.Background(), "10144", 1); err == nil {
				t.Fatal("ListEpisodes() error = nil, want an error")
			} else if !errors.Is(err, tc.want) {
				t.Errorf("ListEpisodes() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A client built for an unsupported category must refuse both calls with a
// clear error, without touching the network.
func TestUnknownCategory(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	for _, category := range []string{"", "short", "drama"} {
		c := NewClient(srv.URL, 2*time.Second, category)

		_, err := c.ListSeries(context.Background(), 1)
		if err == nil {
			t.Fatalf("ListSeries(category %q) error = nil, want an error", category)
		}
		if !strings.Contains(err.Error(), ErrUnknownCategory.Error()) {
			t.Errorf("ListSeries(category %q) error = %v, want it to wrap %v", category, err, ErrUnknownCategory)
		}

		_, err = c.ListEpisodes(context.Background(), "10144", 1)
		if err == nil {
			t.Fatalf("ListEpisodes(category %q) error = nil, want an error", category)
		}
		if !strings.Contains(err.Error(), ErrUnknownCategory.Error()) {
			t.Errorf("ListEpisodes(category %q) error = %v, want it to wrap %v", category, err, ErrUnknownCategory)
		}
	}

	if called != 0 {
		t.Errorf("server dipanggil %d kali, want 0", called)
	}
}

// Code, Category and the constructor defaults must be stable.
func TestClientIdentity(t *testing.T) {
	c := NewClient("https://example.test/base/", 0, " "+strings.ToUpper(catalog.CategorySeries)+" ")
	if got := c.Code(); got != "densgo" {
		t.Errorf("Code() = %q, want %q", got, "densgo")
	}
	if got := c.Category(); got != catalog.CategorySeries {
		t.Errorf("Category() = %q, want %q", got, catalog.CategorySeries)
	}
	if got := c.BaseURL; got != "https://example.test/base" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", got)
	}
	if c.HTTP == nil {
		t.Fatal("HTTP = nil, want a client with a timeout")
	}
	if c.HTTP.Timeout != DefaultTimeout {
		t.Errorf("HTTP.Timeout = %v, want %v for a non-positive timeout", c.HTTP.Timeout, DefaultTimeout)
	}
	if got := c.Header.Get("User-Agent"); got != userAgent {
		t.Errorf("User-Agent = %q, want %q", got, userAgent)
	}

	movie := NewClient("https://example.test", 5*time.Second, catalog.CategoryMovie)
	if got := movie.Category(); got != catalog.CategoryMovie {
		t.Errorf("Category() = %q, want %q", got, catalog.CategoryMovie)
	}
	if movie.HTTP.Timeout != 5*time.Second {
		t.Errorf("HTTP.Timeout = %v, want 5s", movie.HTTP.Timeout)
	}
}
