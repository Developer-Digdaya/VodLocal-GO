// Package download implements a small parallel file downloader with retries,
// resume and size verification.
//
// A Downloader runs a fixed pool of workers, each of which fetches one job at a
// time. Data is written to "<Dest>.part" and only renamed onto Dest once the
// transfer succeeded and, when the expected size is known, has been verified.
// A failed job never leaves a partial file behind.
package download

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Defaults applied by New when the corresponding Options field is unset.
const (
	// DefaultMaxParallel is the number of concurrent downloads used when
	// Options.MaxParallel is less than one.
	DefaultMaxParallel = 8
	// DefaultRetries is the number of retries used when Options.Retries is
	// negative.
	DefaultRetries = 3
	// DefaultBackoff is the delay before the first retry when Options.Backoff
	// is not positive.
	DefaultBackoff = 500 * time.Millisecond
	// DefaultTimeout bounds a single HTTP request when Options.Timeout is not
	// positive.
	DefaultTimeout = 60 * time.Second
)

// PartSuffix is appended to Job.Dest while a file is being downloaded. The
// partial file is renamed onto Job.Dest only after a successful transfer.
const PartSuffix = ".part"

// Job is a single file to download.
type Job struct {
	// URL is the address of the resource.
	URL string
	// Dest is the absolute path of the target file.
	Dest string
	// Size is the expected size in bytes, or 0 when it is unknown.
	Size int64
}

// Options configures a Downloader. The zero value is valid and selects the
// documented defaults.
type Options struct {
	// MaxParallel is the maximum number of concurrent downloads; values below
	// one select DefaultMaxParallel.
	MaxParallel int
	// Retries is the number of extra attempts after the first failed one, so a
	// job is tried at most Retries+1 times. Zero disables retries and a negative
	// value selects DefaultRetries.
	Retries int
	// Backoff is the delay before the first retry. Every further retry doubles
	// it. Values that are not positive select DefaultBackoff.
	Backoff time.Duration
	// Timeout bounds a single HTTP request, including the body. Values that are
	// not positive select DefaultTimeout.
	Timeout time.Duration
	// Client is the HTTP client to use; nil selects a fresh http.Client.
	Client *http.Client
}

// JobError describes why a single Job failed.
type JobError struct {
	Job Job
	Err error
}

// Error implements the error interface.
func (e JobError) Error() string {
	return fmt.Sprintf("download %s -> %s: %v", e.Job.URL, e.Job.Dest, e.Err)
}

// Unwrap returns the underlying failure so errors.Is and errors.As keep
// working on the joined error returned by Download.
func (e JobError) Unwrap() error { return e.Err }

// Result summarises a Download call. Every job ends up in exactly one of Done,
// Skipped or Failed, so Total == Done+Skipped+Failed.
type Result struct {
	// Total is the number of jobs the call was given.
	Total int
	// Done is the number of jobs that were downloaded and moved into place.
	Done int
	// Skipped is the number of jobs whose destination already existed.
	Skipped int
	// Failed is the number of jobs that could not be downloaded.
	Failed int
	// Bytes is the number of bytes accounted for by the downloads that ended up
	// in Done; skipped files do not contribute.
	Bytes int64
	// Errors holds one entry per failed job, sorted by destination and URL.
	Errors []JobError
}

// Downloader downloads a set of files with a bounded amount of concurrency. It
// is safe for concurrent use as long as the underlying http.Client is.
type Downloader struct {
	maxParallel int
	retries     int
	backoff     time.Duration
	timeout     time.Duration
	client      *http.Client
}

// New returns a Downloader configured by opts. Out-of-range values are replaced
// by the defaults, so New never panics.
func New(opts Options) *Downloader {
	d := &Downloader{
		maxParallel: opts.MaxParallel,
		retries:     opts.Retries,
		backoff:     opts.Backoff,
		timeout:     opts.Timeout,
		client:      opts.Client,
	}
	if d.maxParallel < 1 {
		d.maxParallel = DefaultMaxParallel
	}
	if d.retries < 0 {
		d.retries = DefaultRetries
	}
	if d.backoff <= 0 {
		d.backoff = DefaultBackoff
	}
	if d.timeout <= 0 {
		d.timeout = DefaultTimeout
	}
	if d.client == nil {
		d.client = &http.Client{}
	}
	return d
}

// outcome is what a worker reports back for one job.
type outcome struct {
	job     Job
	skipped bool
	n       int64
	err     error
}

// Download fetches every job using at most MaxParallel workers and returns a
// Result that is always fully populated. The returned error is nil when no job
// failed; otherwise it is a joined error holding one JobError per failure,
// which is also available in Result.Errors.
//
// Cancelling ctx stops the running transfers as quickly as possible: in-flight
// requests are aborted and their partial files are removed. Jobs that were not
// started yet fail immediately with the context error, so Download always
// reports on every job it was given.
func (d *Downloader) Download(ctx context.Context, jobs []Job) (Result, error) {
	result := Result{Total: len(jobs)}
	if len(jobs) == 0 {
		return result, nil
	}

	workers := min(d.maxParallel, len(jobs))

	jobCh := make(chan Job)
	// The channel is buffered with one slot per job so workers never block on
	// reporting, which keeps wg.Wait from deadlocking.
	outCh := make(chan outcome, len(jobs))

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for job := range jobCh {
				n, skipped, err := d.fetch(ctx, job)
				outCh <- outcome{job: job, n: n, skipped: skipped, err: err}
			}
		}()
	}

	// Every job is handed to the pool and collected, even when the context is
	// already done: workers keep draining jobCh because fetch fails fast.
	for _, job := range jobs {
		jobCh <- job
	}
	close(jobCh)
	wg.Wait()
	close(outCh)

	for out := range outCh {
		switch {
		case out.skipped:
			result.Skipped++
		case out.err != nil:
			result.Failed++
			result.Errors = append(result.Errors, JobError{Job: out.job, Err: out.err})
		default:
			result.Done++
			result.Bytes += out.n
		}
	}

	if len(result.Errors) == 0 {
		return result, nil
	}

	// Results arrive in completion order; sort them so callers see a stable
	// report that does not depend on scheduling.
	slices.SortFunc(result.Errors, func(a, b JobError) int {
		if c := cmp.Compare(a.Job.Dest, b.Job.Dest); c != 0 {
			return c
		}
		return cmp.Compare(a.Job.URL, b.Job.URL)
	})

	errs := make([]error, len(result.Errors))
	for i, jobErr := range result.Errors {
		errs[i] = jobErr
	}
	return result, errors.Join(errs...)
}

// fetch downloads a single job. It reports the number of bytes written, whether
// the job was skipped, and the failure, if any.
func (d *Downloader) fetch(ctx context.Context, job Job) (int64, bool, error) {
	if job.URL == "" {
		return 0, false, &permanentError{err: errors.New("download: job has no URL")}
	}
	if job.Dest == "" {
		return 0, false, &permanentError{err: errors.New("download: job has no destination")}
	}

	// An existing, non-empty destination is kept as is, provided it matches the
	// expected size when the caller knows it.
	if info, err := os.Stat(job.Dest); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		if job.Size <= 0 || info.Size() == job.Size {
			return 0, true, nil
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, false, err
	}

	if err := os.MkdirAll(filepath.Dir(job.Dest), 0o755); err != nil {
		return 0, false, &permanentError{err: err}
	}

	part := job.Dest + PartSuffix

	// Continue from whatever an interrupted earlier run left behind. Resuming is
	// only attempted when the expected size is known: that is the only way to
	// tell a complete file from a corrupt one.
	have := int64(0)
	if job.Size > 0 {
		if info, err := os.Stat(part); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			have = info.Size()
		}
	}
	if have > job.Size {
		// Leftover from a different file; it cannot be resumed.
		if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, false, &permanentError{err: err}
		}
		have = 0
	}

	// A previous run may have written the whole file before it was interrupted,
	// in which case there is nothing left to fetch.
	if job.Size > 0 && have == job.Size {
		return have, false, commit(part, job.Dest)
	}

	var lastErr error
	for attempt := 0; attempt <= d.retries; attempt++ {
		if attempt > 0 {
			if err := d.waitBackoff(ctx, attempt); err != nil {
				lastErr = err
				break
			}
		}
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		// A failed attempt that nevertheless completed the file is a success.
		if job.Size > 0 && have == job.Size {
			lastErr = nil
			break
		}

		total, err := d.transfer(ctx, job, part, have)
		have = total
		if err == nil && job.Size > 0 && have != job.Size {
			err = &permanentError{err: fmt.Errorf("download: size mismatch for %s: expected %d bytes, got %d", job.Dest, job.Size, have)}
		}
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err

		// A cancelled or expired call has nothing left to retry, and permanent
		// failures (bad request, 4xx, size mismatch) do not get better.
		if ctx.Err() != nil || !retryable(err) {
			break
		}
	}

	if lastErr != nil {
		// Never leave a partial file behind on failure.
		_ = os.Remove(part)
		return 0, false, lastErr
	}

	if err := commit(part, job.Dest); err != nil {
		return 0, false, err
	}
	return have, false, nil
}

// commit moves the finished part file onto its destination. On failure the part
// file is removed so a later run starts from a clean slate.
func commit(part, dest string) error {
	if err := os.Rename(part, dest); err != nil {
		_ = os.Remove(part)
		return &permanentError{err: err}
	}
	return nil
}

// transfer performs one HTTP request and appends the response body to part. It
// returns the number of bytes part holds afterwards, which is the resume offset
// for the next attempt. When the server ignores a resume request and answers
// with the whole resource, part is rewritten from the start.
func (d *Downloader) transfer(ctx context.Context, job Job, part string, have int64) (int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, job.URL, nil)
	if err != nil {
		return have, &permanentError{err: err}
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return have, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server sent the whole resource; start over.
		have = 0
	case http.StatusPartialContent:
		// Appending the remainder to what is already on disk.
	default:
		return have, &statusError{StatusCode: resp.StatusCode, URL: job.URL}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if have > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return have, &permanentError{err: err}
	}

	n, copyErr := io.Copy(f, resp.Body)
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	return have + n, copyErr
}

// waitBackoff sleeps before retry number attempt (1-based), doubling the base
// delay for every further retry. It returns the context error when ctx is done.
func (d *Downloader) waitBackoff(ctx context.Context, attempt int) error {
	delay := d.backoff
	for i := 1; i < attempt; i++ {
		if delay > (1<<62)/2 {
			// Saturate instead of overflowing into a negative duration.
			delay = 1 << 62
			break
		}
		delay *= 2
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryable reports whether a failed attempt is worth repeating. Only
// transient conditions qualify: 5xx, 429 and transport-level errors such as
// timeouts and connection resets. Other 4xx responses and permanent failures
// such as a size mismatch fail the job immediately.
func retryable(err error) bool {
	var status *statusError
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= 500
	}
	var permanent *permanentError
	return !errors.As(err, &permanent)
}

// statusError reports an unexpected HTTP status code.
type statusError struct {
	StatusCode int
	URL        string
}

// Error implements the error interface.
func (e *statusError) Error() string {
	return fmt.Sprintf("download: unexpected HTTP status %d %s for %s",
		e.StatusCode, http.StatusText(e.StatusCode), e.URL)
}

// permanentError marks a failure that will not be fixed by trying again.
type permanentError struct {
	err error
}

// Error implements the error interface.
func (e *permanentError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped failure.
func (e *permanentError) Unwrap() error { return e.err }
