package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWebhookMengirimEventJSON memastikan Webhook memakai POST, path yang
// dikonfigurasi, header JSON dan badan berisi seluruh field Event.
func TestWebhookMengirimEventJSON(t *testing.T) {
	var method, path, ctype string
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		ctype = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	want := testEvent()
	if err := NewWebhook(srv.URL+"/hooks/sync", 2*time.Second).Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, mau POST", method)
	}
	if path != "/hooks/sync" {
		t.Errorf("path = %q, mau /hooks/sync", path)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, mau application/json", ctype)
	}

	var got Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.RunID != want.RunID {
		t.Errorf("RunID = %d, mau %d", got.RunID, want.RunID)
	}
	if got.Generation != want.Generation {
		t.Errorf("Generation = %q, mau %q", got.Generation, want.Generation)
	}
	if got.Status != want.Status {
		t.Errorf("Status = %q, mau %q", got.Status, want.Status)
	}
	if got.Titles != want.Titles || got.Playables != want.Playables {
		t.Errorf("Titles/Playables = %d/%d, mau %d/%d", got.Titles, got.Playables, want.Titles, want.Playables)
	}
	if got.RenditionsReady != want.RenditionsReady || got.RenditionsFailed != want.RenditionsFailed {
		t.Errorf("ready/failed = %d/%d, mau %d/%d",
			got.RenditionsReady, got.RenditionsFailed, want.RenditionsReady, want.RenditionsFailed)
	}
	if got.BytesDownloaded != want.BytesDownloaded {
		t.Errorf("BytesDownloaded = %d, mau %d", got.BytesDownloaded, want.BytesDownloaded)
	}
	if got.Error != want.Error {
		t.Errorf("Error = %q, mau %q", got.Error, want.Error)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, mau %v", got.At, want.At)
	}
}

// TestWebhookRetry5xxLaluSukses memastikan dua kegagalan 500 diulang dan
// percobaan ketiga yang berhasil menutup pengiriman.
func TestWebhookRetry5xxLaluSukses(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			http.Error(w, "coba lagi", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL, 2*time.Second).Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("jumlah request = %d, mau 3", got)
	}
}

// TestWebhook400TanpaRetry memastikan status 4xx selain 429 tidak diulang dan
// kodenya muncul di pesan galat.
func TestWebhook400TanpaRetry(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "payload salah", http.StatusBadRequest)
	}))
	defer srv.Close()

	err := NewWebhook(srv.URL, 2*time.Second).Notify(context.Background(), testEvent())
	if err == nil {
		t.Fatal("Notify: err = nil, mau galat")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("jumlah request = %d, mau 1 (tanpa retry)", got)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("galat = %q, mau memuat kode 400", err)
	}
	if !strings.Contains(err.Error(), "payload salah") {
		t.Errorf("galat = %q, mau memuat body respons", err)
	}
}

// TestWebhook429Diulang memastikan 429 diperlakukan sebagai kegagalan sementara.
func TestWebhook429Diulang(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL, 2*time.Second).Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("jumlah request = %d, mau 2", got)
	}
}

// TestWebhookBatalSaatRetry memastikan context yang dibatalkan menghentikan
// pengiriman lebih cepat daripada jeda backoff.
func TestWebhookBatalSaatRetry(t *testing.T) {
	var requests atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	start := time.Now()
	err := NewWebhook(srv.URL, 2*time.Second).Notify(ctx, testEvent())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Notify: err = nil, mau galat")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, mau membungkus context.Canceled", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Notify kembali setelah %v, mau segera", elapsed)
	}
	if got := requests.Load(); got > 2 {
		t.Errorf("jumlah request = %d, mau tidak berlanjut setelah pembatalan", got)
	}
}
