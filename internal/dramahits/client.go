// Package dramahits berisi klien HTTP untuk API staging Dramahits.
//
// Klien ini membaca dua endpoint penyedia konten:
//
//	GET /series?page[number]=N
//	GET /series/{slug}/episodes?page[number]=N
//
// lalu memetakan respons JSON-nya ke tipe internal/catalog. Karena itu
// *Client bisa dipakai langsung sebagai catalog.Source oleh worker ingest.
//
// Contoh pemakaian:
//
//	c := dramahits.NewClient("https://stage-microdrama.digdaya.co.id/api/v1", 30*time.Second)
//	page, err := c.ListSeries(ctx, 1)
package dramahits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vodlocal/internal/catalog"
)

const (
	// providerCode adalah nilai Code() dan Series.Provider untuk provider ini.
	providerCode = "dramahits"

	// DefaultTimeout dipakai NewClient bila timeout yang diberikan tidak
	// positif, karena timeout nol pada http.Client berarti "tanpa timeout".
	DefaultTimeout = 30 * time.Second

	// MaxResponseBytes membatasi body respons yang mau dibaca (4 MB). Body
	// yang lebih besar ditolak, bukan dibuffer tanpa batas.
	MaxResponseBytes int64 = 4 << 20

	// maxErrorSnippet membatasi potongan body error yang dikutip di pesan.
	maxErrorSnippet = 256
)

// Client adalah klien API Dramahits. Nilai nol dari Client tetap bisa dipakai
// asalkan BaseURL diisi: HTTP akan jatuh ke http.DefaultClient.
type Client struct {
	// BaseURL adalah akar API, misalnya
	// "https://stage-microdrama.digdaya.co.id/api/v1". Garis miring di akhir
	// diabaikan.
	BaseURL string

	// HTTP adalah klien yang dipakai untuk setiap permintaan. Biasanya diisi
	// NewClient, yang sekaligus memberinya timeout.
	HTTP *http.Client
}

// Pastikan Client memenuhi kontrak provider.
var _ catalog.Source = (*Client)(nil)

// NewClient mengembalikan Client untuk baseURL dengan batas waktu timeout per
// permintaan. Timeout yang tidak positif memakai DefaultTimeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// Code mengembalikan kode provider, selalu "dramahits".
func (c *Client) Code() string { return providerCode }

// Category mengembalikan kategori konten Dramahits, selalu short.
func (c *Client) Category() string { return catalog.CategoryShort }

// ListSeries mengambil satu halaman daftar series dari GET /series. Halaman
// kurang dari 1 diperlakukan sebagai halaman 1.
//
// Hasilnya sudah dipetakan ke catalog.SeriesPage: Provider selalu "dramahits"
// dan Category selalu catalog.CategoryShort.
func (c *Client) ListSeries(ctx context.Context, page int) (catalog.SeriesPage, error) {
	page = normalizePage(page)

	var payload seriesResponse
	if err := c.get(ctx, "/series", pageQuery(page), &payload); err != nil {
		return catalog.SeriesPage{}, err
	}

	result := catalog.SeriesPage{
		Items:    make([]catalog.Series, 0, len(payload.Data)),
		Page:     payload.Meta.CurrentPage,
		PerPage:  payload.Meta.PerPage,
		Total:    payload.Meta.Total,
		LastPage: payload.Meta.LastPage,
	}
	if result.Page < 1 {
		// meta.current_page tidak ada/aneh: laporkan halaman yang diminta
		// supaya pemanggil tetap punya penanda halaman yang konsisten.
		result.Page = page
	}

	for _, item := range payload.Data {
		result.Items = append(result.Items, item.series())
	}
	return result, nil
}

// ListEpisodes mengambil satu halaman episode dari
// GET /series/{slug}/episodes. Halaman kurang dari 1 diperlakukan sebagai
// halaman 1, dan seriesSlug di-escape sebagai satu segmen path.
func (c *Client) ListEpisodes(ctx context.Context, seriesSlug string, page int) (catalog.EpisodePage, error) {
	page = normalizePage(page)
	path := "/series/" + url.PathEscape(seriesSlug) + "/episodes"

	var payload episodeResponse
	if err := c.get(ctx, path, pageQuery(page), &payload); err != nil {
		return catalog.EpisodePage{}, err
	}

	result := catalog.EpisodePage{
		Items:    make([]catalog.Episode, 0, len(payload.Data)),
		Page:     payload.Meta.CurrentPage,
		PerPage:  payload.Meta.PerPage,
		Total:    payload.Meta.Total,
		LastPage: payload.Meta.LastPage,
	}
	if result.Page < 1 {
		result.Page = page
	}

	for _, item := range payload.Data {
		result.Items = append(result.Items, item.episode())
	}
	return result, nil
}

// get mengirim GET ke path (relatif terhadap BaseURL) dengan query, lalu
// men-decode body JSON ke out.
//
// Kegagalan transport dikembalikan dengan error aslinya tetap terbungkus
// (errors.Is(err, context.Canceled) tetap berlaku). Status non-2xx menjadi
// error yang memuat kode status dan path, sedangkan JSON rusak atau body
// melebihi MaxResponseBytes juga menjadi error.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if c == nil {
		return errors.New("dramahits: client nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dramahits: GET %s dibatalkan: %w", path, err)
	}

	endpoint := c.endpoint(path, query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("dramahits: menyusun GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("dramahits: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Baca satu byte di atas batas supaya body kebesaran bisa dideteksi tanpa
	// membuffer seluruhnya.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("dramahits: GET %s: membaca body: %w", path, err)
	}
	if int64(len(body)) > MaxResponseBytes {
		return fmt.Errorf("dramahits: GET %s: body melebihi %d byte", path, MaxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("dramahits: GET %s: status %d%s", path, resp.StatusCode, errorSnippet(body))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("dramahits: GET %s: decode JSON: %w", path, err)
	}
	return nil
}

// endpoint menggabungkan BaseURL, path, dan query menjadi URL absolut.
func (c *Client) endpoint(path string, query url.Values) string {
	base := strings.TrimRight(c.BaseURL, "/")
	rel := strings.TrimLeft(path, "/")

	endpoint := base
	switch {
	case rel == "":
	case base == "":
		endpoint = "/" + rel
	default:
		endpoint = base + "/" + rel
	}

	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return endpoint
}

// httpClient mengembalikan klien HTTP yang dipakai.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// normalizePage memaksa nomor halaman minimal 1.
func normalizePage(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// pageQuery membangun query pagination Dramahits: page[number]=N.
func pageQuery(page int) url.Values {
	return url.Values{"page[number]": {strconv.Itoa(normalizePage(page))}}
}

// errorSnippet merender body sebagai sufiks ": <potongan>", atau "" bila
// kosong, supaya kegagalan upstream tetap bisa didiagnosis.
func errorSnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	if len(s) > maxErrorSnippet {
		s = s[:maxErrorSnippet] + "..."
	}
	return ": " + s
}

// pageMeta adalah blok meta pagination pada respons Dramahits.
type pageMeta struct {
	CurrentPage int `json:"current_page"`
	LastPage    int `json:"last_page"`
	PerPage     int `json:"per_page"`
	Total       int `json:"total"`
}

// seriesResponse adalah envelope respons GET /series.
type seriesResponse struct {
	Data []seriesItem `json:"data"`
	Meta pageMeta     `json:"meta"`
}

// seriesItem adalah satu item pada daftar series.
type seriesItem struct {
	ID             string        `json:"id"`
	Type           string        `json:"type"`
	Title          string        `json:"title"`
	Slug           string        `json:"slug"`
	Description    string        `json:"description"`
	AccessType     string        `json:"access_type"`
	AgeRating      string        `json:"age_rating"`
	AgeRatingLabel string        `json:"age_rating_label"`
	PublishedAt    string        `json:"published_at"`
	Media          media         `json:"media"`
	Details        seriesDetails `json:"details"`
	Genres         []string      `json:"genres"`
}

// seriesDetails memuat angka-angka ringkas sebuah series. release_year
// disimpan mentah karena API bisa mengirim string maupun angka.
type seriesDetails struct {
	TotalSeasons  int             `json:"total_seasons"`
	TotalEpisodes int             `json:"total_episodes"`
	ReleaseYear   json.RawMessage `json:"release_year"`
}

// media memuat aset gambar sebuah item.
type media struct {
	PosterVertical artwork `json:"poster_vertical"`
}

// artwork adalah satu aset gambar.
type artwork struct {
	OriginalURL string `json:"original_url"`
}

// episodeResponse adalah envelope respons GET /series/{slug}/episodes.
type episodeResponse struct {
	Data []episodeItem `json:"data"`
	Meta pageMeta      `json:"meta"`
}

// episodeItem adalah satu item pada daftar episode.
type episodeItem struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Slug        string         `json:"slug"`
	PublishedAt string         `json:"published_at"`
	Details     episodeDetails `json:"details"`
}

// episodeDetails memuat detail satu episode.
type episodeDetails struct {
	EpisodeNumber int     `json:"episode_number"`
	Duration      int     `json:"duration"`
	Season        season  `json:"season"`
	Streams       streams `json:"streams"`
}

// season adalah musim tempat episode berada.
type season struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
}

// streams mengelompokkan URL playback sebuah episode.
type streams struct {
	Main    []streamItem `json:"main"`
	Preview []streamItem `json:"preview"`
}

// streamItem adalah satu URL playback. expires_at bisa null sehingga
// di-decode sebagai string kosong.
type streamItem struct {
	Type          string `json:"type"`
	Device        string `json:"device"`
	URL           string `json:"url"`
	TokenRequired bool   `json:"token_required"`
	ExpiresAt     string `json:"expires_at"`
}

// series memetakan item API menjadi catalog.Series.
func (it seriesItem) series() catalog.Series {
	ageRating := strings.TrimSpace(it.AgeRatingLabel)
	if ageRating == "" {
		ageRating = it.AgeRating
	}

	return catalog.Series{
		Provider:     providerCode,
		Category:     catalog.CategoryShort,
		ID:           it.ID,
		Slug:         it.Slug,
		Title:        it.Title,
		Description:  it.Description,
		PosterURL:    it.Media.PosterVertical.OriginalURL,
		Year:         parseYear(it.Details.ReleaseYear),
		EpisodeCount: it.Details.TotalEpisodes,
		AgeRating:    ageRating,
		Genres:       copyStrings(it.Genres),
		PublishedAt:  it.PublishedAt,
	}
}

// episode memetakan item API menjadi catalog.Episode. Streams hanya diambil
// dari details.streams.main, sedangkan preview diambil dari elemen pertama
// details.streams.preview.
func (it episodeItem) episode() catalog.Episode {
	episode := catalog.Episode{
		ID:              it.ID,
		Slug:            it.Slug,
		Title:           it.Title,
		Season:          it.Details.Season.Number,
		Number:          it.Details.EpisodeNumber,
		DurationSeconds: it.Details.Duration,
		PublishedAt:     it.PublishedAt,
		Streams:         make([]catalog.Stream, 0, len(it.Details.Streams.Main)),
	}

	for _, stream := range it.Details.Streams.Main {
		episode.Streams = append(episode.Streams, catalog.Stream{
			Device:        stream.Device,
			URL:           stream.URL,
			TokenRequired: stream.TokenRequired,
			ExpiresAt:     stream.ExpiresAt,
		})
	}

	if preview := it.Details.Streams.Preview; len(preview) > 0 {
		episode.PreviewURL = preview[0].URL
	}
	return episode
}

// copyStrings mengembalikan salinan values, atau slice kosong yang non-nil
// bila tidak ada nilai, supaya pemanggil tidak menerima slice nil yang
// membingungkan saat di-encode ke JSON.
func copyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	return append(out, values...)
}

// parseYear membaca details.release_year yang bisa berupa string ("2025"),
// angka (2025), atau angka pecahan (2025.0). Nilai yang tidak bisa dibaca
// menjadi 0.
func parseYear(raw json.RawMessage) int {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}

	if s[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0
		}
		s = strings.TrimSpace(text)
		if s == "" {
			return 0
		}
	}

	if year, err := strconv.Atoi(s); err == nil {
		return year
	}
	if year, err := strconv.ParseFloat(s, 64); err == nil {
		return int(year)
	}
	return 0
}
