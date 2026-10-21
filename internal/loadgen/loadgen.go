// Package loadgen drives many concurrent HLS playback sessions against a
// running server so the streaming endpoints can be validated for a fleet of
// devices (the target here is 100 tablets).
//
// Every simulated client behaves like a player: it downloads the master
// playlist, chooses one variant, downloads that variant playlist and then keeps
// pulling its segments in order, starting over at the end, until the run is
// finished. The latency of every request is kept so the caller gets percentiles
// next to the totals, and HTTP status codes are counted per code.
//
// Errors are never fatal: a network failure or a non-2xx response is counted in
// Report.Errors and the affected client simply moves on to its next request. Run
// only returns an error for an invalid Config; an expired duration or a
// cancelled context is a normal end of the run and still returns a filled
// Report.
package loadgen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"vodlocal/internal/hls"
)

// userAgent is sent with every request so the load is recognisable in the
// server logs.
const userAgent = "vodlocal-loadgen/1.0"

// Config describes one load test.
type Config struct {
	BaseURL    string        // mis. http://127.0.0.1:8080
	PlayableID int64         // dipakai untuk membentuk URL master
	MasterPath string        // alternatif: path master playlist langsung
	Token      string        // Bearer token device (opsional untuk /media)
	Clients    int           // jumlah klien paralel
	Duration   time.Duration // durasi uji
	Ramp       time.Duration // waktu menaikkan klien secara bertahap
	Timeout    time.Duration // timeout per request
	Variant    string        // opsional: pilih varian berdasarkan substring URI
	Client     *http.Client
}

// Report is the outcome of one load test.
type Report struct {
	Clients        int
	Requests       int64
	Errors         int64
	Bytes          int64
	Duration       time.Duration
	ThroughputMbps float64
	P50, P95, P99  time.Duration
	StatusCodes    map[int]int64
}

// Run starts Clients goroutines that play the stream described by cfg and
// returns once the run is over, either because cfg.Duration elapsed or because
// ctx was cancelled. Requests are counted individually, including the playlist
// requests; Bytes only counts segment payload, the media a device actually
// plays.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	run, err := prepare(cfg)
	if err != nil {
		return Report{}, err
	}

	// The whole run is bounded by Duration; every request inherits this context
	// so nothing survives the end of the run.
	runCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	st := newStats()
	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(cfg.Clients)
	for i := 0; i < cfg.Clients; i++ {
		c := &client{
			http:    run.client,
			master:  run.master,
			token:   cfg.Token,
			variant: cfg.Variant,
			timeout: cfg.Timeout,
		}
		go func(index int) {
			defer wg.Done()
			// Ramp spreads the start of the clients over Ramp so the server sees
			// a climbing load instead of a single spike.
			if !sleepContext(runCtx, rampDelay(index, cfg.Clients, cfg.Ramp)) {
				return
			}
			c.session(runCtx, st)
		}(i)
	}
	wg.Wait()

	elapsed := time.Since(start)
	if run.ownClient {
		// Idle keep-alive connections of the client we created are dropped so a
		// long-lived process does not keep them around after the run.
		run.client.CloseIdleConnections()
	}
	return st.report(cfg.Clients, elapsed), nil
}

// runConfig holds the validated configuration with the defaults applied.
type runConfig struct {
	client    *http.Client
	master    *url.URL
	ownClient bool
}

// prepare validates cfg and resolves the master playlist URL and HTTP client.
func prepare(cfg Config) (runConfig, error) {
	if cfg.Clients < 1 {
		return runConfig{}, fmt.Errorf("loadgen: Clients must be at least 1, got %d", cfg.Clients)
	}
	if cfg.Duration <= 0 {
		return runConfig{}, fmt.Errorf("loadgen: Duration must be positive, got %s", cfg.Duration)
	}
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return runConfig{}, err
	}
	master, err := masterURL(base, cfg)
	if err != nil {
		return runConfig{}, err
	}

	run := runConfig{master: master, client: cfg.Client}
	if run.client == nil {
		run.client = newHTTPClient(cfg)
		run.ownClient = true
	}
	return run, nil
}

// parseBaseURL validates the base URL of the server under test.
func parseBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("loadgen: BaseURL is empty")
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("loadgen: invalid BaseURL %q: %w", raw, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("loadgen: BaseURL %q must use http or https", raw)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("loadgen: BaseURL %q has no host", raw)
	}
	return base, nil
}

// masterURL turns MasterPath (or PlayableID) into the absolute URL of the
// master playlist.
func masterURL(base *url.URL, cfg Config) (*url.URL, error) {
	ref := strings.TrimSpace(cfg.MasterPath)
	if ref == "" {
		ref = fmt.Sprintf("/api/v1/playables/%d/master.m3u8", cfg.PlayableID)
	}
	master, err := resolveURL(base, ref)
	if err != nil {
		return nil, fmt.Errorf("loadgen: invalid master playlist path %q: %w", ref, err)
	}
	return master, nil
}

// resolveURL resolves ref against base. Absolute refs are returned unchanged so
// a playlist may point at another host (a CDN, for instance).
func resolveURL(base *url.URL, ref string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, err
	}
	if parsed.IsAbs() {
		return parsed, nil
	}
	if base == nil {
		return nil, errors.New("no base URL")
	}
	return base.ResolveReference(parsed), nil
}

// newHTTPClient builds the client used when Config.Client is nil. The idle pool
// is sized for Clients so many sessions can keep their connections open at the
// same time; MaxIdleConnsPerHost defaults to 2, which would make 100 clients
// reopen sockets for every segment.
func newHTTPClient(cfg Config) *http.Client {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.MaxIdleConns = 0 // no global cap
	transport.MaxIdleConnsPerHost = idleConnsPerHost(cfg.Clients)
	transport.MaxConnsPerHost = 0
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: transport}
}

// idleConnsPerHost returns the idle connection budget for clients sessions.
func idleConnsPerHost(clients int) int {
	idle := clients * 2
	if idle < 16 {
		idle = 16
	}
	if idle > 512 {
		idle = 512
	}
	return idle
}

// rampDelay returns how long the client at index waits before it starts. With
// 100 clients over 10s every client is 100ms apart.
func rampDelay(index, clients int, ramp time.Duration) time.Duration {
	if ramp <= 0 || clients <= 1 || index <= 0 {
		return 0
	}
	if index >= clients {
		index = clients - 1
	}
	return time.Duration(int64(ramp) * int64(index) / int64(clients))
}

// sleepContext waits for d or until ctx is done, reporting whether it woke up
// on its own.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// client is one simulated player.
type client struct {
	http    *http.Client
	master  *url.URL
	token   string
	variant string
	timeout time.Duration
}

// session plays the stream until ctx is done: master playlist, variant
// playlist, then the segments in order, starting over at the end of the
// playlist because a player keeps pulling media for as long as it is open.
func (c *client) session(ctx context.Context, st *stats) {
	master, ok := c.loadPlaylist(ctx, st, c.master)
	if !ok {
		return
	}

	variant, ok := pickVariant(master, c.variant)
	if !ok {
		st.fail(ctx)
		return
	}
	variantURL, err := resolveURL(c.master, variant.URI)
	if err != nil {
		st.fail(ctx)
		return
	}

	media, ok := c.loadPlaylist(ctx, st, variantURL)
	if !ok {
		return
	}
	if len(media.Segments) == 0 {
		st.fail(ctx)
		return
	}

	// The segment URIs are resolved once: the playlist does not change during
	// the run, and resolving on every loop would only add noise to the hot path.
	segments := make([]*url.URL, 0, len(media.Segments))
	for _, segment := range media.Segments {
		segmentURL, err := resolveURL(variantURL, segment.URI)
		if err != nil {
			st.fail(ctx)
			return
		}
		segments = append(segments, segmentURL)
	}

	for ctx.Err() == nil {
		for _, segmentURL := range segments {
			if ctx.Err() != nil {
				return
			}
			c.record(ctx, st, c.fetch(ctx, segmentURL, false), true)
		}
	}
}

// loadPlaylist downloads and parses one playlist, recording the request. It
// reports whether the playlist is usable; a failure only ends this client.
func (c *client) loadPlaylist(ctx context.Context, st *stats, target *url.URL) (*hls.Playlist, bool) {
	resp := c.fetch(ctx, target, true)
	c.record(ctx, st, resp, false)
	if resp.err != nil {
		return nil, false
	}
	playlist, err := hls.Parse(bytes.NewReader(resp.body))
	if err != nil {
		st.fail(ctx)
		return nil, false
	}
	return playlist, true
}

// record folds one response into the shared statistics. Samples that were
// aborted because the run is over are dropped: they say nothing about the
// server and would only distort the percentiles.
func (c *client) record(ctx context.Context, st *stats, resp response, countBytes bool) {
	if ctx.Err() != nil {
		return
	}
	st.add(resp, countBytes)
}

// response is the outcome of a single GET.
type response struct {
	status  int
	bytes   int64
	latency time.Duration
	body    []byte
	err     error
}

// fetch performs one GET. With wantBody the response body is kept for the
// playlist parser, otherwise it is streamed into io.Discard so a large segment
// never has to fit in memory while still being counted and timed end to end.
func (c *client) fetch(ctx context.Context, target *url.URL, wantBody bool) response {
	reqCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return response{err: fmt.Errorf("loadgen: build request for %s: %w", target.Redacted(), err)}
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	sink := io.Writer(io.Discard)
	var buf *bytes.Buffer
	if wantBody {
		buf = new(bytes.Buffer)
		sink = buf
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return response{latency: time.Since(start), err: fmt.Errorf("loadgen: GET %s: %w", target.Redacted(), err)}
	}
	defer resp.Body.Close()

	written, copyErr := io.Copy(sink, resp.Body)
	out := response{status: resp.StatusCode, bytes: written, latency: time.Since(start)}
	if buf != nil {
		out.body = buf.Bytes()
	}
	if copyErr != nil {
		out.err = fmt.Errorf("loadgen: read body of %s: %w", target.Redacted(), copyErr)
		return out
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.err = fmt.Errorf("loadgen: GET %s: unexpected status %s", target.Redacted(), resp.Status)
	}
	return out
}

// pickVariant chooses the rendition to play: the first variant whose URI
// contains want, or the first variant when want is empty.
func pickVariant(master *hls.Playlist, want string) (hls.Variant, bool) {
	if master == nil || len(master.Variants) == 0 {
		return hls.Variant{}, false
	}
	if want == "" {
		return master.Variants[0], true
	}
	for _, variant := range master.Variants {
		if strings.Contains(variant.URI, want) {
			return variant, true
		}
	}
	return hls.Variant{}, false
}

// stats accumulates the counters and the latency samples. The mutex is the only
// shared state between the clients.
type stats struct {
	mu          sync.Mutex
	requests    int64
	errors      int64
	bytes       int64
	latencies   []time.Duration
	statusCodes map[int]int64
}

// newStats returns an empty stats value.
func newStats() *stats {
	return &stats{statusCodes: make(map[int]int64, 8)}
}

// add records one request outcome.
func (s *stats) add(resp response, countBytes bool) {
	s.mu.Lock()
	s.requests++
	if resp.latency > 0 {
		s.latencies = append(s.latencies, resp.latency)
	}
	if resp.status != 0 {
		s.statusCodes[resp.status]++
	}
	if countBytes {
		s.bytes += resp.bytes
	}
	if resp.err != nil {
		s.errors++
	}
	s.mu.Unlock()
}

// fail counts one error that happened outside a request, such as a playlist
// that could not be parsed. Errors that only exist because the run is over are
// ignored.
func (s *stats) fail(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.errors++
	s.mu.Unlock()
}

// report snapshots the statistics into a Report.
func (s *stats) report(clients int, elapsed time.Duration) Report {
	report := Report{Clients: clients, Duration: elapsed}

	s.mu.Lock()
	report.Requests = s.requests
	report.Errors = s.errors
	report.Bytes = s.bytes
	report.StatusCodes = make(map[int]int64, len(s.statusCodes))
	for code, count := range s.statusCodes {
		report.StatusCodes[code] = count
	}
	latencies := append([]time.Duration(nil), s.latencies...)
	s.mu.Unlock()

	if elapsed > 0 {
		report.ThroughputMbps = float64(report.Bytes) * 8 / elapsed.Seconds() / 1e6
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	report.P50 = percentile(latencies, 0.50)
	report.P95 = percentile(latencies, 0.95)
	report.P99 = percentile(latencies, 0.99)
	return report
}

// percentile returns the nearest-rank percentile p of the sorted samples: the
// smallest sample such that at least p of the samples are not larger.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
