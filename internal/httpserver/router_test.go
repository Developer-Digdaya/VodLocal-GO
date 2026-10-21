package httpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

// envelope adalah bentuk JSON standar yang dipakai sebagian besar endpoint.
type envelope struct {
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
	Data   any    `json:"data"`
	Errors []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

// testClock adalah jam tiruan yang bisa dimajukan.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testReadiness adalah Readiness tiruan.
type testReadiness struct {
	ready bool
	info  string
}

func (r testReadiness) Ready(ctx context.Context) (bool, string) { return r.ready, r.info }

// newTestRouter membangun router dengan logger yang tidak mengotori output test.
func newTestRouter(t *testing.T, cfg httpserver.Config) http.Handler {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return httpserver.NewRouter(cfg)
}

// doGet mengirim GET ke handler dan mengembalikan recorder.
func doGet(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// decodeEnvelope mendekode body JSON envelope standar.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var body envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body bukan envelope JSON (%q): %v", rec.Body.String(), err)
	}
	return body
}

// cookieNamed mengembalikan cookie dengan nama tertentu atau gagal.
func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q tidak ditemukan di %v", name, cookies)
	return nil
}

func TestHealth(t *testing.T) {
	clock := newTestClock(time.Date(2026, 10, 20, 4, 27, 10, 0, time.UTC))
	router := newTestRouter(t, httpserver.Config{Version: "0.2.0", Now: clock.Now})

	rec := doGet(t, router, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body bukan JSON (%q): %v", rec.Body.String(), err)
	}
	if got, want := body["status"], "ok"; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
	if got, want := body["version"], "0.2.0"; got != want {
		t.Errorf("version = %v, want %v", got, want)
	}
	if got, want := body["uptime_seconds"], float64(0); got != want {
		t.Errorf("uptime_seconds = %v, want %v", got, want)
	}
	if _, ok := body["code"]; ok {
		t.Error("/health tidak boleh memakai envelope (ada field code)")
	}
	if got := rec.Header().Get("X-Request-ID"); got == "" {
		t.Error("header X-Request-ID wajib ada")
	}

	clock.Advance(90 * time.Second)
	rec = doGet(t, router, "/health")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body bukan JSON (%q): %v", rec.Body.String(), err)
	}
	if got, want := body["uptime_seconds"], float64(90); got != want {
		t.Errorf("uptime_seconds = %v, want %v", got, want)
	}
}

func TestReady(t *testing.T) {
	t.Run("siap", func(t *testing.T) {
		router := newTestRouter(t, httpserver.Config{
			Ready: testReadiness{ready: true, info: "gen-000042"},
		})
		rec := doGet(t, router, "/ready")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body struct {
			Status     string `json:"status"`
			Generation string `json:"generation"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body bukan JSON (%q): %v", rec.Body.String(), err)
		}
		if body.Status != "ready" || body.Generation != "gen-000042" {
			t.Errorf("body = %+v, want status=ready generation=gen-000042", body)
		}
		if body.Reason != "" {
			t.Errorf("reason = %q, want kosong", body.Reason)
		}
	})

	t.Run("belum siap", func(t *testing.T) {
		router := newTestRouter(t, httpserver.Config{
			Ready: testReadiness{ready: false, info: "no_active_generation"},
		})
		rec := doGet(t, router, "/ready")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		var body struct {
			Status     string `json:"status"`
			Generation string `json:"generation"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body bukan JSON (%q): %v", rec.Body.String(), err)
		}
		if body.Status != "not_ready" || body.Reason != "no_active_generation" {
			t.Errorf("body = %+v, want status=not_ready reason=no_active_generation", body)
		}
		if body.Generation != "" {
			t.Errorf("generation = %q, want kosong", body.Generation)
		}
	})

	t.Run("tanpa readiness", func(t *testing.T) {
		rec := doGet(t, newTestRouter(t, httpserver.Config{}), "/ready")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})
}

func TestRouterMountsAPIUnderVersionPrefix(t *testing.T) {
	var gotPath, gotMethod string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})

	router := newTestRouter(t, httpserver.Config{API: api})
	rec := doGet(t, router, "/api/v1/ping")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/ping" {
		t.Errorf("path di handler API = %q, want %q", gotPath, "/ping")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if rec.Body.String() != "pong" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "pong")
	}
}

func TestRouterWithoutAPI(t *testing.T) {
	rec := doGet(t, newTestRouter(t, httpserver.Config{}), "/api/v1/ping")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != response.CodeNotFound {
		t.Errorf("code = %d, want %d", body.Code, response.CodeNotFound)
	}
}

func TestRouterMediaRoutes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "gen-000001", "seg-00001.ts"), "0123456789")
	writeFile(t, filepath.Join(root, "artwork", "poster.png"), "PNG")

	router := newTestRouter(t, httpserver.Config{Media: &testResolver{root: root}})

	rec := doGet(t, router, "/media/gen-000001/seg-00001.ts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status media = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Type"), "video/mp2t"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	rec = doGet(t, router, "/artwork/poster.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("status artwork = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=86400"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}

	rec = doGet(t, router, "/media/gen-000001/%2e%2e%2fsecret.txt")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("status traversal = %d, want 400 atau 404", rec.Code)
	}
}

func TestRouterDisablesMediaWithoutResolver(t *testing.T) {
	rec := doGet(t, newTestRouter(t, httpserver.Config{}), "/media/gen-000001/seg-00001.ts")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRouterMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()
	newTestRouter(t, httpserver.Config{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != response.CodeMethodNotAllowed {
		t.Errorf("code = %d, want %d", body.Code, response.CodeMethodNotAllowed)
	}
}

func TestRouterLogsRequest(t *testing.T) {
	var buf logBuffer
	router := newTestRouter(t, httpserver.Config{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	doGet(t, router, "/health")

	out := buf.String()
	if out == "" {
		t.Fatal("access log tidak menulis apa pun")
	}
	for _, want := range []string{"method=GET", "path=/health", "status=200", "duration_ms=", "request_id="} {
		if !containsString(out, want) {
			t.Errorf("access log tidak memuat %q: %s", want, out)
		}
	}
}

// logBuffer adalah buffer thread-safe untuk menangkap output slog.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func containsString(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
