package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telegramAPIBase adalah akar Bot API Telegram.
const telegramAPIBase = "https://api.telegram.org"

// maxTelegramErrorRunes membatasi panjang pesan error yang dikutip ke chat
// supaya teks tetap jauh di bawah batas 4096 karakter Telegram.
const maxTelegramErrorRunes = 400

// Telegram mengirim ringkasan Event sebagai pesan teks ke satu chat lewat Bot
// API (POST /bot<token>/sendMessage dengan form chat_id dan text).
//
// Token bot tidak pernah muncul pada error yang dikembalikan, sehingga aman
// dicatat ke log.
type Telegram struct {
	// Token adalah token bot dari BotFather; wajib.
	Token string
	// ChatID adalah tujuan pesan (id chat atau @channel); wajib.
	ChatID string
	// Client dipakai untuk mengirim permintaan. Bila nil dipakai
	// http.DefaultClient.
	Client *http.Client

	// baseURL menggantikan telegramAPIBase pada pengujian.
	baseURL string
}

// Telegram implements Notifier.
var _ Notifier = (*Telegram)(nil)

// NewTelegram menyiapkan Telegram dengan batas waktu timeout per pengiriman.
// timeout tidak positif memakai http.DefaultClient.
func NewTelegram(token, chatID string, timeout time.Duration) *Telegram {
	t := &Telegram{Token: strings.TrimSpace(token), ChatID: strings.TrimSpace(chatID)}
	if timeout > 0 {
		t.Client = &http.Client{Timeout: timeout}
	}
	return t
}

// Notify mengirim e sebagai pesan teks berbahasa Indonesia. Kegagalan
// sementara diulang sesuai kebijakan paket ini; error yang dikembalikan memuat
// kode status HTTP dan tidak pernah memuat token bot.
func (t *Telegram) Notify(ctx context.Context, e Event) error {
	if t == nil {
		return errors.New("notify: telegram tidak dikonfigurasi")
	}
	token := strings.TrimSpace(t.Token)
	if token == "" {
		return errors.New("notify: token bot Telegram kosong")
	}
	chatID := strings.TrimSpace(t.ChatID)
	if chatID == "" {
		return errors.New("notify: chat id Telegram kosong")
	}

	base := strings.TrimRight(strings.TrimSpace(t.baseURL), "/")
	if base == "" {
		base = telegramAPIBase
	}
	endpoint := base + "/bot" + token + "/sendMessage"

	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", telegramText(e))
	body := form.Encode()

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}

	err := withRetry(ctx, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return false, fmt.Errorf("notify: siapkan permintaan telegram: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			// Galat jaringan (termasuk timeout) bisa bersifat sementara.
			return true, fmt.Errorf("notify: kirim telegram: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return false, nil
		}
		return retriableStatus(resp.StatusCode),
			fmt.Errorf("notify: telegram: status %d%s", resp.StatusCode, snippetSuffix(payload))
	})

	// Pesan galat dari net/http memuat URL lengkap, termasuk token bot.
	return redact(err, token)
}

// telegramText menyusun ringkasan Event berbahasa Indonesia.
func telegramText(e Event) string {
	status := e.Status
	if status == "" {
		status = "tidak diketahui"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Sinkronisasi %s: %s (%s)\n", generationLabel(e.Generation), statusLabel(status), status)
	fmt.Fprintf(&b, "Judul: %d, playable: %d\n", e.Titles, e.Playables)
	fmt.Fprintf(&b, "Rendition siap: %d, gagal: %d\n", e.RenditionsReady, e.RenditionsFailed)
	fmt.Fprintf(&b, "Byte terunduh: %s\n", formatBytes(e.BytesDownloaded))
	if msg := oneLine(e.Error, maxTelegramErrorRunes); msg != "" {
		fmt.Fprintf(&b, "Error: %s\n", msg)
	}
	if !e.At.IsZero() {
		fmt.Fprintf(&b, "Waktu: %s\n", e.At.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// generationLabel mengembalikan label generation atau penanda bila kosong.
func generationLabel(label string) string {
	if strings.TrimSpace(label) == "" {
		return "tanpa generation"
	}
	return label
}

// statusLabel menerjemahkan status Event ke bahasa Indonesia.
func statusLabel(status string) string {
	switch status {
	case StatusCompleted:
		return "selesai"
	case StatusPartial:
		return "selesai sebagian"
	case StatusFailed:
		return "gagal"
	case StatusCancelled:
		return "dibatalkan"
	default:
		return "status tidak dikenal"
	}
}

// formatBytes memformat jumlah byte menjadi satuan biner yang mudah dibaca.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// oneLine merapikan s menjadi satu baris dan memotongnya paling banyak max
// karakter.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
