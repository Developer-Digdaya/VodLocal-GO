package provider

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// sampleTitles is the fixture shared by the fake tests: one movie and one
// series, with every field populated so that a copy that drops something is
// visible.
func sampleTitles() []Title {
	return []Title{
		{
			ExternalID:      "mv-1",
			Kind:            KindMovie,
			Title:           "Hujan di Bulan Juni",
			Slug:            "hujan-di-bulan-juni",
			Synopsis:        "Kisah dua mahasiswa yang berbeda keyakinan.",
			ReleaseYear:     2016,
			ContentRating:   "13+",
			Language:        "id",
			Country:         "ID",
			PosterURL:       "https://cdn.example.com/mv-1/poster.jpg",
			BackdropURL:     "https://cdn.example.com/mv-1/backdrop.jpg",
			DurationSeconds: 6480,
			Genres:          []string{"drama", "romance"},
		},
		{
			ExternalID:    "sr-1",
			Kind:          KindSeries,
			Title:         "Rahasia Keluarga",
			Slug:          "rahasia-keluarga",
			ReleaseYear:   2021,
			ContentRating: "17+",
			Language:      "id",
			Country:       "ID",
			PosterURL:     "https://cdn.example.com/sr-1/poster.jpg",
			Genres:        []string{"drama", "mystery"},
		},
	}
}

func sampleEpisodes() map[string][]Episode {
	return map[string][]Episode{
		"sr-1": {
			{
				ExternalID:      "sr-1-e1",
				SeasonNumber:    1,
				EpisodeNumber:   1,
				Name:            "Awal",
				Synopsis:        "Semua dimulai.",
				DurationSeconds: 2700,
				AirDate:         "2021-03-01",
				StillURL:        "https://cdn.example.com/sr-1/e1.jpg",
			},
			{
				ExternalID:      "sr-1-e2",
				SeasonNumber:    1,
				EpisodeNumber:   2,
				Name:            "Rahasia",
				DurationSeconds: 2640,
				AirDate:         "2021-03-08",
			},
		},
	}
}

func samplePlayables() map[string][]Playable {
	return map[string][]Playable{
		"mv-1": {
			{
				ExternalID:      "mv-1",
				TitleExternalID: "mv-1",
				DurationSeconds: 6480,
				Encrypted:       true,
				Variants: []Variant{
					{Quality: "360p", BitrateKbps: 400, Width: 640, Height: 360, Container: ContainerTS, PlaylistURL: "https://cdn.example.com/mv-1/360p/index.m3u8"},
					{Quality: "720p", BitrateKbps: 1800, Width: 1280, Height: 720, Container: ContainerFMP4, PlaylistURL: "https://cdn.example.com/mv-1/720p/index.m3u8"},
				},
				Subtitles: []Subtitle{
					{Lang: "id", Label: "Bahasa Indonesia", Format: FormatVTT, URL: "https://cdn.example.com/mv-1/id.vtt"},
				},
			},
		},
		"sr-1": {
			{
				ExternalID:        "sr-1-e1",
				TitleExternalID:   "sr-1",
				EpisodeExternalID: "sr-1-e1",
				DurationSeconds:   2700,
				Variants: []Variant{
					{Quality: "480p", BitrateKbps: 800, Width: 854, Height: 480, Container: ContainerTS, PlaylistURL: "https://cdn.example.com/sr-1/e1/480p/index.m3u8"},
				},
			},
		},
	}
}

func newSampleFake() *Fake {
	return NewFake(sampleTitles(), sampleEpisodes(), samplePlayables())
}

// Test 1: the fake answers with exactly the data it was built with.
func TestFakeReturnsData(t *testing.T) {
	f := newSampleFake()
	ctx := context.Background()

	if got := f.Code(); got != "fake" {
		t.Errorf("Code() = %q, want %q", got, "fake")
	}

	titles, err := f.ListTitles(ctx)
	if err != nil {
		t.Fatalf("ListTitles() error = %v", err)
	}
	if want := sampleTitles(); !reflect.DeepEqual(titles, want) {
		t.Errorf("ListTitles() = %+v, want %+v", titles, want)
	}

	episodes, err := f.ListEpisodes(ctx, "sr-1")
	if err != nil {
		t.Fatalf("ListEpisodes(sr-1) error = %v", err)
	}
	if want := sampleEpisodes()["sr-1"]; !reflect.DeepEqual(episodes, want) {
		t.Errorf("ListEpisodes(sr-1) = %+v, want %+v", episodes, want)
	}

	// A movie has no episodes.
	episodes, err = f.ListEpisodes(ctx, "mv-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListEpisodes(mv-1) error = %v, want ErrNotFound", err)
	}
	if episodes != nil {
		t.Errorf("ListEpisodes(mv-1) = %+v, want nil", episodes)
	}

	playables, err := f.GetPlayables(ctx, "mv-1")
	if err != nil {
		t.Fatalf("GetPlayables(mv-1) error = %v", err)
	}
	if want := samplePlayables()["mv-1"]; !reflect.DeepEqual(playables, want) {
		t.Errorf("GetPlayables(mv-1) = %+v, want %+v", playables, want)
	}

	playables, err = f.GetPlayables(ctx, "sr-1")
	if err != nil {
		t.Fatalf("GetPlayables(sr-1) error = %v", err)
	}
	if want := samplePlayables()["sr-1"]; !reflect.DeepEqual(playables, want) {
		t.Errorf("GetPlayables(sr-1) = %+v, want %+v", playables, want)
	}

	if _, err := f.GetPlayables(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPlayables(unknown) error = %v, want ErrNotFound", err)
	}
}

// The fake must not hand out references to its own storage: a caller mutating a
// result must not change what the next call returns.
func TestFakeResultIsACopy(t *testing.T) {
	f := newSampleFake()
	ctx := context.Background()

	titles, err := f.ListTitles(ctx)
	if err != nil {
		t.Fatalf("ListTitles() error = %v", err)
	}
	titles[0].Title = "diubah"
	titles[0].Genres[0] = "diubah"
	titles = titles[:1]

	again, err := f.ListTitles(ctx)
	if err != nil {
		t.Fatalf("ListTitles() error = %v", err)
	}
	if want := sampleTitles(); !reflect.DeepEqual(again, want) {
		t.Errorf("ListTitles() after mutating the previous result = %+v, want %+v", again, want)
	}

	playables, err := f.GetPlayables(ctx, "mv-1")
	if err != nil {
		t.Fatalf("GetPlayables(mv-1) error = %v", err)
	}
	playables[0].Variants[0].Quality = "144p"
	playables[0].Subtitles[0].Lang = "en"

	again2, err := f.GetPlayables(ctx, "mv-1")
	if err != nil {
		t.Fatalf("GetPlayables(mv-1) error = %v", err)
	}
	if want := samplePlayables()["mv-1"]; !reflect.DeepEqual(again2, want) {
		t.Errorf("GetPlayables(mv-1) after mutating the previous result = %+v, want %+v", again2, want)
	}
}

// Test 2: FailWith makes every method return exactly that error.
func TestFakeFailWith(t *testing.T) {
	boom := errors.New("upstream mati")
	f := newSampleFake()
	f.FailWith = boom
	ctx := context.Background()

	if _, err := f.ListTitles(ctx); err != boom {
		t.Errorf("ListTitles() error = %v, want %v (same value)", err, boom)
	}
	if _, err := f.ListEpisodes(ctx, "sr-1"); err != boom {
		t.Errorf("ListEpisodes() error = %v, want %v (same value)", err, boom)
	}
	if _, err := f.GetPlayables(ctx, "mv-1"); err != boom {
		t.Errorf("GetPlayables() error = %v, want %v (same value)", err, boom)
	}

	// A sentinel is equally reachable through errors.Is.
	for _, sentinel := range []error{ErrNotFound, ErrUpstream, ErrUpstreamTimeout, ErrBadResponse} {
		f.FailWith = sentinel
		if _, err := f.ListTitles(ctx); !errors.Is(err, sentinel) {
			t.Errorf("ListTitles() error = %v, want errors.Is(_, %v)", err, sentinel)
		}
	}

	// Clearing FailWith restores normal service.
	f.FailWith = nil
	if titles, err := f.ListTitles(ctx); err != nil || len(titles) != 2 {
		t.Errorf("ListTitles() = %d titles, %v; want 2 titles, nil", len(titles), err)
	}
}

// Test 3: Delay is cut short by the caller's context instead of blocking.
func TestFakeDelayHonoursContext(t *testing.T) {
	f := newSampleFake()
	f.Delay = 5 * time.Second

	// A deadline shorter than the delay must win.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := f.ListTitles(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListTitles() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ListTitles() blocked for %v, want it to return as soon as the context ends", elapsed)
	}

	// An already cancelled context fails immediately on every method.
	cancelled, stop := context.WithCancel(context.Background())
	stop()

	if _, err := f.ListEpisodes(cancelled, "sr-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("ListEpisodes() error = %v, want context.Canceled", err)
	}
	if _, err := f.GetPlayables(cancelled, "mv-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetPlayables() error = %v, want context.Canceled", err)
	}

	// A context that outlives the delay gets the data.
	f.Delay = time.Millisecond
	if titles, err := f.ListTitles(context.Background()); err != nil || len(titles) != 2 {
		t.Errorf("ListTitles() = %d titles, %v; want 2 titles, nil", len(titles), err)
	}
}

// The fake is meant to be shared between goroutines (run with -race).
func TestFakeConcurrentUse(t *testing.T) {
	f := newSampleFake()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < 20; j++ {
				if _, err := f.ListTitles(ctx); err != nil {
					t.Errorf("ListTitles() error = %v", err)
					return
				}
				if _, err := f.ListEpisodes(ctx, "sr-1"); err != nil {
					t.Errorf("ListEpisodes() error = %v", err)
					return
				}
				if _, err := f.GetPlayables(ctx, "mv-1"); err != nil {
					t.Errorf("GetPlayables() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
