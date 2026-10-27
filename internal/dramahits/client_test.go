package dramahits

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vodlocal/internal/catalog"
)

// readTestdata mengembalikan isi file di testdata.
func readTestdata(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("membaca testdata/%s: %v", name, err)
	}
	return string(body)
}

// newTestClient menjalankan server uji dengan handler dan mengembalikan Client
// yang menunjuk ke server itu. Server ditutup otomatis saat test selesai.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 5*time.Second)
}

// serveTestdata menjawab setiap permintaan dengan isi testdata name dan
// mencatat permintaan yang masuk.
func serveTestdata(t *testing.T, name string, capture *requestCapture) *Client {
	t.Helper()

	body := readTestdata(t, name)
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture.record(r)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Logf("menulis body: %v", err)
		}
	})
}

// requestCapture menyimpan detail permintaan terakhir yang diterima server uji.
// Aksesnya dikunci supaya aman dipakai lintas goroutine.
type requestCapture struct {
	mu          sync.Mutex
	path        string
	escapedPath string
	rawQuery    string
	requestURI  string
}

// record menyalin detail permintaan r.
func (c *requestCapture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.path = r.URL.Path
	c.escapedPath = r.URL.EscapedPath()
	c.rawQuery = r.URL.RawQuery
	c.requestURI = r.RequestURI
}

// snapshot mengembalikan detail permintaan yang tercatat terakhir.
func (c *requestCapture) snapshot() (path, escapedPath, rawQuery, requestURI string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.path, c.escapedPath, c.rawQuery, c.requestURI
}

// Test 1: ListSeries halaman 1 memetakan field sesuai kontrak catalog.
func TestListSeriesMapsFields(t *testing.T) {
	capture := &requestCapture{}
	c := serveTestdata(t, "series.json", capture)

	if got := c.Code(); got != "dramahits" {
		t.Errorf("Code() = %q, mau %q", got, "dramahits")
	}

	got, err := c.ListSeries(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}

	want := catalog.SeriesPage{
		Page:     1,
		PerPage:  30,
		Total:    95,
		LastPage: 4,
		Items: []catalog.Series{
			{
				Provider:     "dramahits",
				Category:     catalog.CategoryShort,
				ID:           "9f8c2a54-1d4e-4c51-9a9b-2a3c7d8e1f01",
				Slug:         "rahasia-nyonya-muda",
				Title:        "Rahasia Nyonya Muda",
				Description:  "Pernikahan kontrak yang berubah jadi cinta.",
				PosterURL:    "https://cdn.digdaya.co.id/dramahits/rahasia-nyonya-muda/poster-vertical.jpg",
				Year:         2025,
				EpisodeCount: 80,
				AgeRating:    "Dewasa 17+",
				PublishedAt:  "2026-10-20T10:46:00+07:00",
			},
			{
				// release_year berupa angka dan age_rating_label kosong:
				// Year harus tetap terbaca, AgeRating jatuh ke age_rating.
				Provider:     "dramahits",
				Category:     catalog.CategoryShort,
				ID:           "3b7e5d12-8c4a-4f60-b1d2-6e9f0a2b3c4d",
				Slug:         "cinta-anak-kos",
				Title:        "Cinta Anak Kos",
				Description:  "Dua penghuni kos yang bermusuhan lalu jatuh cinta.",
				PosterURL:    "https://cdn.digdaya.co.id/dramahits/cinta-anak-kos/poster-vertical.jpg",
				Year:         2024,
				EpisodeCount: 24,
				AgeRating:    "13+",
				PublishedAt:  "2026-09-01T08:00:00+07:00",
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSeries(ctx, 1) =\n%+v\nmau\n%+v", got, want)
	}

	path, _, _, _ := capture.snapshot()
	if path != "/series" {
		t.Errorf("path = %q, mau %q", path, "/series")
	}
}

// Test 2: ListSeries memanggil path dan query pagination yang benar.
func TestListSeriesRequestsExpectedPathAndQuery(t *testing.T) {
	tests := []struct {
		name       string
		page       int
		wantNumber string
	}{
		{name: "halaman 2", page: 2, wantNumber: "2"},
		{name: "halaman 0 jadi 1", page: 0, wantNumber: "1"},
		{name: "halaman negatif jadi 1", page: -3, wantNumber: "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &requestCapture{}
			c := serveTestdata(t, "series.json", capture)

			if _, err := c.ListSeries(context.Background(), tt.page); err != nil {
				t.Fatalf("ListSeries(ctx, %d): %v", tt.page, err)
			}

			path, _, rawQuery, requestURI := capture.snapshot()

			if path != "/series" {
				t.Errorf("path = %q, mau %q", path, "/series")
			}

			// url.Values wajib meng-encode nama kunci: page[number] menjadi
			// page%5Bnumber%5D.
			wantQuery := "page%5Bnumber%5D=" + tt.wantNumber
			if rawQuery != wantQuery {
				t.Errorf("RawQuery = %q, mau %q", rawQuery, wantQuery)
			}
			if !strings.Contains(requestURI, "?"+wantQuery) {
				t.Errorf("RequestURI = %q, mau memuat %q", requestURI, "?"+wantQuery)
			}
		})
	}
}

// Test 3: ListEpisodes memetakan season, number, durasi, stream, dan preview.
func TestListEpisodesMapsFields(t *testing.T) {
	capture := &requestCapture{}
	c := serveTestdata(t, "episodes.json", capture)

	got, err := c.ListEpisodes(context.Background(), "rahasia-nyonya-muda", 1)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}

	want := catalog.EpisodePage{
		Page:     1,
		PerPage:  30,
		Total:    2,
		LastPage: 1,
		Items: []catalog.Episode{
			{
				ID:              "a1b2c3d4-0001-4e5f-8a9b-0c1d2e3f4a5b",
				Slug:            "rahasia-nyonya-muda-episode-1",
				Title:           "Episode 1",
				Season:          1,
				Number:          1,
				DurationSeconds: 138,
				PublishedAt:     "2026-10-20T10:50:00+07:00",
				Streams: []catalog.Stream{
					{
						Device: "ios",
						URL:    "https://cdn.digdaya.co.id/dramahits/rahasia-nyonya-muda/ep1/ios.m3u8",
					},
					{
						Device: "android",
						URL:    "https://cdn.digdaya.co.id/dramahits/rahasia-nyonya-muda/ep1/android.m3u8",
					},
				},
				PreviewURL: "https://cdn.digdaya.co.id/dramahits/rahasia-nyonya-muda/ep1/preview.mp4",
			},
			{
				// Episode tanpa blok preview: PreviewURL harus kosong, dan
				// expires_at berupa string ikut terpetakan.
				ID:              "a1b2c3d4-0002-4e5f-8a9b-0c1d2e3f4a5b",
				Slug:            "rahasia-nyonya-muda-episode-2",
				Title:           "Episode 2",
				Season:          1,
				Number:          2,
				DurationSeconds: 95,
				PublishedAt:     "2026-10-20T10:55:00+07:00",
				Streams: []catalog.Stream{
					{
						Device:        "all",
						URL:           "https://cdn.digdaya.co.id/dramahits/rahasia-nyonya-muda/ep2/all.m3u8",
						TokenRequired: true,
						ExpiresAt:     "2026-10-21T00:00:00+07:00",
					},
				},
				PreviewURL: "",
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListEpisodes(ctx, \"rahasia-nyonya-muda\", 1) =\n%+v\nmau\n%+v", got, want)
	}

	path, _, _, _ := capture.snapshot()
	wantPath := "/series/rahasia-nyonya-muda/episodes"
	if path != wantPath {
		t.Errorf("path = %q, mau %q", path, wantPath)
	}
}

// Test 4: seriesSlug dengan karakter khusus di-escape sebagai satu segmen path.
func TestListEpisodesEscapesSlug(t *testing.T) {
	const slug = "series khusus/2025?promo=1"

	capture := &requestCapture{}
	c := serveTestdata(t, "episodes.json", capture)

	if _, err := c.ListEpisodes(context.Background(), slug, 1); err != nil {
		t.Fatalf("ListEpisodes(ctx, %q, 1): %v", slug, err)
	}

	path, escapedPath, rawQuery, requestURI := capture.snapshot()

	wantEscaped := "/series/" + url.PathEscape(slug) + "/episodes"
	if escapedPath != wantEscaped {
		t.Errorf("EscapedPath = %q, mau %q", escapedPath, wantEscaped)
	}
	if !strings.Contains(escapedPath, "%2F") {
		t.Errorf("EscapedPath = %q, mau memuat %%2F (garis miring di slug ikut di-escape)", escapedPath)
	}
	if wantPath := "/series/" + slug + "/episodes"; path != wantPath {
		t.Errorf("path = %q, mau %q", path, wantPath)
	}
	if want := "page%5Bnumber%5D=1"; rawQuery != want {
		t.Errorf("RawQuery = %q, mau %q", rawQuery, want)
	}
	if !strings.Contains(requestURI, "%2F") {
		t.Errorf("RequestURI = %q, mau memuat %%2F", requestURI)
	}
}

// Test 5: status non-2xx menjadi error yang memuat kode status dan path.
func TestStatusError(t *testing.T) {
	tests := []struct {
		name     string
		wantPath string
		call     func(*Client) error
	}{
		{
			name:     "series",
			wantPath: "/series",
			call: func(c *Client) error {
				_, err := c.ListSeries(context.Background(), 1)
				return err
			},
		},
		{
			name:     "episodes",
			wantPath: "/series/rahasia-nyonya-muda/episodes",
			call: func(c *Client) error {
				_, err := c.ListEpisodes(context.Background(), "rahasia-nyonya-muda", 1)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"Terjadi kesalahan pada server"}`, http.StatusInternalServerError)
			})

			err := tt.call(c)
			if err == nil {
				t.Fatal("mau error, dapat nil")
			}
			if !strings.Contains(err.Error(), "500") {
				t.Errorf("error = %q, mau memuat kode status 500", err)
			}
			if !strings.Contains(err.Error(), tt.wantPath) {
				t.Errorf("error = %q, mau memuat path %q", err, tt.wantPath)
			}
		})
	}
}

// Test 6: body JSON rusak menjadi error.
func TestBrokenJSONError(t *testing.T) {
	const broken = `{"data": [{"id": "x"`

	tests := []struct {
		name string
		call func(*Client) error
	}{
		{
			name: "series",
			call: func(c *Client) error {
				_, err := c.ListSeries(context.Background(), 1)
				return err
			},
		},
		{
			name: "episodes",
			call: func(c *Client) error {
				_, err := c.ListEpisodes(context.Background(), "rahasia-nyonya-muda", 1)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(broken)); err != nil {
					t.Logf("menulis body: %v", err)
				}
			})

			if err := tt.call(c); err == nil {
				t.Fatal("mau error decode JSON, dapat nil")
			}
		})
	}
}

// Test 7: pembatalan context diteruskan sebagai error.
func TestContextCancel(t *testing.T) {
	t.Run("dibatalkan sebelum request", func(t *testing.T) {
		var called atomic.Bool
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := c.ListSeries(ctx, 1)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, mau errors.Is(err, context.Canceled)", err)
		}
		if called.Load() {
			t.Error("server tidak boleh menerima permintaan setelah context dibatalkan")
		}
	})

	t.Run("dibatalkan saat request berjalan", func(t *testing.T) {
		started := make(chan struct{})
		var once sync.Once

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(started) })
			// Tahan respons sampai klien membatalkan permintaan.
			<-r.Context().Done()
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			_, err := c.ListEpisodes(ctx, "rahasia-nyonya-muda", 1)
			errCh <- err
		}()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("server tidak menerima permintaan")
		}

		cancel()

		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, mau errors.Is(err, context.Canceled)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ListEpisodes tidak berhenti setelah context dibatalkan")
		}
	})
}

// Test 8: body melebihi MaxResponseBytes ditolak, bukan dibuffer.
func TestResponseBodyTooLarge(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		oversized := bytes.Repeat([]byte("a"), int(MaxResponseBytes)+1)
		if _, err := w.Write(oversized); err != nil {
			t.Logf("menulis body: %v", err)
		}
	})

	_, err := c.ListSeries(context.Background(), 1)
	if err == nil {
		t.Fatal("mau error body kebesaran, dapat nil")
	}
	if !strings.Contains(err.Error(), "melebihi") {
		t.Errorf("error = %q, mau memuat %q", err, "melebihi")
	}
}
