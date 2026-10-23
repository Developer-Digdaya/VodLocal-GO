package syncjob

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vodlocal/internal/download"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/notify"
	"vodlocal/internal/provider"
)

// ---------------------------------------------------------------- helpers ---

// recordingNotifier mencatat Event yang diterimanya dan mengembalikan err tetap
// (bila diisi) untuk meniru kanal notifikasi yang rusak.
type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
	err    error
}

// Notify implements notify.Notifier.
func (r *recordingNotifier) Notify(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, e)
	return r.err
}

// got mengembalikan salinan Event yang tercatat.
func (r *recordingNotifier) got() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

// newNotifyRig menyiapkan Syncer lewat Config dengan Notifier terpasang.
// Helper ini mirip newRig, tetapi mengisi Config.Notifier supaya integrasi
// notifikasi benar-benar diuji dari jalur konfigurasi.
func newNotifyRig(t *testing.T, client *http.Client, n notify.Notifier, provs ...provider.Provider) *testRig {
	t.Helper()

	st := newTestStore(t)
	root := filepath.Join(t.TempDir(), "media")
	media, err := mediafs.New(root)
	if err != nil {
		t.Fatalf("mediafs.New: %v", err)
	}

	syncer, err := New(Config{
		Store:      st,
		Media:      media,
		Downloader: download.New(download.Options{Client: client, Retries: 0}),
		Providers:  provs,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: client,
		Notifier:   n,
	})
	if err != nil {
		t.Fatalf("syncjob.New: %v", err)
	}

	return &testRig{store: st, media: media, syncer: syncer, root: root}
}

// --------------------------------------------------------------- skenario ---

// TestRunTanpaNotifier memastikan Config.Notifier nil tidak membuat Run panic
// dan hasilnya tetap seperti biasa.
func TestRunTanpaNotifier(t *testing.T) {
	srv := mediaServer(t, 3)
	rig := newNotifyRig(t, srv.Client(), nil, movieProvider(srv.URL+"/hls/index.m3u8"))

	if rig.syncer.notifier != nil {
		t.Errorf("Syncer.notifier = %v, mau nil", rig.syncer.notifier)
	}

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RenditionsReady != 1 {
		t.Errorf("ready = %d, mau 1", res.RenditionsReady)
	}
	if got := scanString(t, rig.store.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
}

// TestRunNotifierHappyPath memastikan notifier dipanggil tepat sekali dengan
// status completed dan angka dari Result.
func TestRunNotifierHappyPath(t *testing.T) {
	srv := mediaServer(t, 3)
	rec := &recordingNotifier{}
	rig := newNotifyRig(t, srv.Client(), rec, movieProvider(srv.URL+"/hls/index.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("notifier dipanggil %d kali, mau 1", len(events))
	}
	ev := events[0]
	if ev.Status != notify.StatusCompleted {
		t.Errorf("Status = %q, mau %q", ev.Status, notify.StatusCompleted)
	}
	if ev.RunID != res.RunID || ev.RunID == 0 {
		t.Errorf("RunID = %d, mau %d", ev.RunID, res.RunID)
	}
	if ev.Generation != res.GenerationLabel || ev.Generation == "" {
		t.Errorf("Generation = %q, mau %q", ev.Generation, res.GenerationLabel)
	}
	if ev.Titles != 1 || ev.Playables != 1 {
		t.Errorf("Titles/Playables = %d/%d, mau 1/1", ev.Titles, ev.Playables)
	}
	if ev.RenditionsReady != 1 || ev.RenditionsFailed != 0 {
		t.Errorf("ready/failed = %d/%d, mau 1/0", ev.RenditionsReady, ev.RenditionsFailed)
	}
	if ev.BytesDownloaded <= 0 {
		t.Errorf("BytesDownloaded = %d, mau > 0", ev.BytesDownloaded)
	}
	if ev.Error != "" {
		t.Errorf("Error = %q, mau kosong", ev.Error)
	}
	if ev.At.IsZero() {
		t.Error("At kosong, mau terisi")
	}
}

// TestRunNotifierPartial memastikan rendition yang gagal dilaporkan dengan
// status partial beserta jumlah kegagalannya.
func TestRunNotifierPartial(t *testing.T) {
	srv := mediaServer(t, 3)

	titles := []provider.Title{{
		ExternalID: "mv1",
		Kind:       provider.KindMovie,
		Title:      "Film Uji",
		Slug:       "film-uji",
	}}
	playables := map[string][]provider.Playable{
		"mv1": {{
			ExternalID:      "mv1",
			TitleExternalID: "mv1",
			Variants: []provider.Variant{
				{
					Quality:     "360p",
					BitrateKbps: 800,
					Width:       640,
					Height:      360,
					Container:   provider.ContainerTS,
					PlaylistURL: srv.URL + "/hls/tidak-ada.m3u8",
				},
				{
					Quality:     "720p",
					BitrateKbps: 2200,
					Width:       1280,
					Height:      720,
					Container:   provider.ContainerTS,
					PlaylistURL: srv.URL + "/hls/index.m3u8",
				},
			},
		}},
	}

	rec := &recordingNotifier{}
	rig := newNotifyRig(t, srv.Client(), rec, provider.NewFake(titles, nil, playables))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Partial {
		t.Fatalf("Partial = false, mau true")
	}

	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("notifier dipanggil %d kali, mau 1", len(events))
	}
	ev := events[0]
	if ev.Status != notify.StatusPartial {
		t.Errorf("Status = %q, mau %q", ev.Status, notify.StatusPartial)
	}
	if ev.RenditionsReady != 1 || ev.RenditionsFailed != 1 {
		t.Errorf("ready/failed = %d/%d, mau 1/1", ev.RenditionsReady, ev.RenditionsFailed)
	}
	if ev.Error != "" {
		t.Errorf("Error = %q, mau kosong untuk run yang selesai sebagian", ev.Error)
	}
}

// TestRunNotifierFailed memastikan kegagalan fatal dilaporkan sekali dengan
// status failed dan sebabnya.
func TestRunNotifierFailed(t *testing.T) {
	srv := mediaServer(t, 3)
	rec := &recordingNotifier{}
	rig := newNotifyRig(t, srv.Client(), rec, movieProvider(srv.URL+"/hls/tidak-ada.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if !errors.Is(err, ErrNoRenditions) {
		t.Fatalf("Run: err = %v, mau ErrNoRenditions", err)
	}

	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("notifier dipanggil %d kali, mau 1", len(events))
	}
	ev := events[0]
	if ev.Status != notify.StatusFailed {
		t.Errorf("Status = %q, mau %q", ev.Status, notify.StatusFailed)
	}
	if ev.RunID != res.RunID || ev.RunID == 0 {
		t.Errorf("RunID = %d, mau %d", ev.RunID, res.RunID)
	}
	if ev.Generation != res.GenerationLabel {
		t.Errorf("Generation = %q, mau %q", ev.Generation, res.GenerationLabel)
	}
	if ev.RenditionsFailed != 1 {
		t.Errorf("RenditionsFailed = %d, mau 1", ev.RenditionsFailed)
	}
	if ev.Error == "" {
		t.Error("Error kosong, mau memuat sebab kegagalan")
	}
}

// TestRunNotifierCancelled memastikan run yang dibatalkan dilaporkan dengan
// status cancelled walau context run sudah mati.
func TestRunNotifierCancelled(t *testing.T) {
	srv := mediaServer(t, 3)

	fake := movieProvider(srv.URL + "/hls/index.m3u8")
	fake.Delay = 2 * time.Second

	rec := &recordingNotifier{}
	rig := newNotifyRig(t, srv.Client(), rec, fake)

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := rig.syncer.Run(context.Background(), "manual")
		done <- outcome{res: res, err: err}
	}()

	runID := waitActiveRun(t, rig.store)
	if err := rig.syncer.Cancel(runID); err != nil {
		t.Fatalf("Cancel(%d): %v", runID, err)
	}

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, mau context.Canceled", got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run tidak berhenti setelah Cancel")
	}

	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("notifier dipanggil %d kali, mau 1", len(events))
	}
	if events[0].Status != notify.StatusCancelled {
		t.Errorf("Status = %q, mau %q", events[0].Status, notify.StatusCancelled)
	}
	if events[0].RunID != runID {
		t.Errorf("RunID = %d, mau %d", events[0].RunID, runID)
	}
}

// TestRunNotifierErrorTidakMenggagalkanRun memastikan galat notifier hanya
// dicatat dan tidak mengubah hasil Run.
func TestRunNotifierErrorTidakMenggagalkanRun(t *testing.T) {
	srv := mediaServer(t, 3)
	rec := &recordingNotifier{err: errors.New("webhook mati")}
	rig := newNotifyRig(t, srv.Client(), rec, movieProvider(srv.URL+"/hls/index.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v, mau nil walau notifier gagal", err)
	}
	if res.RenditionsReady != 1 {
		t.Errorf("ready = %d, mau 1", res.RenditionsReady)
	}
	if got := len(rec.got()); got != 1 {
		t.Errorf("notifier dipanggil %d kali, mau 1", got)
	}
	if got := scanString(t, rig.store.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
}

// panickingNotifier meniru notifier pihak ketiga yang panic.
type panickingNotifier struct{}

// Notify implements notify.Notifier.
func (panickingNotifier) Notify(context.Context, notify.Event) error {
	panic("notifier rusak")
}

// TestRunNotifierPanikTidakMenggagalkanRun memastikan panic di dalam notifier
// berubah menjadi peringatan log, bukan kegagalan Run.
func TestRunNotifierPanikTidakMenggagalkanRun(t *testing.T) {
	srv := mediaServer(t, 3)
	rig := newNotifyRig(t, srv.Client(), panickingNotifier{}, movieProvider(srv.URL+"/hls/index.m3u8"))

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v, mau nil walau notifier panic", err)
	}
	if res.RenditionsReady != 1 {
		t.Errorf("ready = %d, mau 1", res.RenditionsReady)
	}
	if got := scanString(t, rig.store.Read(), `SELECT status FROM sync_runs WHERE id = ?`, res.RunID); got != "completed" {
		t.Errorf("sync_runs.status = %q, mau completed", got)
	}
}
