// Package catalog mendefinisikan kontrak data untuk pemilihan konten dari
// provider. Tipe di sini dipakai bersama oleh klien provider, worker ingest,
// dan panel admin, sehingga tidak ada paket yang saling mengimpor secara
// melingkar.
package catalog

import "context"

// Kategori konten bersifat statis per provider.
const (
	// CategoryShort dipakai Dramahits.
	CategoryShort = "short"
	// CategoryVOD dipakai Densgo.
	CategoryVOD = "vod"
)

// Series adalah satu judul yang bisa dipilih dari daftar konten.
type Series struct {
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
}

// SeriesPage adalah satu halaman daftar series.
type SeriesPage struct {
	Items    []Series `json:"items"`
	Page     int      `json:"page"`
	PerPage  int      `json:"per_page"`
	Total    int      `json:"total"`
	LastPage int      `json:"last_page"`
}

// Stream adalah satu URL playback yang disediakan provider.
type Stream struct {
	Device        string `json:"device"`
	URL           string `json:"url"`
	TokenRequired bool   `json:"token_required"`
	ExpiresAt     string `json:"expires_at"`
}

// Episode adalah satu episode dari sebuah series.
type Episode struct {
	ID              string   `json:"id"`
	Slug            string   `json:"slug"`
	Title           string   `json:"title"`
	Season          int      `json:"season"`
	Number          int      `json:"number"`
	DurationSeconds int      `json:"duration_seconds"`
	PublishedAt     string   `json:"published_at"`
	Streams         []Stream `json:"streams"`
	PreviewURL      string   `json:"preview_url"`
}

// EpisodePage adalah satu halaman daftar episode.
type EpisodePage struct {
	Items    []Episode `json:"items"`
	Page     int       `json:"page"`
	PerPage  int       `json:"per_page"`
	Total    int       `json:"total"`
	LastPage int       `json:"last_page"`
}

// Source adalah provider yang bisa menyuplai daftar series dan episode.
type Source interface {
	Code() string
	ListSeries(ctx context.Context, page int) (SeriesPage, error)
	ListEpisodes(ctx context.Context, seriesSlug string, page int) (EpisodePage, error)
}
