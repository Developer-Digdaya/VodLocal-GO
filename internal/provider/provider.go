// Package provider is the abstraction boundary between the catalog and the
// upstream streaming services (Dens TV, Dramahits).
//
// The real upstream API contract is not available yet, so this package fixes
// only the vocabulary the rest of the application depends on: the model types
// below, the Provider interface every adapter implements, the sentinel errors
// an adapter must map its failures to, an in-memory Fake for tests (fake.go)
// and a small HTTP helper for future HTTP adapters (httpclient.go).
//
// Nothing in this package knows the wire format of a concrete service; the
// per-service adapters live in their own packages and translate that wire
// format into these types.
package provider

import (
	"context"
	"errors"
)

// Values accepted by Title.Kind.
const (
	KindMovie  = "movie"
	KindSeries = "series"
)

// Values accepted by Variant.Container.
const (
	ContainerTS   = "ts"
	ContainerFMP4 = "fmp4"
)

// Values accepted by Subtitle.Format.
const (
	FormatVTT = "vtt"
	FormatSRT = "srt"
)

// Title is a movie or a series as the upstream service describes it.
//
// ExternalID is the identifier the upstream service uses; it is the value the
// caller passes back to ListEpisodes and GetPlayables. DurationSeconds is only
// meaningful for movies (a series has no single duration).
type Title struct {
	ExternalID      string
	Kind            string // "movie" | "series"
	Title           string
	Slug            string
	Synopsis        string
	ReleaseYear     int
	ContentRating   string
	Language        string
	Country         string
	PosterURL       string
	BackdropURL     string
	DurationSeconds int
	Genres          []string
}

// Episode is one episode of a series. ExternalID is the upstream identifier of
// the episode, not of the series.
type Episode struct {
	ExternalID      string
	SeasonNumber    int
	EpisodeNumber   int
	Name            string
	Synopsis        string
	DurationSeconds int
	AirDate         string
	StillURL        string
}

// Variant is one encodable rendition of a playable: a quality label, the
// technical parameters needed to advertise it and the playlist that serves it.
type Variant struct {
	Quality     string // "360p","480p","720p"
	BitrateKbps int
	Width       int
	Height      int
	Container   string // "ts" | "fmp4"
	PlaylistURL string
}

// Subtitle is one sidecar subtitle track of a playable.
type Subtitle struct {
	Lang   string
	Label  string
	Format string // "vtt" | "srt"
	URL    string
}

// Playable is one thing that can be played: a whole movie, or a single episode
// of a series. EpisodeExternalID is empty for a movie.
type Playable struct {
	ExternalID        string
	TitleExternalID   string
	EpisodeExternalID string // kosong untuk movie
	DurationSeconds   int
	Encrypted         bool
	Variants          []Variant
	Subtitles         []Subtitle
}

// Provider is the contract every upstream adapter implements.
//
// Implementations must return the sentinel errors of this package (wrapped or
// bare) so that callers can classify failures with errors.Is:
// ErrNotFound, ErrUpstream, ErrUpstreamTimeout and ErrBadResponse.
type Provider interface {
	// Code is the stable short name of the service, e.g. "fake".
	Code() string

	// ListTitles returns the catalog of the service.
	ListTitles(ctx context.Context) ([]Title, error)

	// ListEpisodes returns the episodes of one series, identified by its
	// upstream identifier.
	ListEpisodes(ctx context.Context, titleExternalID string) ([]Episode, error)

	// GetPlayables returns everything that can be played for a title: the
	// movie itself, or every episode of the series.
	GetPlayables(ctx context.Context, titleExternalID string) ([]Playable, error)
}

// Errors returned by a Provider. Adapters wrap them with fmt.Errorf("%w: ...")
// so that callers can test them with errors.Is while still getting context.
var (
	// ErrNotFound reports that the upstream service has no such title,
	// episode or playable.
	ErrNotFound = errors.New("provider: data tidak ditemukan")

	// ErrUpstream reports that the upstream service answered with a server
	// error or could not be reached at all.
	ErrUpstream = errors.New("provider: layanan upstream bermasalah")

	// ErrUpstreamTimeout reports that the upstream service did not answer in
	// time (request deadline, 408 or 504).
	ErrUpstreamTimeout = errors.New("provider: upstream timeout")

	// ErrBadResponse reports that the upstream reply could not be understood:
	// unparseable JSON, an unexpected status or an oversized body.
	ErrBadResponse = errors.New("provider: respons tidak valid")
)
