// Package ingest membangun satu generation media dari series yang dipilih
// admin ("selection"), bukan dari seluruh katalog seperti syncjob.
//
// Alur Start:
//
//  1. tolak bila daftar selection kosong atau memuat lebih dari satu provider,
//  2. tolak bila masih ada sync_runs yang belum terminal (ErrAlreadyRunning),
//  3. tolak bila pemakaian disk pada root media sudah mencapai ambang blokir
//     (ErrInsufficientStorage); pemeriksaan ini berjalan sebelum baris database
//     apa pun dibuat, jadi penolakan tidak meninggalkan generation/sync_run,
//  4. sisipkan generations berstatus 'building' dengan label gen-%06d dari
//     MAX(id)+1,
//  5. mulai staging mediafs untuk label tersebut,
//  6. catat sync_runs trigger 'manual', scope 'selection', status 'running',
//  7. kembalikan runID dan lanjutkan sisanya di goroutine.
//
// Pemeriksaan kapasitas hanya melihat satu kali pemakaian disk di awal run
// (lewat Config.DiskUsage). Ini cukup karena staging ditulis di filesystem yang
// sama dengan root media (<root>/.building-<label>) dan Commit menerbitkannya
// dengan rename, bukan salin: tidak ada penyalinan berkas dan tidak ada ruang
// ganda antara staging dan generation final. Karena itu persentase awal adalah
// perkiraan yang aman untuk kebutuhan satu generation, dan satu run yang sudah
// berjalan tidak dihentikan di tengah jalan bila disk kemudian menipis.
//
// Proses di goroutine:
//
//   - ambil seluruh halaman episode setiap series lewat catalog.Source,
//   - untuk setiap episode pilih stream android (fallback ios), ambil master
//     playlist, turunkan ke satu varian (sisi pendek <= 720 dan terbesar),
//     tulis playlist lokal, lalu unduh seluruh segmen dan key ke staging,
//   - simpan providers/titles/episodes/playables/renditions,
//   - catat progres setiap series ke generation_items (status, tahap, jumlah
//     episode, dan persentase) sebagai best effort: kegagalan menulis progres
//     hanya di-log dan tidak pernah menggagalkan run,
//   - unduh poster series ke dalam generation, satu folder dengan medianya
//     (kegagalan poster tidak menggagalkan episode),
//   - bila tidak ada satu pun rendition berstatus 'ready': Abort, generation &
//     run 'failed',
//   - Commit, tukar generation aktif dalam satu transaksi, lalu tandai run
//     'completed' beserta counter-nya. Generation lama dibiarkan berstatus
//     'retired' tanpa dihapus otomatis: penghapusan hanya dilakukan admin.
//
// Semua timestamp disimpan sebagai TEXT ISO-8601 UTC. Seluruh path yang disimpan
// di database (playlist, segmen, poster) relatif terhadap root generation,
// sehingga bisa disajikan lewat /media/<label>/<path>. Nama folder series dan
// episode memakai UUID provider (fallback: slug series dan sNNENN), bukan judul
// yang mudah ditebak, dengan layout:
//
//	<generation>/dramahits/series/<seriesDir>/poster.jpg
//	<generation>/dramahits/series/<seriesDir>/<episodeDir>/<quality>/index.m3u8
//
// Subtitle belum ditangani paket ini.
package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vodlocal/internal/appsettings"
	"vodlocal/internal/catalog"
	"vodlocal/internal/download"
	"vodlocal/internal/hls"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

// Kesalahan yang dikembalikan paket ini.
var (
	// ErrAlreadyRunning dilaporkan Start bila masih ada sync_runs yang belum
	// terminal (queued/running/verifying/switching/cleanup).
	ErrAlreadyRunning = errors.New("ingest: ingest lain masih berjalan")
	// ErrNoSelections dilaporkan Start bila tidak ada series yang dipilih.
	ErrNoSelections = errors.New("ingest: tidak ada series yang dipilih")
	// ErrMixedProviders dilaporkan Start bila selection memuat lebih dari satu
	// provider.
	ErrMixedProviders = errors.New("ingest: provider selection tidak seragam")
	// ErrNoRenditions dilaporkan proses latar bila tidak ada satu pun rendition
	// yang berhasil disiapkan.
	ErrNoRenditions = errors.New("ingest: tidak ada rendition yang siap")
	// ErrInsufficientStorage dilaporkan Start bila pemakaian disk pada root
	// media sudah mencapai Config.StorageBlockPercent. Pesannya menyebut
	// persentase pemakaian dan ambang yang berlaku.
	ErrInsufficientStorage = errors.New("ingest: ruang disk tidak mencukupi")
)

const (
	// providerCode dan providerName adalah provider default untuk selection
	// lama yang tidak mengisi provider.
	providerCode = "dramahits"
	providerName = "Dramahits"

	// generationLabelFormat adalah format label generation mediafs.
	generationLabelFormat = "gen-%06d"
	// playlistFileName adalah nama berkas playlist lokal.
	playlistFileName = "index.m3u8"
	// segmentFilePrefix adalah awalan berkas segmen yang dibuat hls.Rewrite.
	segmentFilePrefix = "seg-"
	// posterFileName adalah nama berkas poster series.
	posterFileName = "poster.jpg"

	// runTrigger dan runScope adalah nilai kolom sync_runs untuk paket ini.
	runTrigger = "manual"
	runScope   = "selection"

	// playlistTimeout membatasi satu permintaan playlist (master maupun varian).
	playlistTimeout = 30 * time.Second
	// maxPlaylistBytes membatasi ukuran playlist yang mau dibaca.
	maxPlaylistBytes = 8 << 20
	// maxEpisodePages membatasi jumlah halaman episode supaya sumber yang salah
	// melaporkan LastPage tidak membuat proses berputar selamanya.
	maxEpisodePages = 500

	// dbTimeout membatasi satu operasi database. Konteksnya tidak ikut
	// dibatalkan bersama run supaya pembukuan status akhir tetap bisa ditulis.
	dbTimeout = 30 * time.Second
	// maxNoteLen membatasi panjang catatan/error yang ditulis ke database.
	maxNoteLen = 500
)

// Config mengatur Service.
type Config struct {
	// Store adalah database aplikasi; wajib.
	Store *store.Store
	// Media mengelola direktori generation; wajib.
	Media *mediafs.Manager
	// Downloader dipakai untuk segmen dan poster. Bila nil dibuat satu dengan
	// opsi default.
	Downloader *download.Downloader
	// Source adalah provider tunggal (kompatibilitas lama). Bila Sources
	// diisi, Source diabaikan.
	Source catalog.Source
	// Sources adalah kumpulan provider berdasarkan kategori; wajib bila Source
	// kosong.
	Sources catalog.SourceSet
	// Logger menerima log progres. Bila nil dipakai slog.Default().
	Logger *slog.Logger
	// Now memasok waktu sekarang. Bila nil dipakai time.Now.
	Now func() time.Time
	// ArtworkRoot dipertahankan demi kompatibilitas pemanggil lama dan tidak
	// lagi dipakai: poster ingest ditulis ke dalam generation, satu folder
	// dengan media series-nya
	// (<generation>/dramahits/series/<seriesDir>/poster.jpg), sehingga
	// titles.poster_path relatif terhadap root generation dan bisa disajikan
	// lewat /media/<label>/<path>.
	ArtworkRoot string
	// MaxEpisodes membatasi jumlah episode yang diproses per series pada satu
	// run. Nilai 0 (default) berarti tanpa batas; nilai negatif diperlakukan
	// seperti 0. Opsi ini disediakan untuk smoke test lokal, dan episode yang
	// dibatasi ikut memengaruhi generation_items.total_episodes supaya progres
	// series tersebut tetap bisa mencapai 100%.
	MaxEpisodes int
	// DiskUsage melaporkan byte terpakai dan kapasitas total untuk path.
	//
	// Start memanggilnya dengan root media (mediafs.Manager.Root()) sebelum
	// membuat generation, sehingga path yang diukur adalah filesystem yang sama
	// dengan tempat staging dan generation ditulis. Bila nil, atau bila
	// StorageBlockPercent 0, pemeriksaan kapasitas dilewati. Error dari fungsi
	// ini tidak menggagalkan Start: pemeriksaan dilewati dan alasannya di-log
	// sebagai Warn, karena kehabisan ruang pada akhirnya tetap terdeteksi
	// sebagai kegagalan tulis/unduh.
	DiskUsage func(path string) (used, total int64, err error)
	// StorageBlockPercent menolak Start bila persentase pemakaian >= nilai ini.
	// 0 berarti pemeriksaan dilewati.
	//
	// Persentase dihitung sebagai used*100/total dan dibandingkan dengan angka
	// bulat (pembulatan ke bawah). Bila DiskUsage melaporkan total <= 0,
	// pemeriksaan juga dilewati.
	StorageBlockPercent int
}

// Service adalah worker yang memproses series terpilih menjadi satu generation
// media. Service aman dipakai dari beberapa goroutine.
type Service struct {
	store   *store.Store
	media   *mediafs.Manager
	dl      *download.Downloader
	sources catalog.SourceSet
	log     *slog.Logger
	nowFn   func() time.Time
	client  *http.Client

	// maxEpisodes adalah batas jumlah episode per series (0 = tanpa batas).
	maxEpisodes int

	// diskUsage dan storageBlockPercent adalah pemeriksaan kapasitas disk yang
	// dijalankan Start sebelum membuat generation. diskUsage nil atau
	// storageBlockPercent <= 0 berarti pemeriksaan dilewati.
	diskUsage           func(path string) (used, total int64, err error)
	storageBlockPercent int

	// startMu menyerialkan fase penyiapan Start (cek run aktif, buat generation,
	// mulai staging, catat sync_runs) sehingga dua Start bersamaan tidak pernah
	// berebut label generation yang sama. Unique index di database tetap menjadi
	// penjaga terakhir untuk proses lain.
	startMu sync.Mutex
}

// New menyiapkan Service dari cfg.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("ingest: Config.Store wajib diisi")
	}
	if cfg.Media == nil {
		return nil, errors.New("ingest: Config.Media wajib diisi")
	}
	if cfg.Source == nil && len(cfg.Sources) == 0 {
		return nil, errors.New("ingest: Config.Source atau Config.Sources wajib diisi")
	}

	sources := cfg.Sources
	if len(sources) == 0 {
		sources = catalog.SourceSet{cfg.Source}
	}

	client := &http.Client{Timeout: playlistTimeout}

	dl := cfg.Downloader
	if dl == nil {
		dl = download.New(download.Options{Client: client, Retries: 0})
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	maxEpisodes := cfg.MaxEpisodes
	if maxEpisodes < 0 {
		maxEpisodes = 0
	}

	// Ambang negatif diperlakukan seperti 0 (pemeriksaan dilewati) supaya
	// Config yang berasal dari sumber lain tidak bisa memblokir semua run.
	blockPercent := cfg.StorageBlockPercent
	if blockPercent < 0 {
		blockPercent = 0
	}

	return &Service{
		store:               cfg.Store,
		media:               cfg.Media,
		dl:                  dl,
		sources:             sources,
		log:                 logger,
		nowFn:               nowFn,
		client:              client,
		maxEpisodes:         maxEpisodes,
		diskUsage:           cfg.DiskUsage,
		storageBlockPercent: blockPercent,
	}, nil
}

// Start memulai satu ingest untuk series yang dipilih dan langsung
// mengembalikan id baris sync_runs. Sisa pekerjaan berjalan di goroutine yang
// status terminalnya bisa dipantau lewat tabel sync_runs.
//
// ctx dipakai selama run berjalan: pembatalannya menghentikan unduhan dan
// menandai generation serta run sebagai 'failed'.
//
// Error yang dikembalikan membungkus ErrNoSelections, ErrMixedProviders,
// ErrAlreadyRunning, ErrInsufficientStorage atau kegagalan database/staging.
func (s *Service) Start(ctx context.Context, selections []catalog.Series) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.validateSelections(selections); err != nil {
		return 0, err
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	// Langkah 1: tolak bila sudah ada run yang belum terminal.
	activeID, err := s.activeRunID(ctx)
	if err != nil {
		return 0, err
	}
	if activeID != 0 {
		return 0, fmt.Errorf("%w (run %d)", ErrAlreadyRunning, activeID)
	}

	// Langkah 2: tolak bila disk media sudah hampir penuh. Pemeriksaan ini
	// sengaja dijalankan sebelum baris database apa pun dibuat, jadi penolakan
	// tidak meninggalkan generation 'building' atau sync_runs yatim.
	if err := s.checkStorageCapacity(); err != nil {
		return 0, err
	}

	// Langkah 3: label generation berikutnya + baris generations 'building'.
	genID, label, err := s.createGeneration(ctx)
	if err != nil {
		return 0, err
	}

	// Langkah 4: staging mediafs.
	build, err := s.media.NewStaging(label)
	if err != nil {
		s.markGenerationFailed(ctx, genID, fmt.Sprintf("staging %s: %v", label, err))
		return 0, fmt.Errorf("ingest: mulai staging %s: %w", label, err)
	}

	// Langkah 5: baris sync_runs 'running'.
	runID, err := s.insertRun(ctx, genID)
	if err != nil {
		if abortErr := build.Abort(); abortErr != nil {
			s.log.Warn("gagal membuang staging", "generation", label, "error", abortErr)
		}
		s.markGenerationFailed(ctx, genID, err.Error())
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
		}
		return 0, err
	}

	st := &runState{
		runID:        runID,
		genID:        genID,
		label:        label,
		build:        build,
		dir:          build.Dir(),
		selections:   selections,
		maxShortSide: appsettings.MaxShortSide(ctx, s.store.Read()),
	}

	s.log.Info("ingest dimulai",
		"run_id", st.runID, "generation", st.label,
		"series", len(st.selections), "max_short_side", st.maxShortSide)

	// Langkah 6: jalankan sisanya di latar dan kembalikan runID segera.
	// Context request dibatalkan begitu handler selesai, jadi pembatalannya
	// dilepas agar worker tetap berjalan sampai selesai.
	runCtx := context.WithoutCancel(ctx)
	go s.run(runCtx, st)

	return runID, nil
}

// checkStorageCapacity menolak start bila pemakaian disk pada root media sudah
// mencapai ambang blokir. Pemeriksaan dilewati bila Config.DiskUsage nil,
// Config.StorageBlockPercent <= 0, kapasitas total tidak masuk akal, atau
// pembacaan pemakaian gagal (kegagalan hanya di-log sebagai Warn).
func (s *Service) checkStorageCapacity() error {
	if s.diskUsage == nil || s.storageBlockPercent <= 0 {
		return nil
	}

	root := s.media.Root()
	used, total, err := s.diskUsage(root)
	if err != nil {
		s.log.Warn("pemakaian disk tidak terbaca, pemeriksaan kapasitas dilewati",
			"path", root, "error", err)
		return nil
	}
	if total <= 0 {
		s.log.Warn("kapasitas disk tidak dilaporkan, pemeriksaan kapasitas dilewati",
			"path", root, "used", used, "total", total)
		return nil
	}

	// Kapasitas disk nyata jauh di bawah 92 EB, jadi used*100 aman dari luapan
	// int64. Pembagian integer membulatkan ke bawah; ambang 90 berarti blokir
	// mulai 90,0%.
	percent := int(used * 100 / total)
	if percent < s.storageBlockPercent {
		return nil
	}

	s.log.Warn("ingest ditolak karena ruang disk hampir habis",
		"path", root,
		"used_bytes", used,
		"total_bytes", total,
		"percent", percent,
		"threshold_percent", s.storageBlockPercent,
	)
	return fmt.Errorf("%w: pemakaian disk %d%% sudah mencapai ambang %d%% (path %s)",
		ErrInsufficientStorage, percent, s.storageBlockPercent, root)
}

// validateSelections memastikan daftar selection tidak kosong, setiap kategori
// dikenal, source-nya tersedia, dan movie membawa stream_url.
func (s *Service) validateSelections(selections []catalog.Series) error {
	if len(selections) == 0 {
		return ErrNoSelections
	}

	for _, sel := range selections {
		code := normalizeProvider(sel.Provider)
		category := normalizeCategory(sel.Category)

		switch category {
		case catalog.CategoryShort, catalog.CategoryMovie, catalog.CategorySeries:
		default:
			return fmt.Errorf("ingest: kategori %q tidak dikenal untuk %q", category, firstNonEmpty(sel.Slug, sel.ID))
		}

		if _, ok := s.sources.For(code, category); !ok {
			return fmt.Errorf("ingest: source provider %q kategori %q tidak tersedia", code, category)
		}
		if category == catalog.CategoryMovie && strings.TrimSpace(sel.StreamURL) == "" {
			return fmt.Errorf("ingest: movie %q tidak memiliki stream_url", firstNonEmpty(sel.Slug, sel.ID))
		}
	}
	return nil
}

// runState adalah keadaan satu ingest selama berjalan.
type runState struct {
	runID      int64
	genID      int64
	label      string
	build      *mediafs.Build
	dir        string
	selections []catalog.Series
	// maxShortSide adalah snapshot setting untuk seluruh run.
	maxShortSide int

	titles     int
	playables  int
	renditions int
	ready      int
	failed     int
	bytes      int64

	// item adalah pelapor progres series yang sedang diproses. Dipakai
	// buildRendition untuk memperbarui current_step tanpa mengubah signature
	// fungsi tersebut; nil di luar processSeries.
	item *seriesProgress
}

// stagingRoot adalah direktori staging generation ini, tempat seluruh berkas
// media (playlist, segmen, poster) ditulis sebelum Commit. Setelah Commit path
// ini tidak ada lagi; pembacaan sesudahnya harus lewat mediafs.Manager.
func (r *runState) stagingRoot() string {
	if r.build != nil {
		return r.build.Dir()
	}
	return r.dir
}

// run menjalankan seluruh proses di goroutine. Panic dari proses (termasuk dari
// implementasi catalog.Source) diubah menjadi kegagalan run, tidak pernah
// dijatuhkan ke proses utama.
func (s *Service) run(ctx context.Context, st *runState) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("ingest panic", "run_id", st.runID, "panic", r)
			s.fail(ctx, st, fmt.Errorf("ingest: panic: %v", r))
		}
	}()

	_ = s.execute(ctx, st)
}

// execute menjalankan sisa alur setelah baris database dan staging siap.
func (s *Service) execute(ctx context.Context, st *runState) error {
	provIDs := make(map[string]int64)
	for _, series := range st.selections {
		code := normalizeProvider(series.Provider)
		if _, ok := provIDs[code]; ok {
			continue
		}
		id, err := s.upsertProvider(ctx, code)
		if err != nil {
			return s.fail(ctx, st, err)
		}
		provIDs[code] = id
	}

	// Satu baris generation_items per selection, idempotent dan best effort:
	// kegagalan di sini tidak menggagalkan run.
	s.seedGenerationItems(ctx, st, provIDs)

	for _, series := range st.selections {
		if err := ctx.Err(); err != nil {
			return s.fail(ctx, st, err)
		}
		code := normalizeProvider(series.Provider)
		if err := s.processSeries(ctx, st, provIDs[code], series); err != nil {
			return s.fail(ctx, st, err)
		}
	}

	// Tidak ada satu pun rendition siap berarti generation ini tidak layak
	// tayang.
	if st.ready == 0 {
		return s.fail(ctx, st, fmt.Errorf("%w: 0 dari %d rendition", ErrNoRenditions, st.renditions))
	}

	if err := st.build.Commit(); err != nil {
		return s.fail(ctx, st, fmt.Errorf("ingest: commit generation %s: %w", st.label, err))
	}

	if err := s.switchGeneration(ctx, st); err != nil {
		return s.fail(ctx, st, err)
	}
	if n, err := s.store.BackfillGenerationItems(ctx); err != nil {
		s.log.Warn("gagal backfill generation_items", "error", err)
	} else if n > 0 {
		s.log.Info("backfill generation_items", "rows", n)
	}

	// Generation lama dibiarkan 'retired' di disk: paket ini tidak pernah
	// menghapusnya otomatis, penghapusan hanya lewat panel admin. Yang
	// dibersihkan di sini hanya katalog yang medianya sudah tidak ada.
	s.cleanupOrphans(ctx)

	if err := s.finishRun(ctx, st, "completed", nil); err != nil {
		s.log.Warn("gagal menutup sync run", "run_id", st.runID, "error", err)
		return err
	}

	s.log.Info("ingest selesai",
		"run_id", st.runID,
		"generation", st.label,
		"series", len(st.selections),
		"titles", st.titles,
		"playables", st.playables,
		"ready", st.ready,
		"failed", st.failed,
		"bytes", st.bytes,
	)
	return nil
}

// fail menutup run yang gagal: staging dibuang, generation dan run ditandai.
// Pembukuan memakai konteks turunan yang tidak ikut dibatalkan supaya status
// akhir tetap tercatat walau ctx sudah selesai.
func (s *Service) fail(ctx context.Context, st *runState, cause error) error {
	committed := st.build != nil && st.build.Closed()
	if st.build != nil && !committed {
		if err := st.build.Abort(); err != nil {
			s.log.Warn("gagal membuang staging", "generation", st.label, "error", err)
		}
	}

	if marked := s.markGenerationFailed(ctx, st.genID, cause.Error()); committed && marked {
		// Staging sudah terbit tetapi proses sesudahnya gagal: buang direktori
		// generation-nya supaya tidak ada generation 'failed' yang menempati disk.
		if err := s.media.Remove(st.label); err != nil && !errors.Is(err, mediafs.ErrNotFound) {
			s.log.Warn("gagal menghapus direktori generation gagal",
				"generation", st.label, "error", err)
		}
	}

	if err := s.finishRun(ctx, st, "failed", cause); err != nil {
		s.log.Warn("gagal mencatat akhir run", "run_id", st.runID, "error", err)
	}

	s.log.Error("ingest gagal",
		"run_id", st.runID,
		"generation", st.label,
		"ready", st.ready,
		"failed", st.failed,
		"error", cause,
	)
	return cause
}

// processSeries menyimpan satu series beserta seluruh episodenya sekaligus
// melaporkan progresnya ke generation_items.
//
// Kegagalan fatal dikembalikan dan baris generation_items series ini ditandai
// 'failed' lewat defer. Series yang selesai tanpa satu pun episode berhasil
// juga ditandai 'failed' karena tidak menghasilkan rendition yang bisa dipakai.
func (s *Service) processSeries(ctx context.Context, st *runState, provID int64, series catalog.Series) (err error) {
	provider := normalizeProvider(series.Provider)
	category := normalizeCategory(series.Category)
	src, ok := s.sources.For(provider, category)
	if !ok {
		return fmt.Errorf("ingest: source provider %q kategori %q tidak tersedia", provider, category)
	}
	kind := titleKind(category)

	// slug dipakai untuk kolom katalog (titles.slug), kunci episode, dan log.
	slug := safePathComponent(firstNonEmpty(series.Slug, series.ID))

	// Folder media memakai UUID provider supaya path tidak bisa ditebak dari
	// judul/slug; slug hanya fallback bila ID kosong.
	seriesDir := safePathComponent(firstNonEmpty(series.ID, series.Slug))

	pr := s.newSeriesProgress(st, provID, series)
	st.item = pr
	defer func() {
		st.item = nil
		if err != nil {
			pr.fail(ctx, err)
		}
	}()

	pr.start(ctx)

	// Poster series hanya diunduh sekali; kegagalannya tidak menggagalkan
	// episode mana pun.
	if strings.TrimSpace(series.PosterURL) != "" {
		pr.step(ctx, stepPoster)
	}
	posterPath := s.fetchPoster(ctx, st, provider, category, seriesDir, series.PosterURL)

	titleID, err := s.upsertTitle(ctx, provID, series, slug, posterPath, kind)
	if err != nil {
		return err
	}
	st.titles++

	pr.step(ctx, stepEpisodes)

	// Movie tidak punya endpoint episode: stream_url dari daftar langsung
	// dipakai sebagai satu playable.
	if category == catalog.CategoryMovie {
		pr.setTotal(ctx, 1)
		ep := catalog.Episode{
			ID:      series.ID,
			Slug:    series.Slug,
			Title:   series.Title,
			Season:  1,
			Number:  1,
			Streams: []catalog.Stream{{Device: "android", URL: series.StreamURL}},
		}
		pr.episodeStart(ctx, 1, 1)
		res, epErr := s.processEpisode(ctx, st, titleID, provider, category, seriesDir, ep, 0, true)
		if epErr != nil {
			return epErr
		}
		pr.episodeDone(ctx, res)
		if res.ready {
			pr.finish(ctx)
		} else {
			pr.fail(ctx, fmt.Errorf("ingest: movie %s gagal disiapkan", slug))
		}
		return nil
	}

	episodes, err := s.listEpisodes(ctx, src, firstNonEmpty(series.Slug, series.ID))
	if err != nil {
		return err
	}

	// MaxEpisodes hanya untuk smoke test lokal. total_episodes melaporkan
	// jumlah episode yang benar-benar diproses supaya progres bisa 100%.
	if s.maxEpisodes > 0 && len(episodes) > s.maxEpisodes {
		s.log.Info("jumlah episode dibatasi MaxEpisodes",
			"series", slug, "episodes", len(episodes), "max", s.maxEpisodes)
		episodes = episodes[:s.maxEpisodes]
	}

	pr.setTotal(ctx, len(episodes))

	s.log.Info("series diproses",
		"provider", provider, "category", category,
		"series", slug, "episodes", len(episodes), "poster", posterPath != "")

	for i, ep := range episodes {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		season, number := episodeCoordinates(ep, i)
		pr.episodeStart(ctx, season, number)

		res, epErr := s.processEpisode(ctx, st, titleID, provider, category, seriesDir, ep, i, false)
		if epErr != nil {
			return epErr
		}
		pr.episodeDone(ctx, res)
	}

	// Kegagalan di tingkat series hanya menandai baris generation_items series
	// ini; run tetap lanjut ke series berikutnya dan status akhir run tetap
	// ditentukan oleh aturan lama (tidak ada rendition siap = gagal).
	switch {
	case pr.total == 0:
		pr.fail(ctx, fmt.Errorf("ingest: series %s tidak punya episode", slug))
	case pr.done == 0:
		pr.fail(ctx, fmt.Errorf("ingest: semua %d episode series %s gagal", pr.failed, slug))
	default:
		pr.finish(ctx)
	}
	return nil
}

// listEpisodes mengambil seluruh halaman episode satu series dari source yang
// sudah dipilih.
func (s *Service) listEpisodes(ctx context.Context, src catalog.Source, seriesSlug string) ([]catalog.Episode, error) {
	var out []catalog.Episode

	for page := 1; page <= maxEpisodePages; page++ {
		res, err := src.ListEpisodes(ctx, seriesSlug, page)
		if err != nil {
			return nil, fmt.Errorf("ingest: ListEpisodes(%s) halaman %d: %w", seriesSlug, page, err)
		}
		out = append(out, res.Items...)

		last := res.LastPage
		if last <= 0 {
			// Sumber tidak melaporkan jumlah halaman: satu halaman dianggap
			// seluruhnya.
			last = page
		}
		if page >= last {
			break
		}
		if len(res.Items) == 0 {
			// Pengaman: halaman kosong yang masih mengaku punya lanjutan.
			s.log.Warn("halaman episode kosong, pengambilan dihentikan",
				"series", seriesSlug, "page", page, "last_page", last)
			break
		}
	}

	return out, nil
}

// episodeCoordinates mengembalikan nomor season dan episode yang dipakai untuk
// path media serta penanda progres. Nilai yang tidak masuk akal diganti dengan
// nilai turunan dari urutan episode.
func episodeCoordinates(ep catalog.Episode, index int) (season, number int) {
	season = ep.Season
	if season <= 0 {
		season = 1
	}
	number = ep.Number
	if number <= 0 {
		number = index + 1
	}
	return season, number
}

// episodeDirectory mengembalikan nama folder satu episode di dalam generation:
// UUID episode dari provider, atau sNNENN bila ID kosong.
func episodeDirectory(ep catalog.Episode, season, number int) string {
	return safePathComponent(firstNonEmpty(ep.ID, fmt.Sprintf("s%02de%02d", season, number)))
}

// processEpisode menyiapkan satu episode lalu menyimpannya ke database. Hanya
// kegagalan fatal (context dibatalkan, database, atau filesystem) yang
// dikembalikan; kegagalan playlist/unduhan dicatat sebagai rendition gagal
// lewat episodeResult.
func (s *Service) processEpisode(ctx context.Context, st *runState, titleID int64, provider, category, seriesDir string, ep catalog.Episode, index int, movie bool) (episodeResult, error) {
	episodeName := firstNonEmpty(ep.Slug, ep.ID, fmt.Sprintf("ep-%d", index+1))
	if movie {
		episodeName = firstNonEmpty(ep.Slug, ep.ID, "movie")
	}

	stream, ok := pickStream(ep.Streams)
	if !ok {
		s.log.Warn("episode tanpa stream android/ios dilewati",
			"series", seriesDir, "episode", episodeName)
		return episodeResult{}, nil
	}

	season, number := episodeCoordinates(ep, index)
	episodeDir := ""
	if !movie {
		episodeDir = episodeDirectory(ep, season, number)
	}

	// Selisih st.bytes dipakai sebagai jumlah byte episode ini, termasuk
	// unduhan yang berakhir gagal di tengah jalan.
	bytesBefore := st.bytes

	rec, err := s.buildRendition(ctx, st, provider, category, seriesDir, episodeDir, stream.URL)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return episodeResult{}, cerr
		}
		st.renditions++
		st.failed++
		s.log.Warn("episode gagal disiapkan",
			"series", seriesDir, "episode", episodeName, "error", err)
		return episodeResult{attempted: true, bytes: st.bytes - bytesBefore}, nil
	}

	durationSeconds := ep.DurationSeconds
	if durationSeconds <= 0 {
		durationSeconds = rec.duration
	}

	var playableID int64
	if movie {
		playableID, err = s.upsertPlayable(ctx, titleID, nil, durationSeconds, rec.encrypted)
	} else {
		episodeID, episodeErr := s.upsertEpisode(ctx, titleID, ep, season, number)
		if episodeErr != nil {
			return episodeResult{}, episodeErr
		}
		playableID, err = s.upsertPlayable(ctx, titleID, &episodeID, durationSeconds, rec.encrypted)
	}
	if err != nil {
		return episodeResult{}, err
	}
	if _, err := s.upsertRendition(ctx, st, playableID, rec); err != nil {
		return episodeResult{}, err
	}

	res := episodeResult{
		attempted: true,
		ready:     rec.status == "ready",
		bytes:     st.bytes - bytesBefore,
	}

	st.playables++
	st.renditions++
	if res.ready {
		st.ready++
		s.log.Info("episode siap",
			"series", seriesDir, "episode", episodeName,
			"quality", rec.quality, "segments", rec.segmentCount, "bytes", rec.bytesDone)
	} else {
		st.failed++
		s.log.Warn("episode tersimpan tetapi rendition gagal",
			"series", seriesDir, "episode", episodeName,
			"quality", rec.quality, "error", rec.lastError)
	}
	return res, nil
}

// renditionBuild adalah hasil penyiapan satu rendition di staging, siap ditulis
// sebagai baris renditions.
type renditionBuild struct {
	quality      string
	width        int
	height       int
	bitrateKbps  int
	sourceURL    string
	relPlaylist  string
	segmentCount int
	segmentsDone int
	totalBytes   int64
	bytesDone    int64
	duration     int
	encrypted    bool
	status       string
	lastError    string
	verifiedAt   string
}

// buildRendition mengubah satu URL stream menjadi playlist lokal beserta
// segmennya di dalam staging, lalu memverifikasi hasilnya. seriesDir dan
// episodeDir adalah komponen path folder series/episode (UUID provider atau
// fallback-nya).
func (s *Service) buildRendition(ctx context.Context, st *runState, provider, category, seriesDir, episodeDir, streamURL string) (*renditionBuild, error) {
	media, finalURL, variant, variantURL, err := s.fetchMediaPlaylist(ctx, streamURL, st.maxShortSide)
	if err != nil {
		return nil, err
	}

	quality := qualityLabel(variant.Width, variant.Height, variant.Bandwidth)
	relDir := path.Join(provider, category, seriesDir, episodeDir, quality)
	relPlaylist := path.Join(relDir, playlistFileName)
	dirAbs := filepath.Join(st.stagingRoot(), filepath.FromSlash(relDir))

	out, assets, err := hls.Rewrite(media, hls.RewriteOptions{})
	if err != nil {
		return nil, fmt.Errorf("ingest: rewrite %s: %w", variantURL, err)
	}

	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		return nil, fmt.Errorf("ingest: buat direktori rendition %s: %w", relDir, err)
	}
	if err := os.WriteFile(filepath.Join(dirAbs, playlistFileName), out, 0o644); err != nil {
		return nil, fmt.Errorf("ingest: tulis playlist %s: %w", relPlaylist, err)
	}

	rec := &renditionBuild{
		quality:      quality,
		width:        variant.Width,
		height:       variant.Height,
		bitrateKbps:  variant.Bandwidth / 1000,
		sourceURL:    variantURL,
		relPlaylist:  relPlaylist,
		segmentCount: len(media.Segments),
		status:       "ready",
	}

	var totalDuration float64
	for _, seg := range media.Segments {
		totalDuration += seg.Duration
		if seg.Key != nil && seg.Key.Method != "" && !strings.EqualFold(seg.Key.Method, "NONE") {
			rec.encrypted = true
		}
	}
	rec.duration = int(math.Round(totalDuration))

	jobs, err := s.jobsFor(dirAbs, finalURL, assets)
	if err != nil {
		return nil, err
	}

	if len(jobs) > 0 {
		// Tahap unduhan segmen; progres per segmen dilaporkan lewat callback
		// downloader dan di-throttle oleh seriesProgress.
		s.itemStep(ctx, st, stepSegments)

		var onProgress func(done, total int, bytes int64)
		if st.item != nil {
			onProgress = func(done, total int, bytes int64) {
				st.item.segmentProgress(ctx, done, total, bytes)
			}
		}
		res, dlErr := s.dl.DownloadWithProgress(ctx, jobs, onProgress)
		st.bytes += res.Bytes
		rec.bytesDone = res.Bytes
		if dlErr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			rec.status = "failed"
			rec.lastError = truncate(fmt.Sprintf("%d dari %d berkas gagal: %v", res.Failed, res.Total, dlErr), maxNoteLen)
		}
	}

	if onDisk, err := dirBytes(dirAbs); err == nil {
		rec.totalBytes = onDisk
		if rec.status == "ready" {
			rec.bytesDone = onDisk
		}
	} else {
		rec.totalBytes = rec.bytesDone
		s.log.Warn("gagal menghitung isi direktori rendition", "dir", relDir, "error", err)
	}

	// Verifikasi: playlist harus ada dan jumlah berkas segmen di disk harus
	// sesuai dengan yang dideklarasikan playlist.
	segmentsOnDisk := countSegmentFiles(dirAbs)
	if rec.status == "ready" && segmentsOnDisk != rec.segmentCount {
		rec.status = "failed"
		rec.lastError = fmt.Sprintf("jumlah segmen %d, seharusnya %d", segmentsOnDisk, rec.segmentCount)
	}
	rec.segmentsDone = segmentsOnDisk
	if rec.status == "ready" {
		rec.verifiedAt = s.stamp()
	}

	// Rendition yang gagal tidak boleh meninggalkan berkas parsial di staging:
	// selain mengotori generation, sisa segmen tetap memakan ruang saat disk
	// penuh. Baris database tetap disimpan agar error bisa dilihat di UI.
	if rec.status == "failed" {
		if err := os.RemoveAll(dirAbs); err != nil {
			s.log.Warn("gagal membersihkan berkas rendition gagal",
				"dir", relDir, "error", err)
		} else {
			rec.totalBytes = 0
			rec.bytesDone = 0
			rec.segmentsDone = 0
		}
	}

	return rec, nil
}

// fetchMediaPlaylist mengambil playlist dari streamURL dan menurunkannya ke
// satu media playlist. Bila provider langsung memberi media playlist (bukan
// master), playlist itu dipakai apa adanya. Nilai balik keempat adalah URL
// varian yang dipakai sebagai renditions.source_url.
func (s *Service) fetchMediaPlaylist(ctx context.Context, streamURL string, maxShortSide int) (*hls.Playlist, string, hls.Variant, string, error) {
	pl, finalURL, err := s.getPlaylist(ctx, streamURL)
	if err != nil {
		return nil, "", hls.Variant{}, "", err
	}
	if !pl.IsMaster {
		return pl, finalURL, hls.Variant{}, finalURL, nil
	}
	if len(pl.Variants) == 0 {
		return nil, "", hls.Variant{}, "", fmt.Errorf("ingest: master playlist %s tanpa varian", streamURL)
	}

	variant, ok := hls.SelectVariant(pl.Variants, maxShortSide)
	if !ok {
		return nil, "", hls.Variant{}, "", fmt.Errorf("ingest: master playlist %s tanpa variant ber-URI", streamURL)
	}

	variantURL, err := resolveURL(finalURL, variant.URI)
	if err != nil {
		return nil, "", hls.Variant{}, "", fmt.Errorf("ingest: resolusi variant %q: %w", variant.URI, err)
	}

	media, mediaURL, err := s.getPlaylist(ctx, variantURL)
	if err != nil {
		return nil, "", hls.Variant{}, "", err
	}
	if media.IsMaster {
		return nil, "", hls.Variant{}, "", fmt.Errorf("ingest: master playlist bersarang di %s", variantURL)
	}
	return media, mediaURL, variant, variantURL, nil
}

// getPlaylist melakukan satu GET playlist dan mem-parse hasilnya. URL final
// (setelah redirect) dikembalikan supaya URI relatif di dalam playlist bisa
// diselesaikan dengan benar.
func (s *Service) getPlaylist(ctx context.Context, rawURL string) (*hls.Playlist, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("ingest: permintaan playlist %q: %w", rawURL, err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("ingest: ambil playlist %q: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("ingest: playlist %s: status %d", rawURL, resp.StatusCode)
	}

	pl, err := hls.Parse(io.LimitReader(resp.Body, maxPlaylistBytes))
	if err != nil {
		return nil, "", fmt.Errorf("ingest: parse playlist %s: %w", rawURL, err)
	}

	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return pl, final, nil
}

// jobsFor menyusun pekerjaan unduhan dari aset playlist. URI `data:` (mis. key
// AES yang tertanam di playlist) dilewati karena tidak perlu diunduh dan sudah
// dipertahankan hls.Rewrite apa adanya.
func (s *Service) jobsFor(dirAbs, baseURL string, assets []hls.AssetRef) ([]download.Job, error) {
	jobs := make([]download.Job, 0, len(assets))
	for _, a := range assets {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.SourceURI)), "data:") {
			continue
		}
		src, err := resolveURL(baseURL, a.SourceURI)
		if err != nil {
			return nil, fmt.Errorf("ingest: resolusi aset %q: %w", a.SourceURI, err)
		}
		jobs = append(jobs, download.Job{URL: src, Dest: filepath.Join(dirAbs, a.LocalName)})
	}
	return jobs, nil
}

// fetchPoster mengunduh poster satu series ke dalam generation yang sedang
// dibangun, satu folder dengan medianya, lalu mengembalikan path relatif
// terhadap root generation (mis. dramahits/series/<seriesDir>/poster.jpg).
// Path itu yang disimpan titles.poster_path, sehingga poster bisa disajikan
// lewat /media/<label>/<path> tanpa ArtworkRoot. Kegagalan unduhan hanya dicatat
// di log dan menghasilkan string kosong, sehingga poster tidak pernah
// menggagalkan episode.
func (s *Service) fetchPoster(ctx context.Context, st *runState, provider, category, seriesDir, posterURL string) string {
	posterURL = strings.TrimSpace(posterURL)
	if posterURL == "" {
		return ""
	}

	rel := path.Join(provider, category, seriesDir, posterFileName)
	dest := filepath.Join(st.stagingRoot(), filepath.FromSlash(rel))

	res, err := s.dl.Download(ctx, []download.Job{{URL: posterURL, Dest: dest}})
	if err != nil {
		s.log.Warn("poster gagal diunduh, dilewati",
			"series", seriesDir, "url", posterURL, "error", err)
		return ""
	}
	st.bytes += res.Bytes
	return rel
}

// upsertProvider memastikan baris providers untuk code ada dan mengembalikan
// id-nya.
func (s *Service) upsertProvider(ctx context.Context, code string) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	code = normalizeProvider(code)
	name := providerDisplayName(code)
	category := providerDefaultCategory(code)
	now := s.stamp()

	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO providers (code, name, category, is_enabled, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?)
ON CONFLICT(code) DO UPDATE SET
    name       = excluded.name,
    is_enabled = 1,
    updated_at = excluded.updated_at
RETURNING id`,
		code, name, category, now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ingest: simpan provider %q: %w", code, err)
	}
	return id, nil
}

// upsertTitle menyimpan satu judul dan mengembalikan id titles. slug mengisi
// kolom titles.slug, posterPath relatif terhadap root generation, dan kind
// adalah movie atau series.
func (s *Service) upsertTitle(ctx context.Context, provID int64, series catalog.Series, slug, posterPath, kind string) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	external := firstNonEmpty(series.Slug, series.ID)
	name := firstNonEmpty(series.Title, external)
	now := s.stamp()

	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO titles (provider_id, external_id, kind, title, slug, synopsis, release_year,
                    content_rating, poster_path, is_published, fetched_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(provider_id, external_id) DO UPDATE SET
    kind           = excluded.kind,
    title          = excluded.title,
    slug           = excluded.slug,
    synopsis       = excluded.synopsis,
    release_year   = excluded.release_year,
    content_rating = excluded.content_rating,
    poster_path    = COALESCE(excluded.poster_path, titles.poster_path),
    is_published   = 1,
    fetched_at     = excluded.fetched_at,
    updated_at     = excluded.updated_at
RETURNING id`,
		provID, external, kind, name, slug, nullString(series.Description),
		nullInt(int64(series.Year)), nullString(series.AgeRating), nullString(posterPath),
		now, now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ingest: simpan title %q: %w", external, err)
	}
	return id, nil
}

// upsertEpisode menyimpan satu episode dan mengembalikan id-nya.
func (s *Service) upsertEpisode(ctx context.Context, titleID int64, ep catalog.Episode, season, number int) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	external := firstNonEmpty(ep.Slug, ep.ID)
	if external == "" {
		external = fmt.Sprintf("s%02de%02d", season, number)
	}
	name := firstNonEmpty(ep.Title, external)
	now := s.stamp()

	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO episodes (title_id, external_id, season_number, episode_number, name,
                      duration_seconds, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(title_id, external_id) DO UPDATE SET
    name             = excluded.name,
    duration_seconds = excluded.duration_seconds,
    updated_at       = excluded.updated_at
RETURNING id`,
		titleID, external, season, number, nullString(name),
		nullInt(int64(ep.DurationSeconds)), now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ingest: simpan episode %q: %w", external, err)
	}
	return id, nil
}

// upsertPlayable memastikan satu baris playables untuk episode (episodeID
// non-nil) atau movie (episodeID nil) dan mengembalikan id-nya.
func (s *Service) upsertPlayable(ctx context.Context, titleID int64, episodeID *int64, durationSeconds int, encrypted bool) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	now := s.stamp()
	enc := 0
	if encrypted {
		enc = 1
	}
	duration := nullInt(int64(durationSeconds))

	var (
		id    int64
		err   error
		label string
		query string
		args  []any
	)
	if episodeID == nil {
		label = "movie"
		query = `SELECT id FROM playables WHERE title_id = ? AND episode_id IS NULL`
		args = []any{titleID}
	} else {
		label = fmt.Sprintf("episode %d", *episodeID)
		query = `SELECT id FROM playables WHERE episode_id = ?`
		args = []any{*episodeID}
	}

	err = s.store.Write().QueryRowContext(ctx, query, args...).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if episodeID == nil {
			err = s.store.Write().QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, NULL, ?, ?, ?, ?) RETURNING id`,
				titleID, duration, enc, now, now).Scan(&id)
		} else {
			err = s.store.Write().QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				titleID, *episodeID, duration, enc, now, now).Scan(&id)
		}
		if err != nil {
			return 0, fmt.Errorf("ingest: simpan playable %s: %w", label, err)
		}
		return id, nil
	case err != nil:
		return 0, fmt.Errorf("ingest: cari playable %s: %w", label, err)
	}

	if _, err := s.store.Write().ExecContext(ctx, `
UPDATE playables SET duration_seconds = ?, is_encrypted = ?, updated_at = ? WHERE id = ?`,
		duration, enc, now, id,
	); err != nil {
		return 0, fmt.Errorf("ingest: perbarui playable %s: %w", label, err)
	}
	return id, nil
}

// upsertRendition menulis hasil satu rendition ke tabel renditions.
func (s *Service) upsertRendition(ctx context.Context, st *runState, playableID int64, rec *renditionBuild) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	now := s.stamp()
	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height, container,
                        source_url, playlist_path, segment_count, segments_done, total_bytes,
                        bytes_done, duration_seconds, status, verified_at, last_error,
                        created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'ts', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(playable_id, generation_id, quality) DO UPDATE SET
    bitrate_kbps     = excluded.bitrate_kbps,
    width            = excluded.width,
    height           = excluded.height,
    container        = excluded.container,
    source_url       = excluded.source_url,
    playlist_path    = excluded.playlist_path,
    segment_count    = excluded.segment_count,
    segments_done    = excluded.segments_done,
    total_bytes      = excluded.total_bytes,
    bytes_done       = excluded.bytes_done,
    duration_seconds = excluded.duration_seconds,
    status           = excluded.status,
    verified_at      = excluded.verified_at,
    last_error       = excluded.last_error,
    updated_at       = excluded.updated_at
RETURNING id`,
		playableID, st.genID, rec.quality, nullInt(int64(rec.bitrateKbps)),
		nullInt(int64(rec.width)), nullInt(int64(rec.height)), nullString(rec.sourceURL),
		rec.relPlaylist, rec.segmentCount, rec.segmentsDone, rec.totalBytes, rec.bytesDone,
		nullInt(int64(rec.duration)), rec.status, nullString(rec.verifiedAt),
		nullString(truncate(rec.lastError, maxNoteLen)), now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ingest: simpan rendition %s: %w", rec.relPlaylist, err)
	}
	return id, nil
}

// switchGeneration menerbitkan staging lalu menukar generation aktif dalam satu
// transaksi. Generation lama di-retire lebih dulu supaya unique index
// ux_generations_one_active tidak dilanggar.
func (s *Service) switchGeneration(ctx context.Context, st *runState) error {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	files, bytes := 0, int64(0)
	if dir, err := s.media.GenerationDir(st.label); err != nil {
		s.log.Warn("path generation tidak valid", "generation", st.label, "error", err)
	} else if n, b, err := dirStats(dir); err != nil {
		s.log.Warn("gagal menghitung isi generation", "generation", st.label, "error", err)
	} else {
		files, bytes = n, b
	}

	now := s.stamp()
	tx, err := s.store.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ingest: mulai transaksi switch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
UPDATE generations SET status = 'retired', retired_at = ?
 WHERE status = 'active' AND id <> ?`, now, st.genID,
	); err != nil {
		return fmt.Errorf("ingest: retire generation lama: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE generations
   SET status = 'active', activated_at = ?, file_count = ?, total_bytes = ?, note = ?
 WHERE id = ?`,
		now, files, bytes, fmt.Sprintf("ingest run %d", st.runID), st.genID,
	); err != nil {
		return fmt.Errorf("ingest: aktifkan generation %s: %w", st.label, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ingest: commit switch generation: %w", err)
	}

	s.log.Info("generation aktif", "generation", st.label, "files", files, "bytes", bytes)
	return nil
}

// cleanupOrphans membuang playable/episode/title yang tidak lagi punya
// rendition setelah generation lama dihapus. Best effort: kegagalannya hanya
// dicatat di log supaya run yang sudah berhasil tidak berubah menjadi gagal.
func (s *Service) cleanupOrphans(ctx context.Context) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	deleted, err := s.store.CleanupOrphans(ctx)
	if err != nil {
		s.log.Warn("gagal membersihkan katalog tanpa media", "error", err)
		return
	}
	if deleted > 0 {
		s.log.Info("baris katalog tanpa media dihapus", "rows", deleted)
	}
}

// finishRun menutup baris sync_runs beserta seluruh counter-nya.
func (s *Service) finishRun(ctx context.Context, st *runState, status string, cause error) error {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	var errMsg any
	if cause != nil {
		errMsg = truncate(cause.Error(), maxNoteLen)
	}

	_, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs
   SET status = ?, phase = ?, finished_at = ?, total_providers = 1, total_titles = ?,
       total_playables = ?, total_renditions = ?, renditions_ready = ?,
       renditions_failed = ?, bytes_downloaded = ?, error_message = ?
 WHERE id = ?`,
		status, phaseFor(status), s.stamp(), st.titles, st.playables, st.renditions,
		st.ready, st.failed, st.bytes, errMsg, st.runID,
	)
	if err != nil {
		return fmt.Errorf("ingest: tutup sync run %d: %w", st.runID, err)
	}
	return nil
}

// activeRunID mengembalikan id sync run yang belum terminal, atau 0.
func (s *Service) activeRunID(ctx context.Context) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	var id int64
	err := s.store.Read().QueryRowContext(ctx, `
SELECT id FROM sync_runs
 WHERE status IN ('queued','running','verifying','switching','cleanup')
 ORDER BY id DESC LIMIT 1`).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("ingest: cek sync run aktif: %w", err)
	}
	return id, nil
}

// createGeneration menyisipkan baris generations 'building' dengan label
// gen-<MAX(id)+1> dan mengembalikan id serta labelnya.
func (s *Service) createGeneration(ctx context.Context) (int64, string, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	var maxID sql.NullInt64
	if err := s.store.Write().QueryRowContext(ctx,
		`SELECT MAX(id) FROM generations`).Scan(&maxID); err != nil {
		return 0, "", fmt.Errorf("ingest: baca MAX(id) generations: %w", err)
	}
	next := maxID.Int64 + 1
	if !maxID.Valid || next < 1 {
		next = 1
	}

	label := fmt.Sprintf(generationLabelFormat, next)
	// root_path disimpan relatif terhadap root media (labelnya sendiri).
	if _, err := s.store.Write().ExecContext(ctx, `
INSERT INTO generations (id, label, root_path, status, created_at)
VALUES (?, ?, ?, 'building', ?)`,
		next, label, label, s.stamp(),
	); err != nil {
		return 0, "", fmt.Errorf("ingest: buat generation %s: %w", label, err)
	}
	return next, label, nil
}

// insertRun menyisipkan baris sync_runs 'running' untuk satu generation.
func (s *Service) insertRun(ctx context.Context, genID int64) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	now := s.stamp()
	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO sync_runs (generation_id, trigger_source, scope, status, phase, triggered_by,
                       total_providers, started_at, created_at)
VALUES (?, ?, ?, 'running', 'discovery', ?, 1, ?, ?) RETURNING id`,
		genID, runTrigger, runScope, runTrigger, now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ingest: catat sync run: %w", err)
	}
	return id, nil
}

// markGenerationFailed menandai generation yang masih 'building' sebagai gagal
// dan melaporkan apakah ada baris yang benar-benar berubah.
func (s *Service) markGenerationFailed(ctx context.Context, genID int64, note string) bool {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	res, err := s.store.Write().ExecContext(ctx, `
UPDATE generations SET status = 'failed', note = ?
 WHERE id = ? AND status = 'building'`,
		truncate(note, maxNoteLen), genID,
	)
	if err != nil {
		s.log.Warn("gagal menandai generation gagal", "generation_id", genID, "error", err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false
	}
	return n > 0
}

// sqlCtx menghasilkan konteks untuk satu operasi database: tidak ikut
// dibatalkan bersama ctx (pembukuan status akhir harus tetap bisa ditulis)
// tetapi dibatasi dbTimeout.
func (s *Service) sqlCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
}

// now mengembalikan waktu sekarang menurut Config.Now.
func (s *Service) now() time.Time {
	if s.nowFn == nil {
		return time.Now()
	}
	return s.nowFn()
}

// stamp mengembalikan waktu sekarang dalam format TEXT ISO-8601 UTC.
func (s *Service) stamp() string { return s.stampAt(s.now()) }

// stampAt memformat t sebagai TEXT ISO-8601 UTC.
func (s *Service) stampAt(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// phaseFor memetakan status akhir run ke kolom sync_runs.phase.
func phaseFor(status string) string {
	switch status {
	case "completed":
		return "done"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return status
	}
}

// pickStream memilih stream android, lalu ios, sebagai fallback. Stream tanpa
// URL dilewati.
func pickStream(streams []catalog.Stream) (catalog.Stream, bool) {
	var android, ios *catalog.Stream

	for i := range streams {
		if strings.TrimSpace(streams[i].URL) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(streams[i].Device)) {
		case "android":
			if android == nil {
				android = &streams[i]
			}
		case "ios":
			if ios == nil {
				ios = &streams[i]
			}
		}
	}

	if android != nil {
		return *android, true
	}
	if ios != nil {
		return *ios, true
	}
	return catalog.Stream{}, false
}

// countSegmentFiles menghitung berkas segmen yang sudah selesai di dir.
func countSegmentFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, segmentFilePrefix) {
			continue
		}
		if strings.HasSuffix(name, download.PartSuffix) {
			continue
		}
		n++
	}
	return n
}

// dirBytes menjumlahkan ukuran seluruh berkas biasa di dir (rekursif).
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// dirStats menghitung jumlah berkas dan total ukurannya di dir (rekursif).
func dirStats(dir string) (files int, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return files, bytes, nil
}

// resolveURL menyelesaikan ref terhadap base.
func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// safePathComponent mengubah identifier upstream menjadi satu komponen path
// yang aman, sehingga data provider tidak pernah bisa keluar dari direktori
// generation.
func safePathComponent(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == '-', r == '_':
			if b.Len() > 0 && !dash {
				b.WriteRune(r)
				dash = true
			}
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "x"
	}
	if len(out) > 96 {
		out = strings.Trim(out[:96], "-_")
		if out == "" {
			return "x"
		}
	}
	return out
}

// normalizeProvider mengembalikan kode provider lowercase; kosong berarti
// provider default Dramahits.
func normalizeProvider(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return providerCode
	}
	return code
}

// normalizeCategory mengembalikan kategori lowercase; kosong berarti short.
func normalizeCategory(category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		return catalog.CategoryShort
	}
	return category
}

// titleKind memetakan kategori media ke nilai titles.kind.
func titleKind(category string) string {
	if category == catalog.CategoryMovie {
		return "movie"
	}
	return "series"
}

// providerDisplayName mengembalikan nama tampilan provider.
func providerDisplayName(code string) string {
	switch code {
	case providerCode:
		return providerName
	case "densgo":
		return "Densgo"
	default:
		return code
	}
}

// providerDefaultCategory adalah kategori yang disimpan di kolom
// providers.category. Densgo tetap memakai "vod" sebagai naungan provider;
// kategori per judul (movie/series) disimpan di generation_items dan path media.
func providerDefaultCategory(code string) string {
	switch code {
	case providerCode:
		return catalog.CategoryShort
	case "densgo":
		return "vod"
	default:
		return ""
	}
}

// firstNonEmpty mengembalikan nilai pertama yang tidak kosong setelah di-trim.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// isUniqueViolation melaporkan apakah err berasal dari pelanggaran constraint
// unik SQLite (mis. ux_sync_runs_one_active).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// truncate memotong s menjadi paling banyak n byte.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// nullString mengubah string kosong menjadi NULL.
func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// nullInt mengubah angka nol menjadi NULL.
func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
