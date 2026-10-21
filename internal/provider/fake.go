package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory Provider for tests and for local development without
// network access. It answers deterministically from the data given to NewFake,
// in the order that data was supplied.
//
// Two fields allow a test to inject behaviour:
//
//   - FailWith, when not nil, is returned by every method instead of data.
//   - Delay, when positive, makes every method wait before answering; the wait
//     is cut short by the caller's context, so a test can exercise timeouts
//     without sleeping for real.
//
// Set FailWith and Delay before the Fake is shared between goroutines: the
// fields are plain exported fields, while the data itself is guarded by an
// internal RWMutex, so a Fake is safe for concurrent use as long as those two
// fields are not mutated while calls are in flight.
type Fake struct {
	mu sync.RWMutex

	titles    []Title
	episodes  map[string][]Episode
	playables map[string][]Playable

	// FailWith is returned by every method when it is not nil.
	FailWith error

	// Delay is slept by every method before it answers.
	Delay time.Duration
}

// NewFake returns a Fake serving the given data. The input is copied, so the
// caller may keep mutating its own slices afterwards. A nil map or a nil slice
// means "nothing to serve": episodes and playables for an unknown title make
// the corresponding method return ErrNotFound.
func NewFake(titles []Title, episodes map[string][]Episode, playables map[string][]Playable) *Fake {
	f := &Fake{
		titles:    cloneTitles(titles),
		episodes:  make(map[string][]Episode, len(episodes)),
		playables: make(map[string][]Playable, len(playables)),
	}
	for id, list := range episodes {
		f.episodes[id] = cloneEpisodes(list)
	}
	for id, list := range playables {
		f.playables[id] = clonePlayables(list)
	}
	return f
}

// Fake implements Provider.
var _ Provider = (*Fake)(nil)

// Code implements Provider.
func (f *Fake) Code() string { return "fake" }

// ListTitles implements Provider. It returns every title given to NewFake, in
// the order it was given.
func (f *Fake) ListTitles(ctx context.Context) ([]Title, error) {
	if err := f.gate(ctx); err != nil {
		return nil, err
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	return cloneTitles(f.titles), nil
}

// ListEpisodes implements Provider. It returns ErrNotFound for a title that was
// not given to NewFake.
func (f *Fake) ListEpisodes(ctx context.Context, titleExternalID string) ([]Episode, error) {
	if err := f.gate(ctx); err != nil {
		return nil, err
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	list, ok := f.episodes[titleExternalID]
	if !ok {
		return nil, fmt.Errorf("%w: episode untuk title %q", ErrNotFound, titleExternalID)
	}
	return cloneEpisodes(list), nil
}

// GetPlayables implements Provider. It returns ErrNotFound for a title that was
// not given to NewFake.
func (f *Fake) GetPlayables(ctx context.Context, titleExternalID string) ([]Playable, error) {
	if err := f.gate(ctx); err != nil {
		return nil, err
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	list, ok := f.playables[titleExternalID]
	if !ok {
		return nil, fmt.Errorf("%w: playable untuk title %q", ErrNotFound, titleExternalID)
	}
	return clonePlayables(list), nil
}

// gate enforces the injected failure and latency and honours ctx. Every method
// calls it before touching the data.
func (f *Fake) gate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	f.mu.RLock()
	failWith, delay := f.FailWith, f.Delay
	f.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	if failWith != nil {
		return failWith
	}
	return nil
}

// cloneTitles returns a copy of in that shares no memory with it.
func cloneTitles(in []Title) []Title {
	if in == nil {
		return nil
	}
	out := make([]Title, len(in))
	copy(out, in)
	for i := range out {
		if in[i].Genres != nil {
			out[i].Genres = append([]string(nil), in[i].Genres...)
		}
	}
	return out
}

// cloneEpisodes returns a copy of in that shares no memory with it.
func cloneEpisodes(in []Episode) []Episode {
	if in == nil {
		return nil
	}
	out := make([]Episode, len(in))
	copy(out, in)
	return out
}

// clonePlayables returns a copy of in that shares no memory with it.
func clonePlayables(in []Playable) []Playable {
	if in == nil {
		return nil
	}
	out := make([]Playable, len(in))
	copy(out, in)
	for i := range out {
		if in[i].Variants != nil {
			out[i].Variants = append([]Variant(nil), in[i].Variants...)
		}
		if in[i].Subtitles != nil {
			out[i].Subtitles = append([]Subtitle(nil), in[i].Subtitles...)
		}
	}
	return out
}
