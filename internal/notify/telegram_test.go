package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestTelegramMengirimSendMessage memastikan Telegram memakai POST
// /bot<token>/sendMessage dengan form chat_id dan text yang memuat status.
func TestTelegramMengirimSendMessage(t *testing.T) {
	const token = "123456:ABC-DEF"

	var method, path, chatID, text string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		chatID = r.PostForm.Get("chat_id")
		text = r.PostForm.Get("text")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	tg := NewTelegram(token, "42", 2*time.Second)
	tg.baseURL = srv.URL

	ev := testEvent()
	ev.Status = StatusCompleted
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, mau POST", method)
	}
	if want := "/bot" + token + "/sendMessage"; path != want {
		t.Errorf("path = %q, mau %q", path, want)
	}
	if chatID != "42" {
		t.Errorf("chat_id = %q, mau 42", chatID)
	}

	for _, want := range []string{
		StatusCompleted,
		ev.Generation,
		"Judul: 2",
		"playable: 3",
		"Rendition siap: 4, gagal: 1",
		"Byte terunduh:",
		"Error: satu rendition gagal",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, mau memuat %q", text, want)
		}
	}
	if strings.Contains(text, token) {
		t.Errorf("text memuat token bot: %q", text)
	}
}

// TestTelegramTokenTidakBocorDiError memastikan token bot tidak muncul pada
// galat yang dikembalikan, termasuk bila server menggemakannya, dan status 400
// tidak diulang.
func TestTelegramTokenTidakBocorDiError(t *testing.T) {
	const token = "987654:RAHASIA-jangan-bocor"

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: token `+token+` tidak valid"}`)
	}))
	defer srv.Close()

	tg := NewTelegram(token, "42", 2*time.Second)
	tg.baseURL = srv.URL

	err := tg.Notify(context.Background(), testEvent())
	if err == nil {
		t.Fatal("Notify: err = nil, mau galat")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token bot bocor di galat: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("galat = %q, mau memuat kode 400", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("jumlah request = %d, mau 1 (4xx tidak diulang)", got)
	}
}

// TestTelegramGalatJaringanTanpaToken memastikan galat jaringan (yang memuat URL
// lengkap beserta token) tetap tersunting dan pengiriman berhenti saat context
// dibatalkan.
func TestTelegramGalatJaringanTanpaToken(t *testing.T) {
	const token = "111:JARINGAN-rahasia"

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // koneksi ke addr pasti ditolak

	tg := NewTelegram(token, "42", time.Second)
	tg.baseURL = addr

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := tg.Notify(ctx, testEvent())
	if err == nil {
		t.Fatal("Notify: err = nil, mau galat")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token bot bocor di galat: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("galat = %q, mau memuat penanda penyuntingan", err)
	}
}

// TestTelegram5xxDiulang memastikan status 5xx diulang sampai batas percobaan.
func TestTelegram5xxDiulang(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	tg := NewTelegram("1:tok", "42", 2*time.Second)
	tg.baseURL = srv.URL

	if err := tg.Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("jumlah request = %d, mau 2", got)
	}
}
