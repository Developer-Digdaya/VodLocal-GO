// Package notify mengirim ringkasan satu kali sync run ke kanal luar: webhook
// HTTP (Webhook) dan chat Telegram (Telegram).
//
// Semua implementasi Notifier bersifat best effort. Galat pengiriman
// dikembalikan apa adanya, tetapi pemanggil (syncjob) hanya mencatatnya di log
// karena hasil sinkronisasi tidak boleh bergantung pada kanal notifikasi.
//
// Pengiriman diulang paling banyak tiga percobaan dengan jeda 500 ms lalu 1 s
// untuk kegagalan yang bersifat sementara: status 429, status 5xx dan galat
// jaringan. Status 4xx lain dianggap permanen dan tidak diulang. Penantian jeda
// selalu menghormati context, sehingga pembatalan menghentikan pengiriman
// segera.
//
// Rahasia (token bot Telegram) tidak pernah muncul di pesan error yang
// dikembalikan, sehingga aman dicatat ke log.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status yang mungkin ada pada Event.Status.
const (
	// StatusCompleted berarti run selesai dan seluruh rendition siap.
	StatusCompleted = "completed"
	// StatusPartial berarti run selesai tetapi ada rendition yang gagal.
	StatusPartial = "partial"
	// StatusFailed berarti run gagal fatal.
	StatusFailed = "failed"
	// StatusCancelled berarti run dihentikan lewat pembatalan context.
	StatusCancelled = "cancelled"
)

// Event merangkum satu sync run pada saat mencapai keadaan terminal.
type Event struct {
	// RunID adalah id baris sync_runs; 0 bila run belum sempat tercatat.
	RunID int64
	// Generation adalah label generation yang dibangun (mis. "gen-000042").
	Generation string
	// Status bernilai StatusCompleted, StatusPartial, StatusFailed atau
	// StatusCancelled.
	Status string
	// Titles adalah jumlah judul yang tersinkron pada run ini.
	Titles int
	// Playables adalah jumlah unit putar yang tersinkron.
	Playables int
	// RenditionsReady adalah jumlah rendition yang siap dipakai.
	RenditionsReady int
	// RenditionsFailed adalah jumlah rendition yang gagal.
	RenditionsFailed int
	// BytesDownloaded adalah total byte yang diunduh pada run ini.
	BytesDownloaded int64
	// Error berisi sebab kegagalan; kosong untuk run yang berhasil.
	Error string
	// At adalah waktu event dibuat.
	At time.Time
}

// Notifier mengirim satu Event ke sebuah kanal. Implementasi harus aman
// dipanggil dengan context yang sudah memiliki deadline dan tidak boleh
// menyimpan referensi ke Event setelah Notify kembali.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Multi menjalankan beberapa Notifier secara berurutan. Seluruh notifier tetap
// dipanggil walau ada yang gagal, dan semua galat digabung dengan errors.Join
// sehingga satu kanal yang rusak tidak menghalangi kanal lain. Notifier nil di
// dalam Multi dilewati.
type Multi []Notifier

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, e Event) error {
	errs := make([]error, 0, len(m))
	for _, n := range m {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Batas pengiriman yang dipakai bersama oleh Webhook dan Telegram.
const (
	// maxAttempts adalah jumlah percobaan maksimum per pengiriman.
	maxAttempts = 3
	// maxResponseBytes membatasi body respons yang dibaca (4 KB).
	maxResponseBytes = 4 << 10
	// maxSnippetRunes membatasi panjang body yang dikutip di pesan error.
	maxSnippetRunes = 512
)

// retryBackoff adalah jeda sebelum percobaan berikutnya. Dengan maxAttempts
// bernilai tiga hanya dua entri pertama yang terpakai (500 ms dan 1 s); entri
// 2 s disediakan untuk skenario batas percobaan dinaikkan.
var retryBackoff = []time.Duration{
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
}

// attempt adalah satu percobaan pengiriman. Nilai retry melaporkan apakah
// kegagalannya layak diulang.
type attempt func(ctx context.Context) (retry bool, err error)

// withRetry menjalankan send sampai berhasil, sampai send melaporkan kegagalan
// permanen, atau sampai maxAttempts percobaan habis. Context yang dibatalkan
// menghentikan penantian dan hasil akhirnya selalu membungkus ctx.Err().
func withRetry(ctx context.Context, send attempt) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var last error
	for n := 1; n <= maxAttempts; n++ {
		if n > 1 {
			if err := pause(ctx, retryBackoff[n-2]); err != nil {
				if last != nil {
					return errors.Join(last, err)
				}
				return err
			}
		}

		retry, err := send(ctx)
		if err == nil {
			return nil
		}
		last = err

		if cerr := ctx.Err(); cerr != nil {
			return errors.Join(last, cerr)
		}
		if !retry {
			return last
		}
	}
	return last
}

// pause menunggu d sebelum percobaan berikutnya, atau lebih cepat bila ctx
// dibatalkan.
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("notify: menunggu percobaan ulang: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// retriableStatus melaporkan apakah status HTTP layak diulang: 429 dan 5xx.
// Status 4xx lain (mis. 400/401/403/404) bersifat permanen.
func retriableStatus(code int) bool {
	return code == 429 || code >= 500
}

// snippetSuffix merangkum body respons menjadi akhiran pesan error, mis.
// ": invalid chat id". Body kosong menghasilkan string kosong.
func snippetSuffix(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > maxSnippetRunes {
		s = string(r[:maxSnippetRunes]) + "…"
	}
	return ": " + s
}

// redactedError menyembunyikan rahasia dari pesan error tetapi tetap
// mempertahankan rantai Unwrap, sehingga errors.Is/As tetap bekerja.
type redactedError struct {
	msg   string
	cause error
}

// Error implements error.
func (e *redactedError) Error() string { return e.msg }

// Unwrap mengembalikan galat asli.
func (e *redactedError) Unwrap() error { return e.cause }

// redact mengganti setiap kemunculan secrets pada pesan err dengan "***".
// Galat yang tidak memuat rahasia dikembalikan apa adanya.
func redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := msg
	for _, s := range secrets {
		if s == "" {
			continue
		}
		clean = strings.ReplaceAll(clean, s, "***")
	}
	if clean == msg {
		return err
	}
	return &redactedError{msg: clean, cause: err}
}
