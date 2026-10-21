// Package densgo berisi klien HTTP untuk API publik Dens TV (Densgo).
//
// API ini menyediakan dua jenis konten dari satu basis URL, dibedakan oleh
// parameter type pada endpoint daftar:
//
//	GET /index?q=&type=movie|series&page=N&limit=30
//	GET /episodes?seriesId=10144
//
// Karena itu NewClient menerima kategori, dan satu Client hanya melayani satu
// kategori (movie atau series). Hasilnya dipetakan ke tipe internal/catalog,
// sehingga *Client bisa langsung dipakai sebagai catalog.Source:
//
//	c := densgo.NewClient("https://pp.dens.tv:777/index.php/v3070/adps_list_movies_v1", 30*time.Second, catalog.CategoryMovie)
//	page, err := c.ListSeries(ctx, 1)
//
// Film tidak punya endpoint episode yang berarti: URL playback film sudah
// tersedia di Series.StreamURL, jadi ListEpisodes untuk movie mengembalikan
// halaman kosong tanpa memanggil API.
package densgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vodlocal/internal/catalog"
	"vodlocal/internal/provider"
)

const (
	// ProviderCode adalah nilai Code() dan Series.Provider untuk provider ini.
	ProviderCode = "densgo"

	// DefaultTimeout dipakai NewClient bila timeout yang diberikan tidak
	// positif (lihat provider.NewClient).
	DefaultTimeout = provider.DefaultTimeout

	// pageLimit adalah jumlah item per halaman yang diminta ke API.
	pageLimit = 30

	// userAgent dikirim pada setiap permintaan. Dens TV berada di belakang
	// CDN yang menolak klien tanpa User-Agent yang masuk akal.
	userAgent = "Mozilla/5.0 (compatible; VodLocal/1.0)"

	// movieDevice adalah label perangkat untuk stream film/episode Dens TV.
	movieDevice = "android"
)

// ErrUnknownCategory dilaporkan bila Client dibuat untuk kategori selain
// catalog.CategoryMovie dan catalog.CategorySeries.
var ErrUnknownCategory = errors.New("densgo: kategori tidak didukung")

// Client adalah klien API Dens TV untuk satu kategori.
//
// provider.Client yang di-embed menyediakan BaseURL, HTTP, Header dan GetJSON
// (pemetaan error transport/HTTP/JSON ke sentinel provider.Err*).
type Client struct {
	*provider.Client

	// category adalah catalog.CategoryMovie atau catalog.CategorySeries.
	category string
}

// Pastikan Client memenuhi kontrak catalog.Source.
var _ catalog.Source = (*Client)(nil)

// NewClient mengembalikan Client untuk baseURL dengan batas waktu timeout per
// permintaan. category harus catalog.CategoryMovie atau catalog.CategorySeries;
// nilai lain baru dilaporkan sebagai error oleh ListSeries/ListEpisodes.
func NewClient(baseURL string, timeout time.Duration, category string) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	httpClient := provider.NewClient(baseURL, timeout)
	httpClient.Header = http.Header{"User-Agent": {userAgent}}

	return &Client{
		Client:   httpClient,
		category: normalizeCategory(category),
	}
}

// Code mengembalikan kode provider, selalu "densgo".
func (c *Client) Code() string { return ProviderCode }

// Category mengembalikan kategori yang dilayani client ini.
func (c *Client) Category() string {
	if c == nil {
		return ""
	}
	return c.category
}

// ListSeries mengambil satu halaman daftar film atau serial dari GET /index.
// Halaman kurang dari 1 diperlakukan sebagai halaman 1, dan jumlah item per
// halaman selalu 10.
//
// Hasilnya sudah dipetakan ke catalog.SeriesPage: Provider selalu "densgo",
// Category selalu kategori client, Slug sama dengan ID (API hanya punya id),
// dan EpisodeCount selalu 0 karena daftar tidak menyebutkan jumlah episode.
func (c *Client) ListSeries(ctx context.Context, page int) (catalog.SeriesPage, error) {
	if err := c.validate(); err != nil {
		return catalog.SeriesPage{}, err
	}
	page = normalizePage(page)

	var payload listResponse
	if err := c.GetJSON(ctx, "/index", listQuery(c.category, page), &payload); err != nil {
		return catalog.SeriesPage{}, err
	}

	result := catalog.SeriesPage{
		Items:    make([]catalog.Series, 0, len(payload.Data)),
		Page:     int(payload.CurrentPage),
		PerPage:  int(payload.PerPage),
		Total:    int(payload.TotalData),
		LastPage: int(payload.TotalPages),
	}
	if result.Page < 1 {
		// current_page tidak ada atau aneh: laporkan halaman yang diminta
		// supaya pemanggil tetap punya penanda halaman yang konsisten.
		result.Page = page
	}
	if result.LastPage < page {
		// total_pages hilang/lebih kecil dari halaman yang diminta; jangan
		// sampai pemanggil mengira halaman ini di luar jangkauan.
		result.LastPage = page
	}

	for _, item := range payload.Data {
		result.Items = append(result.Items, item.series(c.category))
	}
	return result, nil
}

// ListEpisodes mengambil daftar episode satu serial dari GET /episodes.
//
// API mengabaikan nomor halaman dan selalu mengembalikan seluruh episode
// sekaligus, jadi page tidak dikirim dan hasilnya selalu dilaporkan sebagai
// satu halaman (Page 1, LastPage 1, PerPage/Total = jumlah episode).
//
// Untuk client berkategori movie tidak ada yang bisa diambil: ingest memakai
// Series.StreamURL, jadi halaman kosong dikembalikan tanpa memanggil API.
func (c *Client) ListEpisodes(ctx context.Context, seriesID string, page int) (catalog.EpisodePage, error) {
	if c == nil {
		return catalog.EpisodePage{}, errors.New("densgo: client nil")
	}

	switch c.category {
	case catalog.CategoryMovie:
		return catalog.EpisodePage{
			Items:    make([]catalog.Episode, 0),
			Page:     1,
			LastPage: 1,
		}, nil
	case catalog.CategorySeries:
	default:
		return catalog.EpisodePage{}, c.unknownCategoryError()
	}

	query := url.Values{"seriesId": {strings.TrimSpace(seriesID)}}

	var payload episodesResponse
	if err := c.GetJSON(ctx, "/episodes", query, &payload); err != nil {
		return catalog.EpisodePage{}, err
	}

	result := catalog.EpisodePage{
		Items:    make([]catalog.Episode, 0, len(payload.Data)),
		Page:     1,
		PerPage:  len(payload.Data),
		Total:    len(payload.Data),
		LastPage: 1,
	}
	for i, item := range payload.Data {
		result.Items = append(result.Items, item.episode(i+1))
	}
	return result, nil
}

// validate memastikan client dan kategorinya bisa dipakai.
func (c *Client) validate() error {
	if c == nil {
		return errors.New("densgo: client nil")
	}
	switch c.category {
	case catalog.CategoryMovie, catalog.CategorySeries:
		return nil
	default:
		return c.unknownCategoryError()
	}
}

// unknownCategoryError menjelaskan kategori yang tidak dikenal beserta nilai
// yang benar-benar didukung.
func (c *Client) unknownCategoryError() error {
	return fmt.Errorf("%w: %q (gunakan %q atau %q)",
		ErrUnknownCategory, c.category, catalog.CategoryMovie, catalog.CategorySeries)
}

// normalizeCategory merapikan kategori supaya "Series " tetap diterima.
func normalizeCategory(category string) string {
	return strings.ToLower(strings.TrimSpace(category))
}

// normalizePage memaksa nomor halaman minimal 1.
func normalizePage(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// listQuery membangun query GET /index.
func listQuery(category string, page int) url.Values {
	return url.Values{
		"q":     {""},
		"type":  {category},
		"page":  {strconv.Itoa(normalizePage(page))},
		"limit": {strconv.Itoa(pageLimit)},
	}
}

// deref mengembalikan nilai yang ditunjuk s, atau "" bila s nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// flexInt membaca angka JSON yang bisa dikirim sebagai string ("10") maupun
// angka (10). API mencampur keduanya (current_page/per_page string,
// total_data/total_pages angka), jadi kedua bentuk diterima dan nilai yang
// tidak bisa dibaca menjadi 0, bukan error: pagination yang aneh tidak boleh
// menggagalkan seluruh halaman.
type flexInt int

// UnmarshalJSON memenuhi json.Unmarshaler untuk flexInt.
func (f *flexInt) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*f = 0
		return nil
	}

	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		text = strings.TrimSpace(s)
		if text == "" {
			*f = 0
			return nil
		}
	}

	if n, err := strconv.Atoi(text); err == nil {
		*f = flexInt(n)
		return nil
	}
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		*f = flexInt(int(v))
		return nil
	}

	*f = 0
	return nil
}

// listResponse adalah envelope respons GET /index.
type listResponse struct {
	CurrentPage flexInt    `json:"current_page"`
	PerPage     flexInt    `json:"per_page"`
	TotalData   flexInt    `json:"total_data"`
	TotalPages  flexInt    `json:"total_pages"`
	Data        []listItem `json:"data"`
}

// listItem adalah satu film atau serial pada daftar.
type listItem struct {
	ID          string `json:"id"`
	MovieCode   string `json:"movie_code"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	PosterURL   string `json:"poster_url"`
	// StreamURL null pada serial, terisi pada film.
	StreamURL *string `json:"stream_url"`
}

// series memetakan item daftar menjadi catalog.Series.
func (it listItem) series(category string) catalog.Series {
	return catalog.Series{
		Provider:     ProviderCode,
		Category:     category,
		ID:           it.ID,
		Slug:         it.ID,
		Title:        it.Title,
		Description:  it.Description,
		PosterURL:    it.PosterURL,
		StreamURL:    deref(it.StreamURL),
		EpisodeCount: 0, // daftar tidak menyebutkan jumlah episode
	}
}

// episodesResponse adalah respons GET /episodes: hanya data, tanpa meta
// pagination.
type episodesResponse struct {
	Data []episodeItem `json:"data"`
}

// episodeItem adalah satu episode pada daftar episode.
type episodeItem struct {
	ID          string  `json:"id"`
	MovieCode   string  `json:"movie_code"`
	Type        string  `json:"type"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	PosterURL   string  `json:"poster_url"`
	StreamURL   *string `json:"stream_url"`
}

// episode memetakan item episode menjadi catalog.Episode dengan nomor urut
// number (API tidak mengirim nomor episode).
//
// DurationSeconds sengaja 0: API mengirim durasi yang tidak bisa dipercaya
// sebagai detik, jadi durasi dibiarkan dihitung HLS di hilir.
func (it episodeItem) episode(number int) catalog.Episode {
	episode := catalog.Episode{
		ID:              it.ID,
		Slug:            it.ID,
		Title:           it.Title,
		Season:          1,
		Number:          number,
		DurationSeconds: 0,
		PreviewURL:      "",
		Streams:         make([]catalog.Stream, 0, 1),
	}

	if streamURL := deref(it.StreamURL); streamURL != "" {
		episode.Streams = append(episode.Streams, catalog.Stream{
			Device: movieDevice,
			URL:    streamURL,
		})
	}
	return episode
}
