package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vodlocal/internal/store"
	"vodlocal/internal/syncjob"
)

// testTick adalah periode pemeriksaan yang dipakai test loop scheduler. Nilai
// ini di bawah batas minimum Config.Tick (1 detik), jadi test memakainya lewat
// newFastScheduler supaya tidak perlu menunggu satu detik per tick. Batas
// minimum itu sendiri diuji di TestNewDefaults.
const testTick = 10 * time.Millisecond

// testWait adalah batas tunggu maksimum untuk satu kondisi test.
const testWait = 5 * time.Second

// testLogger membuang seluruh log supaya keluaran test tetap bersih.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore membuat Store bermigrasi di direktori sementara.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "vodlocal.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("store.Migrate() error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// setSetting menulis satu baris app_settings (upsert).
func setSetting(t *testing.T, db *store.Store, key, value string) {
	t.Helper()

	_, err := db.Write().ExecContext(context.Background(), `
INSERT INTO app_settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`,
		key, value)
	if err != nil {
		t.Fatalf("set setting %q = %q: %v", key, value, err)
	}
}

// newFastScheduler menyiapkan Scheduler lewat New lalu memakai periode tick
// test (10ms) supaya test cepat. Selain Tick, konfigurasi tetap diproses oleh
// New seperti biasa.
func newFastScheduler(t *testing.T, cfg Config) *Scheduler {
	t.Helper()

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	s.tick = testTick

	return s
}

// waitFor menunggu sampai cond bernilai true, atau menggagalkan test bila
// melewati testWait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout menunggu %s", what)
}

// startScheduler menjalankan s.Run di goroutine dan mengembalikan cancel serta
// kanal hasil.
func startScheduler(t *testing.T, s *Scheduler) (context.CancelFunc, chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	return cancel, done
}

// stopScheduler membatalkan ctx dan memastikan Run kembali tanpa error.
func stopScheduler(t *testing.T, cancel context.CancelFunc, done chan error) {
	t.Helper()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Run() tidak kembali setelah context dibatalkan")
	}
}

// stubRunner adalah SyncRunner berbasis kanal untuk test. Setiap Run dicatat,
// memberi sinyal ke started, lalu menunggu gate bila dipasang.
type stubRunner struct {
	mu       sync.Mutex
	triggers []string
	gate     chan struct{}
	started  chan struct{}
}

// newStubRunner membuat stub dengan kanal started berkapasitas buffer.
func newStubRunner(buffer int) *stubRunner {
	if buffer < 1 {
		buffer = 1
	}
	return &stubRunner{started: make(chan struct{}, buffer)}
}

// block memasang gate: Run berikutnya menunggu sampai gate ditutup.
func (r *stubRunner) block(gate chan struct{}) {
	r.mu.Lock()
	r.gate = gate
	r.mu.Unlock()
}

// Run mencatat pemicu lalu menunggu gate bila ada.
func (r *stubRunner) Run(ctx context.Context, trigger string) (syncjob.Result, error) {
	r.mu.Lock()
	r.triggers = append(r.triggers, trigger)
	count := len(r.triggers)
	gate := r.gate
	r.mu.Unlock()

	select {
	case r.started <- struct{}{}:
	default:
	}

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return syncjob.Result{}, ctx.Err()
		}
	}

	return syncjob.Result{RunID: int64(count)}, nil
}

// count mengembalikan jumlah Run yang sudah dimulai.
func (r *stubRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.triggers)
}

// triggerList mengembalikan salinan nilai trigger yang diterima.
func (r *stubRunner) triggerList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.triggers...)
}

// stepClock adalah jam palsu yang naik sebesar step pada setiap pemanggilan
// Now, yaitu sekali per tick scheduler.
type stepClock struct {
	mu   sync.Mutex
	at   time.Time
	step time.Duration
	n    int
}

// newStepClock membuat jam palsu yang mulai pada start.
func newStepClock(start time.Time, step time.Duration) *stepClock {
	return &stepClock{at: start, step: step}
}

// Now mengembalikan waktu sekarang lalu memajukannya satu langkah.
func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	at := c.at
	c.at = c.at.Add(c.step)
	c.n++

	return at
}

// calls mengembalikan berapa kali Now sudah dipanggil.
func (c *stepClock) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// seqClock adalah jam palsu yang mengembalikan daftar waktu berurutan. Setelah
// daftar habis ia mengembalikan sentinel (waktu yang tidak cocok dengan jadwal
// yang diuji) dan menutup kanal exhausted.
type seqClock struct {
	mu       sync.Mutex
	times    []time.Time
	idx      int
	sentinel time.Time
	exhaust  chan struct{}
	once     sync.Once
}

// newSeqClock membuat jam palsu dari daftar times dengan sentinel tertentu.
func newSeqClock(times []time.Time, sentinel time.Time) *seqClock {
	return &seqClock{times: times, sentinel: sentinel, exhaust: make(chan struct{})}
}

// Now mengembalikan waktu berikutnya dari daftar.
func (c *seqClock) Now() time.Time {
	c.mu.Lock()

	var at time.Time
	done := false
	if c.idx < len(c.times) {
		at = c.times[c.idx]
		c.idx++
		done = c.idx >= len(c.times)
	} else {
		at = c.sentinel
		done = true
	}

	c.mu.Unlock()

	if done {
		c.once.Do(func() { close(c.exhaust) })
	}
	return at
}

// exhausted menutup ketika daftar waktu sudah habis dipakai.
func (c *seqClock) exhausted() <-chan struct{} { return c.exhaust }

// minuteSeries membuat n waktu mulai dari start dengan jarak step.
func minuteSeries(start time.Time, step time.Duration, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, start.Add(time.Duration(i)*step))
	}
	return out
}

// noMatchSentinel adalah waktu yang tidak cocok dengan jadwal yang dipakai
// test (Sabtu 00:00, sedangkan jadwalnya hari Senin jam 02:00).
func noMatchSentinel() time.Time {
	return time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
}

// 5. Scheduler memicu SyncRunner tepat sekali untuk satu menit yang sama,
// walaupun banyak tick jatuh di menit itu.
func TestSchedulerTriggersOncePerMinute(t *testing.T) {
	// 1 Januari 2024 adalah hari Senin.
	monday := time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		cron  string
		times []time.Time
		want  int
	}{
		{"satu menit", "0 2 * * 1", minuteSeries(monday, time.Second, 60), 1},
		{"dua menit", "* 2 * * 1", minuteSeries(monday, time.Second, 90), 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestStore(t)
			setSetting(t, db, settingEnabled, "1")
			setSetting(t, db, settingCron, tc.cron)

			runner := newStubRunner(len(tc.times))
			clock := newSeqClock(tc.times, noMatchSentinel())

			s := newFastScheduler(t, Config{
				Store:    db,
				Syncer:   runner,
				Logger:   testLogger(),
				Now:      clock.Now,
				Location: time.UTC,
			})

			cancel, done := startScheduler(t, s)

			select {
			case <-clock.exhausted():
			case <-time.After(testWait):
				t.Fatal("jam palsu tidak pernah habis; scheduler tidak berjalan")
			}

			stopScheduler(t, cancel, done)

			waitFor(t, "seluruh pemicu selesai", func() bool { return runner.count() >= tc.want })

			// Beri kesempatan tick sisa: jumlah pemicu tidak boleh bertambah.
			time.Sleep(20 * time.Millisecond)
			if got := runner.count(); got != tc.want {
				t.Errorf("jumlah pemicu = %d, ingin %d", got, tc.want)
			}
			for _, trigger := range runner.triggerList() {
				if trigger != triggerScheduled {
					t.Errorf("trigger = %q, ingin %q", trigger, triggerScheduled)
				}
			}
		})
	}
}

// 6. sync.schedule_enabled bernilai tidak benar → tidak pernah memicu.
func TestSchedulerDisabledNeverTriggers(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", ""} {
		t.Run(value, func(t *testing.T) {
			db := newTestStore(t)
			setSetting(t, db, settingEnabled, value)
			setSetting(t, db, settingCron, "* * * * *")

			runner := newStubRunner(8)
			clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Minute)

			s := newFastScheduler(t, Config{
				Store:    db,
				Syncer:   runner,
				Logger:   testLogger(),
				Now:      clock.Now,
				Location: time.UTC,
			})

			cancel, done := startScheduler(t, s)
			waitFor(t, "20 tick", func() bool { return clock.calls() >= 20 })
			stopScheduler(t, cancel, done)

			if got := runner.count(); got != 0 {
				t.Errorf("jumlah pemicu = %d dengan setting %q, ingin 0", got, value)
			}
		})
	}
}

// sync.schedule_enabled menerima 1/true/yes/on tanpa membedakan huruf besar.
func TestIsTrue(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"True":  true,
		"yes":   true,
		"YES":   true,
		"on":    true,
		"On":    true,
		" 1 ":   true,
		"0":     false,
		"false": false,
		"no":    false,
		"off":   false,
		"":      false,
		"2":     false,
		"ya":    false,
	}

	for raw, want := range cases {
		if got := isTrue(raw); got != want {
			t.Errorf("isTrue(%q) = %v, ingin %v", raw, got, want)
		}
	}
}

// 7. Cron rusak di settings tidak membuat panic, tidak memicu sync, dan loop
// tetap hidup sehingga jadwal yang diperbaiki langsung berlaku.
func TestSchedulerSurvivesBadCron(t *testing.T) {
	db := newTestStore(t)
	setSetting(t, db, settingEnabled, "TRUE")
	setSetting(t, db, settingCron, "setiap menit")

	runner := newStubRunner(8)
	clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Second)

	s := newFastScheduler(t, Config{
		Store:    db,
		Syncer:   runner,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: time.UTC,
	})

	cancel, done := startScheduler(t, s)

	waitFor(t, "tick dengan cron rusak", func() bool { return clock.calls() >= 10 })

	select {
	case err := <-done:
		t.Fatalf("Run() berhenti saat cron rusak (error: %v), ingin tetap hidup", err)
	default:
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("jumlah pemicu = %d saat cron rusak, ingin 0", got)
	}

	// Perbaiki setting: cron valid harus langsung dipakai tick berikutnya.
	setSetting(t, db, settingCron, "* * * * *")
	waitFor(t, "pemicu setelah cron diperbaiki", func() bool { return runner.count() >= 1 })

	stopScheduler(t, cancel, done)

	if got := runner.count(); got != 1 {
		t.Errorf("jumlah pemicu setelah cron diperbaiki = %d, ingin 1", got)
	}
}

// 8. Context dibatalkan → Run kembali tanpa error.
func TestSchedulerStopsOnContextCancel(t *testing.T) {
	db := newTestStore(t)
	clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Minute)

	s := newFastScheduler(t, Config{
		Store:    db,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: time.UTC,
	})

	cancel, done := startScheduler(t, s)
	waitFor(t, "tick pertama", func() bool { return clock.calls() >= 2 })

	stopScheduler(t, cancel, done)

	// Run kedua kali setelah berhenti juga harus langsung kembali.
	cancel2, done2 := startScheduler(t, s)
	stopScheduler(t, cancel2, done2)
}

// 9. Bila run sebelumnya belum selesai, pemicu berikutnya dilewati sehingga
// tidak pernah ada dua run bersamaan.
func TestSchedulerSkipsWhileRunStillRunning(t *testing.T) {
	db := newTestStore(t)
	setSetting(t, db, settingEnabled, "1")
	setSetting(t, db, settingCron, "* * * * *")

	runner := newStubRunner(8)
	// Setiap tick maju satu menit, jadi setiap tick adalah peluang pemicu baru.
	clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Minute)

	gate := make(chan struct{})
	runner.block(gate)

	s := newFastScheduler(t, Config{
		Store:    db,
		Syncer:   runner,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: time.UTC,
	})

	cancel, done := startScheduler(t, s)

	// Run pertama harus benar-benar mulai dan memblokir.
	select {
	case <-runner.started:
	case <-time.After(testWait):
		t.Fatal("run terjadwal pertama tidak pernah dimulai")
	}

	// Biarkan beberapa menit berlalu selama run pertama masih berjalan.
	start := clock.calls()
	waitFor(t, "menit-menit berikutnya", func() bool { return clock.calls() >= start+3 })

	if got := runner.count(); got != 1 {
		t.Fatalf("jumlah run = %d saat run pertama masih berjalan, ingin 1", got)
	}

	// Hentikan loop lebih dulu, baru lepaskan run yang memblokir.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Run() tidak kembali setelah context dibatalkan")
	}

	close(gate)

	if got := runner.count(); got != 1 {
		t.Errorf("jumlah run = %d setelah gate dibuka, ingin 1", got)
	}
}

// SyncRunner nil → scheduler hanya tidur, tidak panic.
func TestSchedulerNilSyncerOnlySleeps(t *testing.T) {
	db := newTestStore(t)
	setSetting(t, db, settingEnabled, "ON")
	setSetting(t, db, settingCron, "* * * * *")

	clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Second)

	s := newFastScheduler(t, Config{
		Store:    db,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: time.UTC,
	})

	cancel, done := startScheduler(t, s)
	waitFor(t, "20 tick tanpa SyncRunner", func() bool { return clock.calls() >= 20 })
	stopScheduler(t, cancel, done)
}

// Kegagalan query database hanya menghasilkan log Warn; loop tetap hidup dan
// tidak memicu sync.
func TestSchedulerSurvivesStoreError(t *testing.T) {
	db := newTestStore(t)
	setSetting(t, db, settingEnabled, "1")
	setSetting(t, db, settingCron, "* * * * *")

	// Tutup database sebelum loop dimulai: setiap query berikutnya gagal.
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	runner := newStubRunner(8)
	clock := newStepClock(time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC), time.Minute)

	s := newFastScheduler(t, Config{
		Store:    db,
		Syncer:   runner,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: time.UTC,
	})

	cancel, done := startScheduler(t, s)
	waitFor(t, "tick dengan database tertutup", func() bool { return clock.calls() >= 10 })
	stopScheduler(t, cancel, done)

	if got := runner.count(); got != 0 {
		t.Errorf("jumlah pemicu = %d saat database rusak, ingin 0", got)
	}
}

// New menerapkan nilai bawaan dan menolak Config tanpa Store.
func TestNewDefaults(t *testing.T) {
	db := newTestStore(t)

	s, err := New(Config{Store: db})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if s.tick != defaultTick {
		t.Errorf("Tick bawaan = %v, ingin %v", s.tick, defaultTick)
	}
	if s.loc != time.Local {
		t.Errorf("Location bawaan = %v, ingin time.Local", s.loc)
	}
	if s.nowFn == nil || s.log == nil {
		t.Error("Now/Logger bawaan tidak terisi")
	}
	if s.syncer != nil {
		t.Error("Syncer = non-nil, ingin nil")
	}

	small, err := New(Config{Store: db, Tick: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if small.tick != minTick {
		t.Errorf("Tick = %v untuk Tick 10ms, ingin %v", small.tick, minTick)
	}

	if _, err := New(Config{}); err == nil {
		t.Error("New() tanpa Store = nil error, ingin error")
	}

	loc := time.UTC
	withLoc, err := New(Config{Store: db, Location: loc, Tick: 5 * time.Second})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if withLoc.loc != loc || withLoc.tick != 5*time.Second {
		t.Errorf("Location/Tick = %v/%v, ingin %v/5s", withLoc.loc, withLoc.tick, loc)
	}
}
