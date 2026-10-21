package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// testEvent mengembalikan Event contoh yang dipakai beberapa uji.
func testEvent() Event {
	return Event{
		RunID:            7,
		Generation:       "gen-000007",
		Status:           StatusPartial,
		Titles:           2,
		Playables:        3,
		RenditionsReady:  4,
		RenditionsFailed: 1,
		BytesDownloaded:  12_345,
		Error:            "satu rendition gagal",
		At:               time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
	}
}

// recordingNotifier mencatat setiap Event yang diterimanya.
type recordingNotifier struct {
	mu     sync.Mutex
	events []Event
	err    error

	calls int
}

// Notify implements Notifier.
func (r *recordingNotifier) Notify(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++
	r.events = append(r.events, e)
	return r.err
}

// count mengembalikan jumlah pemanggilan Notify.
func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestMultiMemanggilSemuaNotifier memastikan notifier yang gagal tidak
// menghentikan notifier lain dan seluruh galat digabung.
func TestMultiMemanggilSemuaNotifier(t *testing.T) {
	errA := errors.New("kanal A rusak")
	errB := errors.New("kanal B rusak")

	a := &recordingNotifier{err: errA}
	ok := &recordingNotifier{}
	b := &recordingNotifier{err: errB}

	ev := testEvent()
	err := Multi{a, ok, b}.Notify(context.Background(), ev)

	if !errors.Is(err, errA) {
		t.Errorf("error tidak memuat galat kanal A: %v", err)
	}
	if !errors.Is(err, errB) {
		t.Errorf("error tidak memuat galat kanal B: %v", err)
	}
	for name, n := range map[string]*recordingNotifier{"A": a, "tengah": ok, "B": b} {
		if got := n.count(); got != 1 {
			t.Errorf("notifier %s dipanggil %d kali, mau 1", name, got)
		}
	}
	if ok.events[0] != ev {
		t.Errorf("event yang diterima = %+v, mau %+v", ok.events[0], ev)
	}
}

// TestMultiTanpaGalat memastikan Multi yang seluruhnya berhasil mengembalikan
// nil.
func TestMultiTanpaGalat(t *testing.T) {
	a, b := &recordingNotifier{}, &recordingNotifier{}
	if err := (Multi{a, b}).Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Errorf("pemanggilan = %d/%d, mau 1/1", a.count(), b.count())
	}
}

// TestMultiAmanTerhadapNil memastikan Multi kosong maupun berisi Notifier nil
// tidak panic dan tidak menghasilkan galat.
func TestMultiAmanTerhadapNil(t *testing.T) {
	var empty Multi
	if err := empty.Notify(context.Background(), testEvent()); err != nil {
		t.Errorf("Multi kosong: err = %v, mau nil", err)
	}

	withNil := Multi{nil, nil}
	if err := withNil.Notify(context.Background(), testEvent()); err != nil {
		t.Errorf("Multi berisi nil: err = %v, mau nil", err)
	}
}

// TestNotifierTakDikonfigurasi memastikan Webhook/Telegram dengan penerima nil
// mengembalikan galat, bukan panic.
func TestNotifierTakDikonfigurasi(t *testing.T) {
	var w *Webhook
	if err := w.Notify(context.Background(), testEvent()); err == nil {
		t.Error("(*Webhook)(nil).Notify: err = nil, mau galat")
	}

	var tg *Telegram
	if err := tg.Notify(context.Background(), testEvent()); err == nil {
		t.Error("(*Telegram)(nil).Notify: err = nil, mau galat")
	}
}

// TestWithRetryMenghormatiPembatalan memastikan penantian jeda berhenti segera
// setelah context dibatalkan.
func TestWithRetryMenghormatiPembatalan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	start := time.Now()
	err := withRetry(ctx, func(context.Context) (bool, error) {
		calls++
		cancel()
		return true, errors.New("gagal sementara")
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, mau membungkus context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("percobaan = %d, mau 1", calls)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("dibatalkan setelah %v, mau segera", elapsed)
	}
}
