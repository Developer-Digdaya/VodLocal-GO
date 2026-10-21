package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testBody is the deterministic payload the test server serves for a path.
func testBody(path string) string {
	return "content for " + path + "\n"
}

// testJobs builds len(paths) jobs pointing at srvURL, writing into dir.
func testJobs(srvURL, dir string, count int, withSize bool) []Job {
	jobs := make([]Job, 0, count)
	for i := range count {
		path := fmt.Sprintf("/file/%d.bin", i)
		job := Job{
			URL:  srvURL + path,
			Dest: filepath.Join(dir, fmt.Sprintf("file-%d.bin", i)),
		}
		if withSize {
			job.Size = int64(len(testBody(path)))
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// readFile returns the contents of path or fails the test.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(data)
}

// assertNoFilesWithSuffix fails when dir contains a file with the given suffix.
func assertNoFilesWithSuffix(t *testing.T, dir, suffix string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), suffix) {
			t.Errorf("leftover %q in %q", entry.Name(), dir)
		}
	}
}

// assertResult fails when the result does not match the expectation.
func assertResult(t *testing.T, res Result, total, done, skipped, failed int) {
	t.Helper()
	if res.Total != total || res.Done != done || res.Skipped != skipped || res.Failed != failed {
		t.Fatalf("Result = {Total:%d Done:%d Skipped:%d Failed:%d}, want {Total:%d Done:%d Skipped:%d Failed:%d}",
			res.Total, res.Done, res.Skipped, res.Failed, total, done, skipped, failed)
	}
	if len(res.Errors) != failed {
		t.Fatalf("len(Result.Errors) = %d, want %d", len(res.Errors), failed)
	}
}

func TestDownloadAll(t *testing.T) {
	const count = 10

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, count, true)

	var wantBytes int64
	for _, job := range jobs {
		wantBytes += job.Size
	}

	d := New(Options{MaxParallel: 4, Retries: 1, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	assertResult(t, res, count, count, 0, 0)
	if res.Bytes != wantBytes {
		t.Errorf("Result.Bytes = %d, want %d", res.Bytes, wantBytes)
	}

	for _, job := range jobs {
		if got, want := readFile(t, job.Dest), testBody(strings.TrimPrefix(job.URL, srv.URL)); got != want {
			t.Errorf("%q content = %q, want %q", job.Dest, got, want)
		}
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadSkipsExisting(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	t.Run("with size", func(t *testing.T) {
		dir := t.TempDir()
		jobs := testJobs(srv.URL, dir, 1, true)
		if err := os.WriteFile(jobs[0].Dest, []byte(testBody("/file/0.bin")), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		before := hits.Load()

		d := New(Options{MaxParallel: 2})
		res, err := d.Download(context.Background(), jobs)
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		assertResult(t, res, 1, 0, 1, 0)
		if res.Bytes != 0 {
			t.Errorf("Result.Bytes = %d, want 0", res.Bytes)
		}
		if got := hits.Load() - before; got != 0 {
			t.Errorf("server received %d requests, want 0", got)
		}
		if got := readFile(t, jobs[0].Dest); got != testBody("/file/0.bin") {
			t.Errorf("skipped file content = %q", got)
		}
	})

	t.Run("without size", func(t *testing.T) {
		dir := t.TempDir()
		jobs := testJobs(srv.URL, dir, 1, false)
		if err := os.WriteFile(jobs[0].Dest, []byte("already here"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		before := hits.Load()

		d := New(Options{MaxParallel: 2})
		res, err := d.Download(context.Background(), jobs)
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		assertResult(t, res, 1, 0, 1, 0)
		if got := hits.Load() - before; got != 0 {
			t.Errorf("server received %d requests, want 0", got)
		}
	})

	t.Run("empty file is downloaded", func(t *testing.T) {
		dir := t.TempDir()
		jobs := testJobs(srv.URL, dir, 1, true)
		if err := os.WriteFile(jobs[0].Dest, nil, 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		d := New(Options{MaxParallel: 2})
		res, err := d.Download(context.Background(), jobs)
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		assertResult(t, res, 1, 1, 0, 0)
		if got, want := readFile(t, jobs[0].Dest), testBody("/file/0.bin"); got != want {
			t.Errorf("content = %q, want %q", got, want)
		}
	})
}

func TestDownloadRetriesTransientErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 1, true)

	d := New(Options{MaxParallel: 2, Retries: 3, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v, want nil", err)
	}
	assertResult(t, res, 1, 1, 0, 0)
	if got := hits.Load(); got != 3 {
		t.Errorf("server received %d requests, want 3 (two 500s then a 200)", got)
	}
	if got, want := readFile(t, jobs[0].Dest), testBody("/file/0.bin"); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if res.Bytes != int64(len(testBody("/file/0.bin"))) {
		t.Errorf("Result.Bytes = %d, want %d", res.Bytes, len(testBody("/file/0.bin")))
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadDoesNotRetryClientError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 1, true)

	d := New(Options{MaxParallel: 2, Retries: 5, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err == nil {
		t.Fatal("Download() error = nil, want failure")
	}
	assertResult(t, res, 1, 0, 0, 1)
	if got := hits.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (404 is not retried)", got)
	}
	if _, statErr := os.Stat(jobs[0].Dest); !os.IsNotExist(statErr) {
		t.Errorf("destination exists after failure: stat error = %v", statErr)
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)

	// The joined error must expose the underlying job failure.
	if !strings.Contains(err.Error(), jobs[0].URL) {
		t.Errorf("error = %v, want it to mention %q", err, jobs[0].URL)
	}
	if !strings.Contains(res.Errors[0].Err.Error(), "404") {
		t.Errorf("JobError.Err = %v, want it to mention the status code", res.Errors[0].Err)
	}
}

func TestDownloadContextCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		// Block until the client gives up, which cancels the request.
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 4, true)
	for i := range jobs {
		// The server never sends the promised bytes, so the size check fails
		// too; either way the job must not be reported as done.
		jobs[i].Size = 1 << 20
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)

	d := New(Options{MaxParallel: 2, Timeout: 10 * time.Second})
	go func() {
		res, err := d.Download(ctx, jobs)
		done <- outcome{res: res, err: err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received a request")
	}

	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("Download() error = nil, want a context failure")
		}
		if got.res.Failed == 0 {
			t.Errorf("Result.Failed = 0, want > 0")
		}
		if got.res.Total != len(jobs) || got.res.Done != 0 {
			t.Errorf("Result = {Total:%d Done:%d}, want {Total:%d Done:0}", got.res.Total, got.res.Done, len(jobs))
		}
		if got.res.Failed+got.res.Skipped != got.res.Total {
			t.Errorf("Result = %+v, want every job accounted for", got.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download() did not return after the context was cancelled")
	}

	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadRespectsMaxParallel(t *testing.T) {
	const (
		parallel = 3
		count    = 20
	)

	var current, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := current.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer current.Add(-1)
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, count, true)

	d := New(Options{MaxParallel: parallel, Retries: 1, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	assertResult(t, res, count, count, 0, 0)

	if got := peak.Load(); got > parallel {
		t.Errorf("peak concurrency = %d, want <= %d", got, parallel)
	} else if got < 2 {
		t.Errorf("peak concurrency = %d, want downloads to run in parallel", got)
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadResumesPartialFile(t *testing.T) {
	const path = "/file/0.bin"
	body := testBody(path)
	half := len(body) / 2

	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()

		rest, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes=")
		if !ok || !strings.HasSuffix(rest, "-") {
			io.WriteString(w, body)
			return
		}
		start, err := strconv.Atoi(strings.TrimSuffix(rest, "-"))
		if err != nil || start < 0 || start > len(body) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, body[start:])
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 1, true)
	if err := os.WriteFile(jobs[0].Dest+PartSuffix, []byte(body[:half]), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	d := New(Options{MaxParallel: 1, Retries: 1, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	assertResult(t, res, 1, 1, 0, 0)

	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	if len(gotRanges) != 1 || gotRanges[0] != fmt.Sprintf("bytes=%d-", half) {
		t.Errorf("ranges = %q, want one request resuming at %d", gotRanges, half)
	}
	if got := readFile(t, jobs[0].Dest); got != body {
		t.Errorf("content = %q, want %q", got, body)
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadPromotesCompletePart(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 1, true)
	body := testBody("/file/0.bin")
	if err := os.WriteFile(jobs[0].Dest+PartSuffix, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// A part file that already holds the whole resource is promoted without a
	// request.
	d := New(Options{MaxParallel: 1})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	assertResult(t, res, 1, 1, 0, 0)
	if got := hits.Load(); got != 0 {
		t.Errorf("server received %d requests, want 0", got)
	}
	if got := readFile(t, jobs[0].Dest); got != body {
		t.Errorf("content = %q, want %q", got, body)
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
}

func TestDownloadSizeMismatch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 1, true)
	want := jobs[0].Size
	jobs[0].Size = want + 10

	d := New(Options{MaxParallel: 2, Retries: 2, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err == nil {
		t.Fatal("Download() error = nil, want a size mismatch failure")
	}
	assertResult(t, res, 1, 0, 0, 1)
	if res.Bytes != 0 {
		t.Errorf("Result.Bytes = %d, want 0 for a failed job", res.Bytes)
	}
	if _, statErr := os.Stat(jobs[0].Dest); !os.IsNotExist(statErr) {
		t.Errorf("destination exists after a size mismatch: stat error = %v", statErr)
	}
	assertNoFilesWithSuffix(t, dir, PartSuffix)
	if got := hits.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (a size mismatch is permanent)", got)
	}
}

func TestNewNormalizesOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "zero", opts: Options{}},
		{name: "negative", opts: Options{MaxParallel: -4, Retries: -9, Backoff: -time.Second, Timeout: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(tt.opts)
			if d.maxParallel != DefaultMaxParallel {
				t.Errorf("maxParallel = %d, want %d", d.maxParallel, DefaultMaxParallel)
			}
			if d.backoff != DefaultBackoff {
				t.Errorf("backoff = %v, want %v", d.backoff, DefaultBackoff)
			}
			if d.timeout != DefaultTimeout {
				t.Errorf("timeout = %v, want %v", d.timeout, DefaultTimeout)
			}
			if d.client == nil {
				t.Error("client = nil, want a default client")
			}
		})
	}

	// A negative retry count falls back to the default, while zero disables
	// retries explicitly.
	if d := New(Options{Retries: -1}); d.retries != DefaultRetries {
		t.Errorf("retries = %d, want %d", d.retries, DefaultRetries)
	}
	if d := New(Options{Retries: 0}); d.retries != 0 {
		t.Errorf("retries = %d, want 0", d.retries)
	}
}

func TestNewTunesHTTPConnectionPool(t *testing.T) {
	t.Parallel()

	d := New(Options{MaxParallel: 16})
	tr, ok := d.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, mau *http.Transport", d.client.Transport)
	}
	if tr.MaxIdleConnsPerHost < 16 {
		t.Errorf("MaxIdleConnsPerHost = %d, mau >= 16", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d, mau >= MaxIdleConnsPerHost (%d)", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d, mau 0", tr.MaxConnsPerHost)
	}
}

func TestNewKeepsCustomClient(t *testing.T) {
	t.Parallel()

	custom := &http.Client{}
	if d := New(Options{Client: custom}); d.client != custom {
		t.Error("New mengganti client yang diberikan pemanggil")
	}
}

func TestDownloadNormalizedOptionsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testBody(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	jobs := testJobs(srv.URL, dir, 2, true)

	// Zero MaxParallel and a negative retry count must not panic.
	d := New(Options{MaxParallel: 0, Retries: -1, Backoff: time.Millisecond})
	res, err := d.Download(context.Background(), jobs)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	assertResult(t, res, 2, 2, 0, 0)
}

func TestDownloadEmptyJobs(t *testing.T) {
	d := New(Options{})
	res, err := d.Download(context.Background(), nil)
	if err != nil {
		t.Fatalf("Download(nil) error = %v", err)
	}
	if res.Total != 0 || res.Done != 0 || res.Skipped != 0 || res.Failed != 0 || res.Bytes != 0 || len(res.Errors) != 0 {
		t.Errorf("Result = %+v, want the zero value", res)
	}
}
