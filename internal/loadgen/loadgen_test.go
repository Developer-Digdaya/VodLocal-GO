package loadgen

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vodlocal/internal/hls"
)

// masterPlaylist is the master playlist served by the fixture. It carries two
// variants so TestVariantSelection has something to choose between.
const masterPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720
/media/v0/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1920x1080
/media/v1/index.m3u8
`

// mediaPlaylist is the variant playlist served by the fixture. Its segment URIs
// are relative, so a client has to resolve them against the playlist URL.
const mediaPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:1
#EXTINF:4.000,
seg-00001.ts
#EXTINF:4.000,
seg-00002.ts
#EXTINF:4.000,
seg-00003.ts
#EXT-X-ENDLIST
`

// masterPath is the master playlist route the fixture serves.
const masterPath = "/api/v1/playables/7/master.m3u8"

// fixture is a fake origin: a master playlist, two variant playlists and three
// segments per variant. It also records how many requests each path received
// and how many were in flight at the same time.
type fixture struct {
	mu          sync.Mutex
	paths       map[string]int
	total       int
	inFlight    int
	maxInFlight int

	// Configured before the test server starts, read under the lock.
	status       int
	segmentDelay time.Duration
	segmentSize  int
	requireToken string
	cancelAfter  int
	cancel       func()
}

// newFixture returns a fixture that answers normally and serves 32 KiB
// segments.
func newFixture() *fixture {
	return &fixture{paths: make(map[string]int), segmentSize: 32 << 10}
}

// ServeHTTP serves the fake stream and keeps the counters up to date.
func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.total++
	f.paths[r.URL.Path]++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	var (
		status  = f.status
		delay   = f.segmentDelay
		size    = f.segmentSize
		token   = f.requireToken
		trigger = f.cancelAfter > 0 && f.total >= f.cancelAfter
	)
	if trigger {
		// Cancel the run once the server has seen enough traffic. Doing it here
		// keeps the cancellation test deterministic without a sleep.
		f.cancelAfter = 0
	}
	f.mu.Unlock()
	defer f.done()

	if trigger && f.cancel != nil {
		f.cancel()
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}

	path := r.URL.Path
	switch {
	case path == masterPath:
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writePlaylist(w, masterPlaylist)
	case strings.HasPrefix(path, "/media/") && strings.HasSuffix(path, ".m3u8"):
		writePlaylist(w, mediaPlaylist)
	case strings.HasPrefix(path, "/media/") && strings.HasSuffix(path, ".ts"):
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(bytes.Repeat([]byte{'s'}, size))
	default:
		http.NotFound(w, r)
	}
}

// done records that a request finished.
func (f *fixture) done() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

// count returns how many requests the fixture served for path.
func (f *fixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[path]
}

// requests returns the total number of requests served.
func (f *fixture) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// maxConcurrency returns the highest number of simultaneous requests.
func (f *fixture) maxConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// waitIdle waits up to timeout for the fixture to have no request in flight and
// returns the last count it saw.
func (f *fixture) waitIdle(timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		inFlight := f.inFlight
		f.mu.Unlock()
		if inFlight == 0 || time.Now().After(deadline) {
			return inFlight
		}
		time.Sleep(time.Millisecond)
	}
}

// writePlaylist writes an HLS playlist body.
func writePlaylist(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, body)
}

// startFixture serves f and returns the server together with its base URL.
func startFixture(t *testing.T, f *fixture) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return server, server.URL
}

// TestRunServesStream is the happy path: five clients play for 200ms without a
// single error and move real bytes.
func TestRunServesStream(t *testing.T) {
	f := newFixture()
	_, base := startFixture(t, f)

	report, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    5,
		Duration:   200 * time.Millisecond,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if report.Requests == 0 {
		t.Fatal("Report.Requests = 0, want > 0")
	}
	if report.Errors != 0 {
		t.Errorf("Report.Errors = %d, want 0 (status codes: %v)", report.Errors, report.StatusCodes)
	}
	if report.Bytes == 0 {
		t.Fatal("Report.Bytes = 0, want > 0")
	}
	if report.Clients != 5 {
		t.Errorf("Report.Clients = %d, want 5", report.Clients)
	}
	if report.Duration <= 0 {
		t.Errorf("Report.Duration = %s, want > 0", report.Duration)
	}
	if report.ThroughputMbps <= 0 {
		t.Errorf("Report.ThroughputMbps = %v, want > 0", report.ThroughputMbps)
	}
	if report.P50 <= 0 || report.P99 < report.P50 {
		t.Errorf("latencies = p50 %s, p95 %s, p99 %s, want p99 >= p50 > 0",
			report.P50, report.P95, report.P99)
	}

	// The default master path built from PlayableID must be the one requested,
	// and every client has to walk master, variant and segments.
	if got := f.count(masterPath); got != 5 {
		t.Errorf("master requests = %d, want 5 (one per client)", got)
	}
	if got := f.count("/media/v0/index.m3u8"); got != 5 {
		t.Errorf("variant requests = %d, want 5", got)
	}
	if got := f.count("/media/v0/seg-00002.ts"); got == 0 {
		t.Error("middle segment was never requested")
	}
}

// TestRunCountsHTTPFailures makes sure a broken server produces errors in the
// report instead of an error return.
func TestRunCountsHTTPFailures(t *testing.T) {
	f := newFixture()
	f.status = http.StatusNotFound
	_, base := startFixture(t, f)

	report, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    2,
		Duration:   100 * time.Millisecond,
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a failing server is reported, not returned", err)
	}
	if report.Errors == 0 {
		t.Fatal("Report.Errors = 0, want > 0 when the master playlist returns 404")
	}
	if report.Requests == 0 {
		t.Error("Report.Requests = 0, want > 0 (failed requests are counted)")
	}
	if got := report.StatusCodes[http.StatusNotFound]; got == 0 {
		t.Errorf("Report.StatusCodes = %v, want a count for 404", report.StatusCodes)
	}
	if f.requests() == 0 {
		t.Error("the server received no request at all, want the master playlist to be requested")
	}
	if report.Bytes != 0 {
		t.Errorf("Report.Bytes = %d, want 0 when nothing but error pages were served", report.Bytes)
	}
}

// TestRunRejectsInvalidConfig checks the validation of the mandatory fields.
func TestRunRejectsInvalidConfig(t *testing.T) {
	valid := Config{BaseURL: "http://127.0.0.1:8080", PlayableID: 7, Clients: 1, Duration: time.Second}

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no clients", cfg: func() Config { c := valid; c.Clients = 0; return c }()},
		{name: "negative clients", cfg: func() Config { c := valid; c.Clients = -3; return c }()},
		{name: "no duration", cfg: func() Config { c := valid; c.Duration = 0; return c }()},
		{name: "negative duration", cfg: func() Config { c := valid; c.Duration = -time.Second; return c }()},
		{name: "empty base URL", cfg: func() Config { c := valid; c.BaseURL = ""; return c }()},
		{name: "base URL without scheme", cfg: func() Config { c := valid; c.BaseURL = "127.0.0.1:8080"; return c }()},
		{name: "unsupported scheme", cfg: func() Config { c := valid; c.BaseURL = "ftp://127.0.0.1:8080"; return c }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := Run(context.Background(), test.cfg)
			if err == nil {
				t.Fatalf("Run(%+v) error = nil, want an error", test.cfg)
			}
			if report.Requests != 0 || report.Errors != 0 || report.Bytes != 0 {
				t.Errorf("Report = %+v, want the zero value on a validation error", report)
			}
		})
	}

	// A config that is valid but points at a closed port has to be reported, not
	// returned: nothing listens on port 1 here.
	report, err := Run(context.Background(), Config{
		BaseURL:    "http://127.0.0.1:1",
		PlayableID: 7,
		Clients:    2,
		Duration:   50 * time.Millisecond,
		Timeout:    100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run(valid, unreachable) error = %v, want nil", err)
	}
	if report.Errors == 0 {
		t.Error("Report.Errors = 0, want > 0 when nothing listens on the port")
	}
}

// TestReportStatusCodes checks that successful responses are tallied by code.
func TestReportStatusCodes(t *testing.T) {
	f := newFixture()
	_, base := startFixture(t, f)

	report, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    3,
		Duration:   150 * time.Millisecond,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := report.StatusCodes[http.StatusOK]; got == 0 {
		t.Fatalf("Report.StatusCodes = %v, want a positive count for 200", report.StatusCodes)
	}
	var sum int64
	for _, count := range report.StatusCodes {
		sum += count
	}
	if sum != report.Requests {
		t.Errorf("sum of status code counts = %d, want Report.Requests = %d", sum, report.Requests)
	}
}

// TestRunStopsOnContextCancel cancels the run in the middle of the stream and
// expects Run to come back immediately with a filled report.
func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := newFixture()
	f.cancelAfter = 10
	f.cancel = cancel
	_, base := startFixture(t, f)

	start := time.Now()
	report, err := Run(ctx, Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    4,
		Duration:   30 * time.Second, // far longer than the test: cancel must end it
		Timeout:    2 * time.Second,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run() error = %v, want nil after a cancellation", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Run took %s after cancel, want < 2s", elapsed)
	}
	if report.Requests == 0 {
		t.Error("Report.Requests = 0, want the samples gathered before the cancellation")
	}
	if report.Clients != 4 {
		t.Errorf("Report.Clients = %d, want 4", report.Clients)
	}
	if report.Duration <= 0 || report.Duration >= 2*time.Second {
		t.Errorf("Report.Duration = %s, want the elapsed time of the cancelled run", report.Duration)
	}
	if report.StatusCodes[http.StatusOK] == 0 {
		t.Errorf("Report.StatusCodes = %v, want a count for 200", report.StatusCodes)
	}
	// Every client must be gone by the time Run returns, so the fixture has to
	// drain its in-flight requests instead of being left with a stuck handler.
	if inFlight := f.waitIdle(time.Second); inFlight != 0 {
		t.Errorf("%d requests still in flight after Run returned, want 0", inFlight)
	}
}

// TestRunRunsClientsConcurrently proves the clients really overlap: the fixture
// holds every segment request for a while, so the highest number of simultaneous
// requests has to be well above one.
func TestRunRunsClientsConcurrently(t *testing.T) {
	f := newFixture()
	f.segmentDelay = 20 * time.Millisecond
	_, base := startFixture(t, f)

	report, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    12,
		Duration:   500 * time.Millisecond,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := f.maxConcurrency(); got <= 1 {
		t.Fatalf("max concurrent requests = %d, want > 1: the clients did not run in parallel", got)
	}
	if report.Requests == 0 || report.Errors != 0 {
		t.Errorf("Report = {requests %d, errors %d}, want requests > 0 and errors 0",
			report.Requests, report.Errors)
	}
}

// TestVariantSelection checks that Config.Variant picks the matching rendition.
func TestVariantSelection(t *testing.T) {
	f := newFixture()
	_, base := startFixture(t, f)

	if _, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    2,
		Duration:   100 * time.Millisecond,
		Timeout:    2 * time.Second,
		Variant:    "v1",
	}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := f.count("/media/v1/index.m3u8"); got == 0 {
		t.Error("variant v1 was never requested, want it chosen by Config.Variant")
	}
	if got := f.count("/media/v0/index.m3u8"); got != 0 {
		t.Errorf("variant v0 requests = %d, want 0 when Variant selects v1", got)
	}
}

// TestBearerTokenForwarded checks that Config.Token reaches the API routes.
func TestBearerTokenForwarded(t *testing.T) {
	f := newFixture()
	f.requireToken = "device-secret"
	_, base := startFixture(t, f)

	report, err := Run(context.Background(), Config{
		BaseURL:    base,
		PlayableID: 7,
		Clients:    2,
		Duration:   100 * time.Millisecond,
		Timeout:    2 * time.Second,
		Token:      "device-secret",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if report.Errors != 0 {
		t.Errorf("Report.Errors = %d, want 0 with the right bearer token (codes: %v)",
			report.Errors, report.StatusCodes)
	}
	if got := f.count(masterPath); got == 0 {
		t.Error("master playlist was never requested")
	}
}

// TestPickVariant covers the variant selection rules directly.
func TestPickVariant(t *testing.T) {
	master, err := hls.Parse(strings.NewReader(masterPlaylist))
	if err != nil {
		t.Fatalf("hls.Parse(master) error = %v, want nil", err)
	}
	if len(master.Variants) != 2 {
		t.Fatalf("master variants = %d, want 2", len(master.Variants))
	}

	if _, ok := pickVariant(master, "v1"); !ok {
		t.Fatal(`pickVariant(master, "v1") = false, want true`)
	}
	if got, _ := pickVariant(master, "v1"); got.URI != "/media/v1/index.m3u8" {
		t.Errorf("pickVariant picked %q, want /media/v1/index.m3u8", got.URI)
	}
	if got, _ := pickVariant(master, ""); got.URI != "/media/v0/index.m3u8" {
		t.Errorf("pickVariant with an empty hint picked %q, want the first variant", got.URI)
	}
	if _, ok := pickVariant(master, "does-not-exist"); ok {
		t.Error("pickVariant with an unknown hint = true, want false")
	}
	if _, ok := pickVariant(nil, ""); ok {
		t.Error(`pickVariant(nil, "") = true, want false`)
	}

	// A media playlist has no variants at all.
	media, err := hls.Parse(strings.NewReader(mediaPlaylist))
	if err != nil {
		t.Fatalf("hls.Parse(media) error = %v, want nil", err)
	}
	if _, ok := pickVariant(media, ""); ok {
		t.Error("pickVariant(media playlist, \"\") = true, want false")
	}
}

// TestRampDelay checks the spacing of the client starts.
func TestRampDelay(t *testing.T) {
	tests := []struct {
		index, clients int
		ramp, want     time.Duration
	}{
		{index: 0, clients: 100, ramp: 10 * time.Second, want: 0},
		{index: 1, clients: 100, ramp: 10 * time.Second, want: 100 * time.Millisecond},
		{index: 50, clients: 100, ramp: 10 * time.Second, want: 5 * time.Second},
		{index: 99, clients: 100, ramp: 10 * time.Second, want: 9900 * time.Millisecond},
		{index: 3, clients: 4, ramp: 0, want: 0},
		{index: 3, clients: 1, ramp: 10 * time.Second, want: 0},
	}
	for _, test := range tests {
		if got := rampDelay(test.index, test.clients, test.ramp); got != test.want {
			t.Errorf("rampDelay(%d, %d, %s) = %s, want %s",
				test.index, test.clients, test.ramp, got, test.want)
		}
	}
}

// TestPercentile covers the nearest-rank throughput calculation.
func TestPercentile(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil, 0.5) = %s, want 0", got)
	}
	samples := []time.Duration{
		1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond,
		4 * time.Millisecond, 100 * time.Millisecond,
	}
	tests := []struct {
		p    float64
		want time.Duration
	}{
		{p: 0.50, want: 3 * time.Millisecond},
		{p: 0.95, want: 100 * time.Millisecond},
		{p: 0.99, want: 100 * time.Millisecond},
		{p: 0.0, want: 1 * time.Millisecond},
		{p: 1.0, want: 100 * time.Millisecond},
	}
	for _, test := range tests {
		if got := percentile(samples, test.p); got != test.want {
			t.Errorf("percentile(samples, %v) = %s, want %s", test.p, got, test.want)
		}
	}
	if got := percentile([]time.Duration{7 * time.Millisecond}, 0.99); got != 7*time.Millisecond {
		t.Errorf("percentile(single, 0.99) = %s, want 7ms", got)
	}
}
