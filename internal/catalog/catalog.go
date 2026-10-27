// Package catalog mendefinisikan kontrak data untuk pemilihan konten dari
// provider. Tipe di sini dipakai bersama oleh klien provider, worker ingest,
// dan panel admin, sehingga tidak ada paket yang saling mengimpor secara
// melingkar.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Kategori konten.
const (
	// CategoryShort dipakai Dramahits.
	CategoryShort = "short"
	// CategoryMovie dipakai film dari Densgo.
	CategoryMovie = "movie"
	// CategorySeries dipakai serial dari Densgo.
	CategorySeries = "series"
)

// ErrUnknownCategory dilaporkan bila tidak ada source untuk kategori yang
// diminta.
var ErrUnknownCategory = errors.New("catalog: kategori tidak dikenal")

// Series adalah satu judul yang bisa dipilih dari daftar konten. StreamURL
// hanya diisi untuk movie; series mengambil URL dari endpoint episode.
type Series struct {
	Provider     string `json:"provider"`
	Category     string `json:"category"`
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	PosterURL    string `json:"poster_url"`
	StreamURL    string `json:"stream_url,omitempty"`
	Year         int    `json:"year"`
	EpisodeCount int    `json:"episode_count"`
	AgeRating    string `json:"age_rating"`
	PublishedAt  string `json:"published_at"`
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
// Category membedakan dua client Densgo (movie dan series); Dramahits selalu
// short.
type Source interface {
	Code() string
	Category() string
	ListSeries(ctx context.Context, page int) (SeriesPage, error)
	ListEpisodes(ctx context.Context, seriesSlug string, page int) (EpisodePage, error)
}

// SourceSet adalah kumpulan source yang bisa dipilih berdasarkan provider dan
// kategori. Satu provider (Densgo) boleh punya lebih dari satu source.
type SourceSet []Source

// For mengembalikan source yang cocok dengan provider dan kategori. Nilai
// kosong pada salah satu argumen berarti wildcard.
func (s SourceSet) For(provider, category string) (Source, bool) {
	provider = strings.TrimSpace(provider)
	category = strings.TrimSpace(category)

	for _, src := range s {
		if src == nil {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(src.Code()), provider) {
			continue
		}
		if category != "" && !strings.EqualFold(strings.TrimSpace(src.Category()), category) {
			continue
		}
		return src, true
	}
	return nil, false
}

// ByCategory mengembalikan source pertama untuk kategori tersebut.
func (s SourceSet) ByCategory(category string) (Source, bool) {
	category = strings.TrimSpace(category)
	for _, src := range s {
		if src == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(src.Category()), category) {
			return src, true
		}
	}
	return nil, false
}

// ListSeries memenuhi kebutuhan panel admin: pilih source berdasarkan kategori,
// lalu ambil satu halaman daftar.
func (s SourceSet) ListSeries(ctx context.Context, category string, page int) (SeriesPage, error) {
	src, ok := s.ByCategory(category)
	if !ok {
		return SeriesPage{}, fmt.Errorf("%w: %q", ErrUnknownCategory, category)
	}
	return src.ListSeries(ctx, page)
}
