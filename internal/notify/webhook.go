package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Webhook mengirim Event sebagai badan JSON ke satu URL HTTP lewat POST.
//
// Permintaan memakai header Content-Type: application/json, dan badan respons
// dibaca paling banyak 4 KB sebelum ditutup. Status 2xx dianggap berhasil,
// 429/5xx diulang, sedangkan 4xx lain dilaporkan sebagai kegagalan permanen.
type Webhook struct {
	// URL adalah tujuan POST; wajib.
	URL string
	// Client dipakai untuk mengirim permintaan. Bila nil dipakai
	// http.DefaultClient.
	Client *http.Client
}

// Webhook implements Notifier.
var _ Notifier = (*Webhook)(nil)

// NewWebhook menyiapkan Webhook ke url dengan batas waktu timeout per
// pengiriman. timeout tidak positif memakai http.DefaultClient.
func NewWebhook(url string, timeout time.Duration) *Webhook {
	w := &Webhook{URL: strings.TrimSpace(url)}
	if timeout > 0 {
		w.Client = &http.Client{Timeout: timeout}
	}
	return w
}

// Notify mengirim e sebagai JSON ke w.URL. Kegagalan sementara diulang sesuai
// kebijakan paket ini; error yang dikembalikan memuat kode status HTTP.
func (w *Webhook) Notify(ctx context.Context, e Event) error {
	if w == nil {
		return errors.New("notify: webhook tidak dikonfigurasi")
	}
	target := strings.TrimSpace(w.URL)
	if target == "" {
		return errors.New("notify: URL webhook kosong")
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("notify: encode event webhook: %w", err)
	}

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}

	return withRetry(ctx, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return false, fmt.Errorf("notify: siapkan permintaan webhook: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// Galat jaringan (termasuk timeout) bisa bersifat sementara.
			return true, fmt.Errorf("notify: kirim webhook %s: %w", target, err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return false, nil
		}
		return retriableStatus(resp.StatusCode),
			fmt.Errorf("notify: webhook %s: status %d%s", target, resp.StatusCode, snippetSuffix(body))
	})
}
