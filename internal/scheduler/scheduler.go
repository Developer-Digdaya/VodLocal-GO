package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vodlocal/internal/store"
	"vodlocal/internal/syncjob"
)

const (
	// settingEnabled adalah kunci app_settings untuk mengaktifkan jadwal.
	settingEnabled = "sync.schedule_enabled"
	// settingCron adalah kunci app_settings untuk ekspresi cron jadwal.
	settingCron = "sync.schedule_cron"

	// defaultTick adalah periode pemeriksaan bawaan bila Config.Tick kosong.
	defaultTick = 30 * time.Second
	// minTick adalah periode pemeriksaan terkecil yang diterima; nilai lebih
	// kecil dinaikkan ke sini supaya loop tidak membebani database.
	minTick = time.Second

	// triggerScheduled adalah nilai trigger yang diteruskan ke SyncRunner.
	triggerScheduled = "scheduled"
)

// SyncRunner adalah bagian *syncjob.Syncer yang dipakai penjadwal. Antarmuka
// sempit ini membuat penjadwal mudah diuji dengan stub.
type SyncRunner interface {
	Run(ctx context.Context, trigger string) (syncjob.Result, error)
}

// Config mengatur Scheduler.
type Config struct {
	// Store adalah database aplikasi, dipakai membaca app_settings; wajib.
	Store *store.Store
	// Syncer adalah pelaksana sinkronisasi. Bila nil penjadwal hanya tidur:
	// jadwal tetap dievaluasi tetapi tidak ada sync yang dipicu.
	Syncer SyncRunner
	// Logger menerima log penjadwal. Bila nil dipakai slog.Default().
	Logger *slog.Logger
	// Now memasok waktu sekarang. Bila nil dipakai time.Now.
	Now func() time.Time
	// Location adalah zona waktu evaluasi jadwal. Bila nil dipakai time.Local.
	Location *time.Location
	// Tick adalah periode pemeriksaan jadwal. Bila tidak positif dipakai 30
	// detik, dan nilai di bawah 1 detik dinaikkan menjadi 1 detik.
	Tick time.Duration
}

// Scheduler memeriksa jadwal cron secara berkala dan memicu SyncRunner.
//
// Scheduler aman dipakai dari satu goroutine pemanggil Run; status internalnya
// (menit terakhir yang dipicu dan flag run aktif) dilindungi mutex serta
// atomic.Bool sehingga goroutine sync tidak berlomba dengan loop.
type Scheduler struct {
	store  *store.Store
	syncer SyncRunner
	log    *slog.Logger
	nowFn  func() time.Time
	loc    *time.Location
	tick   time.Duration

	// running bernilai true selama satu sync hasil jadwal belum selesai.
	running atomic.Bool

	// mu melindungi lastMinute dan triggered.
	mu sync.Mutex
	// lastMinute adalah awal menit (pada s.loc) yang terakhir dipicu.
	lastMinute time.Time
	// triggered bernilai true bila lastMinute sudah pernah diisi.
	triggered bool
}

// New menyiapkan Scheduler dari cfg dan menerapkan nilai bawaan.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Store == nil {
		return nil, errors.New("scheduler: Config.Store wajib diisi")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}

	tick := cfg.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	if tick < minTick {
		tick = minTick
	}

	return &Scheduler{
		store:  cfg.Store,
		syncer: cfg.Syncer,
		log:    logger,
		nowFn:  nowFn,
		loc:    loc,
		tick:   tick,
	}, nil
}

// Run memblokir sampai ctx selesai. Pada setiap Tick ia membaca app_settings
// lalu memicu satu sync bila jadwal aktif, cocok dengan menit sekarang, dan
// menit itu belum pernah dipicu.
//
// Kegagalan membaca database maupun ekspresi cron yang rusak hanya dicatat
// sebagai log Warn; loop tetap berjalan. Run mengembalikan nil saat ctx
// dibatalkan (shutdown normal).
func (s *Scheduler) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.log.Info("scheduler mulai", "tick", s.tick.String(), "zona", s.loc.String())

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler berhenti")
			return nil
		case <-ticker.C:
			s.evaluate(ctx)
		}
	}
}

// evaluate menjalankan satu siklus pemeriksaan jadwal.
func (s *Scheduler) evaluate(ctx context.Context) {
	now := s.nowFn()

	enabled, expr, err := s.settings(ctx)
	if err != nil {
		s.log.Warn("scheduler: gagal membaca app_settings", "error", err)
		return
	}
	if !enabled {
		return
	}

	sched, err := Parse(expr)
	if err != nil {
		s.log.Warn("scheduler: ekspresi cron tidak valid, jadwal dilewati", "cron", expr, "error", err)
		return
	}

	minutes := sched.withLocation(s.loc)
	if !minutes.Matches(now) {
		return
	}

	minute := now.In(s.loc).Truncate(time.Minute)
	if s.alreadyTriggered(minute) {
		return
	}

	if s.syncer == nil {
		s.log.Debug("scheduler: SyncRunner nil, sync terjadwal dilewati", "menit", minute.Format(time.RFC3339))
		return
	}

	if !s.running.CompareAndSwap(false, true) {
		s.log.Warn("scheduler: sync sebelumnya masih berjalan, pemicu dilewati",
			"menit", minute.Format(time.RFC3339))
		return
	}

	// Menit dicatat sebelum goroutine dijalankan supaya tick berikutnya pada
	// menit yang sama tidak memicu sync kedua.
	s.markTriggered(minute)
	go s.runScheduled(minute)
}

// runScheduled menjalankan satu sync terjadwal dan selalu melepas flag running.
func (s *Scheduler) runScheduled(minute time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("scheduler: sync terjadwal panic",
				"panik", r, "menit", minute.Format(time.RFC3339))
		}
		s.running.Store(false)
	}()

	started := time.Now()
	res, err := s.syncer.Run(context.Background(), triggerScheduled)

	switch {
	case errors.Is(err, syncjob.ErrAlreadyRunning):
		s.log.Warn("scheduler: sync terjadwal dilewati, run lain masih berjalan", "error", err)
	case err != nil:
		s.log.Error("scheduler: sync terjadwal gagal",
			"error", err, "menit", minute.Format(time.RFC3339), "durasi", time.Since(started).String())
	default:
		s.log.Info("scheduler: sync terjadwal selesai",
			"run_id", res.RunID,
			"generation", res.GenerationLabel,
			"menit", minute.Format(time.RFC3339),
			"durasi", time.Since(started).String(),
		)
	}
}

// settings membaca pasangan setting yang dipakai penjadwal.
func (s *Scheduler) settings(ctx context.Context) (bool, string, error) {
	raw, ok, err := s.setting(ctx, settingEnabled)
	if err != nil {
		return false, "", err
	}

	enabled := ok && isTrue(raw)

	expr, exprOK, err := s.setting(ctx, settingCron)
	if err != nil {
		return false, "", err
	}
	if !exprOK {
		expr = ""
	}

	return enabled, expr, nil
}

// readDB mengembalikan pool baca Store. Store yang sudah ditutup dilaporkan
// sebagai error, bukan nil pointer, supaya loop penjadwal tidak pernah panic.
func (s *Scheduler) readDB() (*sql.DB, error) {
	db := s.store.Read()
	if db == nil {
		return nil, errors.New("scheduler: pool baca database sudah ditutup")
	}
	return db, nil
}

// setting membaca satu nilai app_settings. ok bernilai false bila kuncinya
// tidak ada.
func (s *Scheduler) setting(ctx context.Context, key string) (string, bool, error) {
	db, err := s.readDB()
	if err != nil {
		return "", false, err
	}

	var value string
	err = db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("scheduler: baca setting %q: %w", key, err)
	}
	return strings.TrimSpace(value), true, nil
}

// alreadyTriggered melaporkan apakah menit minute sudah pernah dipicu.
func (s *Scheduler) alreadyTriggered(minute time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.triggered && s.lastMinute.Equal(minute)
}

// markTriggered mencatat minute sebagai menit terakhir yang dipicu.
func (s *Scheduler) markTriggered(minute time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMinute = minute
	s.triggered = true
}

// isTrue melaporkan apakah nilai setting bernilai benar. Nilai yang diterima:
// "1", "true", "yes" dan "on" (tanpa membedakan huruf besar-kecil).
func isTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
