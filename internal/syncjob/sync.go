// Package syncjob menjalankan sinkronisasi penuh: katalog dari setiap provider
// upstream, playlist HLS beserta segmennya, subtitle dan artwork.
//
// Satu kali Run membangun satu "generation" media baru di direktori staging
// (mediafs), lalu menerbitkannya secara atomik setelah seluruh berkas
// terverifikasi. Bila tidak ada satu pun rendition yang berhasil diunduh,
// staging dibuang dan tidak ada yang berubah bagi pemutar.
//
// Alur Run:
//
//  1. tolak bila masih ada sync_runs yang belum terminal (ErrAlreadyRunning),
//  2. buat baris generations baru berstatus 'building',
//  3. mulai staging mediafs,
//  4. catat sync_runs berstatus 'running',
//  5. tarik katalog tiap provider dan simpan titles/episodes/playables/renditions,
//  6. unduh playlist + segmen setiap rendition,
//  7. unduh subtitle,
//  8. unduh artwork (kegagalan di sini tidak menggagalkan rendition),
//  9. verifikasi berkas setiap rendition,
//  10. bila tidak ada rendition siap: Abort, generation & run 'failed',
//  11. Commit lalu tukar generation aktif dalam satu transaksi,
//  12. buang playable/episode/title yang tidak lagi punya rendition; generation
//     lama dibiarkan 'retired' tanpa dihapus otomatis (penghapusan hanya lewat
//     panel admin),
//  13. tandai sync_runs 'completed',
//  14. kirim ringkasan akhir ke Config.Notifier (best effort, lihat notify).
//
// Semua timestamp disimpan sebagai TEXT ISO-8601 UTC, dan semua path yang
// ditulis ke database bersifat relatif (terhadap root generation untuk media,
// terhadap ArtworkRoot untuk artwork).
package syncjob

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

	"vodlocal/internal/download"
	"vodlocal/internal/hls"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/notify"
	"vodlocal/internal/provider"
	"vodlocal/internal/store"
)

// Kesalahan yang dikembalikan paket ini.
var (
	// ErrAlreadyRunning dilaporkan bila sudah ada sync run yang belum terminal.
	ErrAlreadyRunning = errors.New("syncjob: sinkronisasi lain masih berjalan")
	// ErrRunNotFound dilaporkan Cancel untuk run yang tidak sedang berjalan.
	ErrRunNotFound = errors.New("syncjob: sync run tidak ditemukan")
	// ErrNoRenditions dilaporkan bila tidak ada satu pun rendition yang siap.
	ErrNoRenditions = errors.New("syncjob: tidak ada rendition yang siap")
)

const (
	// stagingPrefix adalah awalan direktori staging mediafs.
	stagingPrefix = ".building-"
	// generationRootPrefix... label generation memakai format gen-%06d.
	generationLabelFormat = "gen-%06d"
	// playlistFileName adalah nama berkas playlist lokal.
	playlistFileName = "index.m3u8"
	// segmentPrefix adalah awalan berkas segmen yang dibuat hls.Rewrite.
	segmentPrefix = "seg-"
	// maxPlaylistBytes membatasi ukuran playlist yang mau dibaca.
	maxPlaylistBytes = 8 << 20
	// publishScope adalah nilai kolom sync_runs.scope untuk Run.
	publishScope = "full"
	// notifyTimeout membatasi pengiriman notifikasi akhir run. Notifikasi
	// memakai context sendiri supaya tetap terkirim walau run dibatalkan.
	notifyTimeout = 10 * time.Second
)

// Config mengatur Syncer.
type Config struct {
	// Store adalah database aplikasi; wajib.
	Store *store.Store
	// Media mengelola direktori generation; wajib.
	Media *mediafs.Manager
	// Downloader dipakai untuk segmen, subtitle dan artwork. Bila nil, Syncer
	// membuat satu dengan opsi default memakai HTTPClient.
	Downloader *download.Downloader
	// Providers adalah sumber katalog; boleh kosong.
	Providers []provider.Provider
	// ArtworkRoot adalah akar penyimpanan poster/backdrop. Bila kosong dipakai
	// <Media.Root()>/artwork.
	ArtworkRoot string
	// Logger menerima log progres. Bila nil dipakai slog.Default().
	Logger *slog.Logger
	// Now memasok waktu sekarang. Bila nil dipakai time.Now.
	Now func() time.Time
	// HTTPClient dipakai untuk mengambil playlist. Bila nil dipakai
	// http.DefaultClient.
	HTTPClient *http.Client
	// Notifier menerima ringkasan setiap run yang mencapai keadaan terminal.
	// Pengiriman bersifat best effort: kegagalannya hanya dicatat di log. Bila
	// nil tidak ada notifikasi yang dikirim.
	Notifier notify.Notifier
}

// Syncer mengoordinasikan sinkronisasi penuh.
type Syncer struct {
	store     *store.Store
	media     *mediafs.Manager
	dl        *download.Downloader
	providers []provider.Provider
	artwork   string
	log       *slog.Logger
	nowFn     func() time.Time
	client    *http.Client
	notifier  notify.Notifier

	// startMu menyerialkan fase penyiapan run (cek run aktif, buat generation,
	// buat sync_runs) sehingga dua Run di proses yang sama tidak pernah berebut
	// label generation yang sama. Constraint unik di database tetap menjadi
	// penjaga terakhir untuk proses lain.
	startMu sync.Mutex
	// runMu melindungi active.
	runMu sync.Mutex
	// active memetakan id sync run ke CancelFunc run tersebut.
	active map[int64]context.CancelFunc
}

// New menyiapkan Syncer dari cfg.
func New(cfg Config) (*Syncer, error) {
	if cfg.Store == nil {
		return nil, errors.New("syncjob: Config.Store wajib diisi")
	}
	if cfg.Media == nil {
		return nil, errors.New("syncjob: Config.Media wajib diisi")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	dl := cfg.Downloader
	if dl == nil {
		dl = download.New(download.Options{Client: client})
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	artwork := strings.TrimSpace(cfg.ArtworkRoot)
	if artwork == "" {
		artwork = filepath.Join(cfg.Media.Root(), "artwork")
	}
	absArtwork, err := filepath.Abs(artwork)
	if err != nil {
		return nil, fmt.Errorf("syncjob: resolve artwork root %q: %w", artwork, err)
	}
	if err := os.MkdirAll(absArtwork, 0o755); err != nil {
		return nil, fmt.Errorf("syncjob: buat artwork root %q: %w", absArtwork, err)
	}

	providers := make([]provider.Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		providers = append(providers, p)
	}

	return &Syncer{
		store:     cfg.Store,
		media:     cfg.Media,
		dl:        dl,
		providers: providers,
		artwork:   absArtwork,
		log:       logger,
		nowFn:     nowFn,
		client:    client,
		notifier:  cfg.Notifier,
		active:    make(map[int64]context.CancelFunc),
	}, nil
}

// Result merangkum satu kali Run.
type Result struct {
	RunID            int64
	GenerationLabel  string
	Titles           int
	Playables        int
	RenditionsReady  int
	RenditionsFailed int
	BytesDownloaded  int64
	// Partial bernilai true bila selesai tetapi ada rendition yang gagal.
	Partial bool
}

// Run menjalankan satu sinkronisasi penuh. trigger adalah sumber pemicu
// ('manual', 'scheduled' atau 'boot'); nilai lain diperlakukan sebagai
// 'manual'.
//
// Error yang dikembalikan membungkus cause-nya, sehingga pembatalan lewat
// Cancel dapat dikenali dengan errors.Is(err, context.Canceled).
func (s *Syncer) Run(ctx context.Context, trigger string) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	res := Result{}
	source := normalizeTrigger(trigger)

	s.startMu.Lock()

	// Langkah 1: tolak bila sudah ada run yang belum terminal.
	activeID, err := s.activeRunID(runCtx)
	if err != nil {
		s.startMu.Unlock()
		return res, err
	}
	if activeID != 0 {
		s.startMu.Unlock()
		return res, fmt.Errorf("%w (run %d)", ErrAlreadyRunning, activeID)
	}

	// Langkah 2: label generation berikutnya + baris generations.
	genID, label, err := s.createGeneration(runCtx)
	if err != nil {
		s.startMu.Unlock()
		return res, err
	}
	res.GenerationLabel = label

	// Langkah 3: staging.
	build, err := s.media.NewStaging(label)
	if err != nil {
		s.markGenerationFailed(runCtx, genID, err.Error())
		s.startMu.Unlock()
		err = fmt.Errorf("syncjob: mulai staging %s: %w", label, err)
		s.notifyTerminal(runCtx, res, terminalStatus(err), err)
		return res, err
	}

	// Langkah 4: baris sync_runs.
	runID, err := s.insertRun(runCtx, genID, source)
	if err != nil {
		if abortErr := build.Abort(); abortErr != nil {
			s.log.Warn("gagal membuang staging", "generation", label, "error", abortErr)
		}
		s.markGenerationFailed(runCtx, genID, err.Error())
		s.startMu.Unlock()
		if isUniqueViolation(err) {
			err = fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
		}
		s.notifyTerminal(runCtx, res, terminalStatus(err), err)
		return res, err
	}
	res.RunID = runID

	s.runMu.Lock()
	s.active[runID] = cancel
	s.runMu.Unlock()
	s.startMu.Unlock()

	defer func() {
		s.runMu.Lock()
		delete(s.active, runID)
		s.runMu.Unlock()
	}()

	st := &runState{
		id:    runID,
		genID: genID,
		label: label,
		build: build,
		dir:   build.Dir(),
		phase: "discovery",
		res:   res,
	}

	s.log.Info("sync dimulai", "run_id", runID, "generation", label, "trigger", source, "providers", len(s.providers))

	return s.execute(runCtx, st)
}

// Cancel menghentikan run runID. Run yang tidak (lagi) berjalan dilaporkan
// dengan ErrRunNotFound.
func (s *Syncer) Cancel(runID int64) error {
	s.runMu.Lock()
	cancel, ok := s.active[runID]
	s.runMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %d", ErrRunNotFound, runID)
	}
	cancel()
	return nil
}

// execute menjalankan sisa alur Run setelah baris database dan staging siap.
func (s *Syncer) execute(ctx context.Context, st *runState) (Result, error) {
	// dbCtx tidak ikut dibatalkan bersama run: pembukuan status akhir harus
	// tetap bisa ditulis walau run dibatalkan.
	dbCtx := context.WithoutCancel(ctx)

	// fail menutup run yang gagal lalu mengirim notifikasi terminal. Kedua
	// langkah itu tetap berjalan walau ctx sudah dibatalkan.
	fail := func(cause error) (Result, error) {
		err := s.abort(dbCtx, st, cause)
		s.notifyTerminal(ctx, st.res, terminalStatus(err), err)
		return st.res, err
	}

	// Langkah 5: katalog.
	if err := s.discover(ctx, dbCtx, st); err != nil {
		return fail(err)
	}

	// Langkah 6: playlist + segmen.
	s.setRunPhase(dbCtx, st, "running", "segments")
	if err := s.fetchRenditions(ctx, dbCtx, st); err != nil {
		return fail(err)
	}

	// Langkah 7: subtitle.
	s.setRunPhase(dbCtx, st, "running", "subtitles")
	if err := s.fetchSubtitles(ctx, dbCtx, st); err != nil {
		return fail(err)
	}

	// Langkah 8: artwork.
	s.setRunPhase(dbCtx, st, "running", "artwork")
	if err := s.fetchArtwork(ctx, dbCtx, st); err != nil {
		return fail(err)
	}

	// Langkah 9: verifikasi.
	s.setRunPhase(dbCtx, st, "verifying", "verify")
	if err := s.verify(dbCtx, st); err != nil {
		return fail(err)
	}

	st.res.RenditionsReady, st.res.RenditionsFailed = countRenditions(st.rends)
	st.res.BytesDownloaded = st.bytes
	st.res.Partial = st.res.RenditionsFailed > 0

	// Langkah 10: tidak ada yang siap berarti generation ini tidak layak tayang.
	if st.res.RenditionsReady == 0 {
		return fail(fmt.Errorf("%w: 0 dari %d rendition", ErrNoRenditions, len(st.rends)))
	}

	// Langkah 11: tukar generation.
	s.setRunPhase(dbCtx, st, "switching", "switch")
	if err := s.switchGeneration(dbCtx, st); err != nil {
		return fail(err)
	}

	// Langkah 12: buang playable/episode/title yang tidak lagi punya rendition.
	// Generation 'retired' sengaja tidak pernah dihapus di sini: penghapusan
	// hanya dilakukan admin lewat panel, bahkan bila ada yang sedang menonton.
	// Best effort: kegagalan di sini hanya dicatat dan tidak menggagalkan run.
	s.setRunPhase(dbCtx, st, "cleanup", "cleanup")
	if deleted, err := s.store.CleanupOrphans(dbCtx); err != nil {
		s.log.Warn("gagal membersihkan katalog tanpa media", "error", err)
	} else if deleted > 0 {
		s.log.Info("baris katalog tanpa media dihapus", "rows", deleted)
	}

	// Langkah 13: selesai. Kolom sync_runs.status tetap 'completed' walau ada
	// rendition gagal; Event memakai 'partial' untuk membedakannya.
	s.setRunPhase(dbCtx, st, "cleanup", "done")
	if err := s.finishRun(dbCtx, st, "completed", nil); err != nil {
		s.notifyTerminal(ctx, st.res, notify.StatusFailed, err)
		return st.res, err
	}

	s.log.Info("sync selesai",
		"run_id", st.id,
		"generation", st.label,
		"titles", st.res.Titles,
		"playables", st.res.Playables,
		"ready", st.res.RenditionsReady,
		"failed", st.res.RenditionsFailed,
		"bytes", st.res.BytesDownloaded,
		"partial", st.res.Partial,
	)

	status := notify.StatusCompleted
	if st.res.Partial {
		status = notify.StatusPartial
	}
	s.notifyTerminal(ctx, st.res, status, nil)

	return st.res, nil
}

// abort menutup run yang gagal: staging dibuang, generation dan run ditandai.
func (s *Syncer) abort(ctx context.Context, st *runState, cause error) error {
	if st.build != nil && !st.build.Closed() {
		if err := st.build.Abort(); err != nil {
			s.log.Warn("gagal membuang staging", "generation", st.label, "error", err)
		}
	}

	status := notify.StatusFailed
	if errors.Is(cause, context.Canceled) {
		status = notify.StatusCancelled
	}

	s.markGenerationFailed(ctx, st.genID, cause.Error())
	if err := s.finishRun(ctx, st, status, cause); err != nil {
		s.log.Warn("gagal mencatat akhir run", "run_id", st.id, "error", err)
	}

	s.log.Error("sync gagal",
		"run_id", st.id,
		"generation", st.label,
		"status", status,
		"error", cause,
	)
	return cause
}

// terminalStatus memetakan galat akhir run ke status notify.Event.
func terminalStatus(cause error) string {
	if errors.Is(cause, context.Canceled) {
		return notify.StatusCancelled
	}
	return notify.StatusFailed
}

// notifyTerminal mengirim ringkasan akhir run ke Config.Notifier.
//
// Pengiriman bersifat best effort: context run yang mungkin sudah dibatalkan
// diganti context baru lewat context.WithoutCancel dengan batas notifyTimeout,
// dan kegagalan pengiriman hanya dicatat di log sehingga tidak pernah mengubah
// hasil Run. Notifier nil (default) tidak melakukan apa pun.
func (s *Syncer) notifyTerminal(ctx context.Context, res Result, status string, cause error) {
	if s.notifier == nil {
		return
	}

	ev := notify.Event{
		RunID:            res.RunID,
		Generation:       res.GenerationLabel,
		Status:           status,
		Titles:           res.Titles,
		Playables:        res.Playables,
		RenditionsReady:  res.RenditionsReady,
		RenditionsFailed: res.RenditionsFailed,
		BytesDownloaded:  res.BytesDownloaded,
		At:               s.now().UTC(),
	}
	if cause != nil {
		ev.Error = cause.Error()
	}

	if ctx == nil {
		ctx = context.Background()
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()

	if err := s.sendNotification(nctx, ev); err != nil {
		s.log.Warn("notifikasi sync gagal",
			"run_id", ev.RunID,
			"generation", ev.Generation,
			"status", status,
			"error", err,
		)
	}
}

// sendNotification memanggil notifier satu kali dan mengubah panic dari
// implementasi notifier menjadi error, sehingga notifikasi tidak pernah
// menjatuhkan proses sync.
func (s *Syncer) sendNotification(ctx context.Context, ev notify.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("syncjob: notifier panic: %v", r)
		}
	}()
	return s.notifier.Notify(ctx, ev)
}

// runState adalah keadaan satu run selama berjalan.
type runState struct {
	id      int64
	genID   int64
	label   string
	build   *mediafs.Build
	dir     string
	phase   string
	res     Result
	bytes   int64
	rends   []*renditionRecord
	subs    []subtitleTask
	arts    []artworkTask
	fileCnt int
	total   int64
}

// renditionRecord menampung satu baris renditions beserta path tujuannya.
type renditionRecord struct {
	id           int64
	playableID   int64
	quality      string
	playlistURL  string
	relDir       string
	playlistRel  string
	playlistAbs  string
	dirAbs       string
	segmentCount int
	segmentsDone int
	totalBytes   int64
	bytesDone    int64
	duration     int
	status       string
	verifiedAt   string
	lastError    string
}

// subtitleTask adalah satu subtitle yang harus diunduh.
type subtitleTask struct {
	playableID int64
	relDir     string
	sub        provider.Subtitle
}

// artworkTask adalah satu gambar yang harus diunduh.
type artworkTask struct {
	titleID int64
	relPath string
	url     string
	column  string
}

// discover menarik katalog setiap provider dan menyimpannya ke database.
func (s *Syncer) discover(ctx context.Context, dbCtx context.Context, st *runState) error {
	for _, p := range s.providers {
		if err := ctx.Err(); err != nil {
			return err
		}

		provID, err := s.upsertProvider(dbCtx, p.Code())
		if err != nil {
			return fmt.Errorf("syncjob: simpan provider %q: %w", p.Code(), err)
		}

		titles, err := p.ListTitles(ctx)
		if err != nil {
			return fmt.Errorf("syncjob: provider %s: ListTitles: %w", p.Code(), err)
		}

		for _, t := range titles {
			if err := ctx.Err(); err != nil {
				return err
			}

			eps, plays, err := fetchTitleData(ctx, p, t)
			if err != nil {
				return err
			}
			if err := s.storeTitle(dbCtx, st, provID, p.Code(), t, eps, plays); err != nil {
				return err
			}
			st.res.Titles++
		}

		if err := s.touchProvider(dbCtx, provID); err != nil {
			return err
		}
		s.log.Info("katalog provider tersimpan",
			"provider", p.Code(),
			"titles", len(titles),
			"renditions", len(st.rends),
		)
	}
	return nil
}

// fetchTitleData mengambil episode dan playable satu title. ErrNotFound
// dianggap "tidak ada data" supaya satu title kosong tidak menggagalkan run.
func fetchTitleData(ctx context.Context, p provider.Provider, t provider.Title) ([]provider.Episode, []provider.Playable, error) {
	var eps []provider.Episode
	if t.Kind == provider.KindSeries {
		list, err := p.ListEpisodes(ctx, t.ExternalID)
		switch {
		case err == nil:
			eps = list
		case errors.Is(err, provider.ErrNotFound):
			// series tanpa episode upstream
		default:
			return nil, nil, fmt.Errorf("syncjob: provider %s: ListEpisodes(%s): %w", p.Code(), t.ExternalID, err)
		}
	}

	plays, err := p.GetPlayables(ctx, t.ExternalID)
	switch {
	case err == nil:
	case errors.Is(err, provider.ErrNotFound):
		plays = nil
	default:
		return nil, nil, fmt.Errorf("syncjob: provider %s: GetPlayables(%s): %w", p.Code(), t.ExternalID, err)
	}
	return eps, plays, nil
}

// storeTitle menyimpan satu title beserta episode, playable dan rendition-nya
// dalam satu transaksi pendek.
func (s *Syncer) storeTitle(ctx context.Context, st *runState, provID int64, code string, t provider.Title, eps []provider.Episode, plays []provider.Playable) error {
	tx, err := s.store.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("syncjob: mulai transaksi title %q: %w", t.ExternalID, err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.stamp()

	kind := t.Kind
	if kind != provider.KindMovie && kind != provider.KindSeries {
		kind = provider.KindMovie
	}
	name := t.Title
	if name == "" {
		name = t.ExternalID
	}
	slug := t.Slug
	if slug == "" {
		slug = safePathComponent(t.ExternalID)
	}

	var titleID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO titles (provider_id, external_id, kind, title, slug, synopsis, release_year,
                    content_rating, language, country, duration_seconds, is_published,
                    fetched_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(provider_id, external_id) DO UPDATE SET
    kind             = excluded.kind,
    title            = excluded.title,
    slug             = excluded.slug,
    synopsis         = excluded.synopsis,
    release_year     = excluded.release_year,
    content_rating   = excluded.content_rating,
    language         = excluded.language,
    country          = excluded.country,
    duration_seconds = excluded.duration_seconds,
    fetched_at       = excluded.fetched_at,
    updated_at       = excluded.updated_at
RETURNING id`,
		provID, t.ExternalID, kind, name, slug, nullString(t.Synopsis),
		nullInt(int64(t.ReleaseYear)), nullString(t.ContentRating), nullString(t.Language),
		nullString(t.Country), nullInt(int64(t.DurationSeconds)),
		now, now, now,
	).Scan(&titleID)
	if err != nil {
		return fmt.Errorf("syncjob: simpan title %q: %w", t.ExternalID, err)
	}

	epIDs := make(map[string]int64, len(eps))
	for _, ep := range eps {
		id, err := upsertEpisode(ctx, tx, titleID, ep, now)
		if err != nil {
			return fmt.Errorf("syncjob: simpan episode %q: %w", ep.ExternalID, err)
		}
		if ep.ExternalID != "" {
			epIDs[ep.ExternalID] = id
		}
	}

	base := path.Join(safePathComponent(code), kind, safePathComponent(t.ExternalID))

	for _, pl := range plays {
		var epID sql.NullInt64
		if pl.EpisodeExternalID != "" {
			id, ok := epIDs[pl.EpisodeExternalID]
			if !ok {
				err := tx.QueryRowContext(ctx,
					`SELECT id FROM episodes WHERE title_id = ? AND external_id = ?`,
					titleID, pl.EpisodeExternalID).Scan(&id)
				switch {
				case errors.Is(err, sql.ErrNoRows):
					s.log.Warn("playable dilewati: episode tidak dikenal",
						"provider", code, "title", t.ExternalID, "episode", pl.EpisodeExternalID)
					continue
				case err != nil:
					return fmt.Errorf("syncjob: resolusi episode %q: %w", pl.EpisodeExternalID, err)
				}
			}
			epID = sql.NullInt64{Int64: id, Valid: true}
		}

		playableID, err := upsertPlayable(ctx, tx, titleID, epID, pl, now)
		if err != nil {
			return fmt.Errorf("syncjob: simpan playable %q: %w", pl.ExternalID, err)
		}
		st.res.Playables++

		playableBase := base
		if pl.EpisodeExternalID != "" {
			playableBase = path.Join(base, safePathComponent(pl.EpisodeExternalID))
		}

		for _, v := range pl.Variants {
			if strings.TrimSpace(v.PlaylistURL) == "" {
				s.log.Warn("variant tanpa playlist dilewati",
					"provider", code, "title", t.ExternalID, "quality", v.Quality)
				continue
			}
			rec := &renditionRecord{
				playableID:  playableID,
				quality:     v.Quality,
				playlistURL: v.PlaylistURL,
				relDir:      path.Join(playableBase, safePathComponent(v.Quality)),
				status:      "pending",
			}
			rec.playlistRel = path.Join(rec.relDir, playlistFileName)
			rec.playlistAbs = filepath.Join(st.dir, filepath.FromSlash(rec.playlistRel))
			rec.dirAbs = filepath.Dir(rec.playlistAbs)

			id, err := insertRendition(ctx, tx, st.genID, rec, v, now)
			if err != nil {
				return fmt.Errorf("syncjob: simpan rendition %q/%q: %w", t.ExternalID, v.Quality, err)
			}
			rec.id = id
			st.rends = append(st.rends, rec)
		}

		for _, sub := range pl.Subtitles {
			st.subs = append(st.subs, subtitleTask{playableID: playableID, relDir: playableBase, sub: sub})
		}
	}

	artBase := path.Join(safePathComponent(code), safePathComponent(t.ExternalID))
	if t.PosterURL != "" {
		st.arts = append(st.arts, artworkTask{
			titleID: titleID,
			relPath: path.Join(artBase, "poster."+extFromURL(t.PosterURL, "jpg")),
			url:     t.PosterURL,
			column:  "poster_path",
		})
	}
	if t.BackdropURL != "" {
		st.arts = append(st.arts, artworkTask{
			titleID: titleID,
			relPath: path.Join(artBase, "backdrop."+extFromURL(t.BackdropURL, "jpg")),
			url:     t.BackdropURL,
			column:  "backdrop_path",
		})
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncjob: commit title %q: %w", t.ExternalID, err)
	}
	return nil
}

// upsertEpisode menyimpan satu episode dan mengembalikan id-nya.
func upsertEpisode(ctx context.Context, tx *sql.Tx, titleID int64, ep provider.Episode, now string) (int64, error) {
	season := ep.SeasonNumber
	if season <= 0 {
		season = 1
	}

	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO episodes (title_id, external_id, season_number, episode_number, name, synopsis,
                      duration_seconds, air_date, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(title_id, season_number, episode_number) DO UPDATE SET
    name             = excluded.name,
    synopsis         = excluded.synopsis,
    duration_seconds = excluded.duration_seconds,
    air_date         = excluded.air_date,
    updated_at       = excluded.updated_at
RETURNING id`,
		titleID, ep.ExternalID, season, ep.EpisodeNumber, nullString(ep.Name),
		nullString(ep.Synopsis), nullInt(int64(ep.DurationSeconds)), nullString(ep.AirDate),
		now, now,
	).Scan(&id)
	return id, err
}

// upsertPlayable menyimpan satu playable (movie: episode_id NULL) dan
// mengembalikan id-nya.
func upsertPlayable(ctx context.Context, tx *sql.Tx, titleID int64, epID sql.NullInt64, pl provider.Playable, now string) (int64, error) {
	encrypted := 0
	if pl.Encrypted {
		encrypted = 1
	}
	duration := nullInt(int64(pl.DurationSeconds))

	var id int64
	var err error
	if epID.Valid {
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM playables WHERE episode_id = ?`, epID.Int64).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				titleID, epID.Int64, duration, encrypted, now, now).Scan(&id)
			return id, err
		}
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE playables SET duration_seconds = ?, is_encrypted = ?, updated_at = ? WHERE id = ?`,
			duration, encrypted, now, id)
		return id, err
	}

	err = tx.QueryRowContext(ctx,
		`SELECT id FROM playables WHERE title_id = ? AND episode_id IS NULL`, titleID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, NULL, ?, ?, ?, ?) RETURNING id`,
			titleID, duration, encrypted, now, now).Scan(&id)
		return id, err
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE playables SET duration_seconds = ?, is_encrypted = ?, updated_at = ? WHERE id = ?`,
		duration, encrypted, now, id)
	return id, err
}

// insertRendition membuat baris renditions berstatus 'pending'.
func insertRendition(ctx context.Context, tx *sql.Tx, genID int64, rec *renditionRecord, v provider.Variant, now string) (int64, error) {
	container := v.Container
	if container != provider.ContainerFMP4 {
		container = provider.ContainerTS
	}

	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
                        container, source_url, playlist_path, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?) RETURNING id`,
		rec.playableID, genID, v.Quality, nullInt(int64(v.BitrateKbps)),
		nullInt(int64(v.Width)), nullInt(int64(v.Height)), container,
		nullString(v.PlaylistURL), rec.playlistRel, now, now,
	).Scan(&id)
	return id, err
}

// fetchRenditions mengunduh playlist dan segmen setiap rendition. Kegagalan
// satu rendition dicatat pada barisnya; hanya pembatalan context yang
// menghentikan seluruh proses.
func (s *Syncer) fetchRenditions(ctx context.Context, dbCtx context.Context, st *runState) error {
	for _, rec := range st.rends {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := s.fetchRendition(ctx, st, rec)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			rec.status = "failed"
			rec.lastError = err.Error()
			s.log.Warn("rendition gagal",
				"rendition", rec.id, "quality", rec.quality, "error", err)
		}

		if err := s.saveRendition(dbCtx, rec); err != nil {
			return err
		}
	}
	return nil
}

// fetchRendition mengambil satu playlist, menulis versi lokalnya, lalu
// mengunduh seluruh aset yang dirujuknya.
func (s *Syncer) fetchRendition(ctx context.Context, st *runState, rec *renditionRecord) error {
	pl, finalURL, err := s.fetchPlaylist(ctx, rec.playlistURL)
	if err != nil {
		return err
	}

	out, assets, err := hls.Rewrite(pl, hls.RewriteOptions{})
	if err != nil {
		return fmt.Errorf("syncjob: rewrite %s: %w", rec.playlistURL, err)
	}

	if err := os.MkdirAll(rec.dirAbs, 0o755); err != nil {
		return fmt.Errorf("syncjob: buat direktori rendition %s: %w", rec.dirAbs, err)
	}
	if err := os.WriteFile(rec.playlistAbs, out, 0o644); err != nil {
		return fmt.Errorf("syncjob: tulis playlist %s: %w", rec.playlistAbs, err)
	}

	rec.segmentCount = 0
	var duration float64
	for _, seg := range pl.Segments {
		duration += seg.Duration
	}
	rec.duration = int(math.Round(duration))

	jobs := make([]download.Job, 0, len(assets))
	for _, a := range assets {
		if a.Kind == hls.KindSegment {
			rec.segmentCount++
		}
		src, err := resolveURL(finalURL, a.SourceURI)
		if err != nil {
			return fmt.Errorf("syncjob: resolusi aset %q: %w", a.SourceURI, err)
		}
		jobs = append(jobs, download.Job{URL: src, Dest: filepath.Join(rec.dirAbs, a.LocalName)})
	}

	var dlErr error
	if len(jobs) > 0 {
		res, err := s.dl.Download(ctx, jobs)
		rec.segmentsDone = res.Done + res.Skipped
		rec.bytesDone = res.Bytes
		st.bytes += res.Bytes
		if err != nil {
			dlErr = fmt.Errorf("syncjob: %d dari %d berkas gagal: %w", res.Failed, res.Total, err)
		}
	}

	// total_bytes dihitung dari isi direktori supaya berkas yang dilewati
	// (sudah ada) tetap terhitung.
	if onDisk, err := dirBytes(rec.dirAbs); err == nil {
		rec.totalBytes = onDisk
		if dlErr == nil {
			rec.bytesDone = onDisk
		}
	}

	if dlErr != nil {
		return dlErr
	}
	rec.status = "ready"
	rec.verifiedAt = s.stamp()
	return nil
}

// fetchPlaylist mengambil dan mem-parse playlist. Master playlist diturunkan ke
// varian dengan bandwidth tertinggi (satu tingkat).
func (s *Syncer) fetchPlaylist(ctx context.Context, rawURL string) (*hls.Playlist, string, error) {
	pl, finalURL, err := s.getPlaylist(ctx, rawURL)
	if err != nil {
		return nil, "", err
	}
	if !pl.IsMaster {
		return pl, finalURL, nil
	}
	if len(pl.Variants) == 0 {
		return nil, "", fmt.Errorf("syncjob: master playlist %s tanpa varian", rawURL)
	}

	best := pl.Variants[0]
	for _, v := range pl.Variants[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	next, err := resolveURL(finalURL, best.URI)
	if err != nil {
		return nil, "", fmt.Errorf("syncjob: resolusi varian %q: %w", best.URI, err)
	}

	media, mediaURL, err := s.getPlaylist(ctx, next)
	if err != nil {
		return nil, "", err
	}
	if media.IsMaster {
		return nil, "", fmt.Errorf("syncjob: master playlist bersarang di %s", next)
	}
	return media, mediaURL, nil
}

// getPlaylist melakukan satu GET dan mem-parse hasilnya.
func (s *Syncer) getPlaylist(ctx context.Context, rawURL string) (*hls.Playlist, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("syncjob: permintaan playlist %q: %w", rawURL, err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("syncjob: ambil playlist %q: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("syncjob: playlist %s: status %d", rawURL, resp.StatusCode)
	}

	pl, err := hls.Parse(io.LimitReader(resp.Body, maxPlaylistBytes))
	if err != nil {
		return nil, "", fmt.Errorf("syncjob: parse playlist %s: %w", rawURL, err)
	}

	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return pl, final, nil
}

// saveRendition menulis kembali keadaan satu rendition.
func (s *Syncer) saveRendition(ctx context.Context, rec *renditionRecord) error {
	_, err := s.store.Write().ExecContext(ctx, `
UPDATE renditions
   SET playlist_path = ?, segment_count = ?, segments_done = ?, total_bytes = ?, bytes_done = ?,
       duration_seconds = ?, status = ?, verified_at = ?, last_error = ?, updated_at = ?
 WHERE id = ?`,
		rec.playlistRel, rec.segmentCount, rec.segmentsDone, rec.totalBytes, rec.bytesDone,
		nullInt(int64(rec.duration)), rec.status, nullString(rec.verifiedAt),
		nullString(rec.lastError), s.stamp(), rec.id,
	)
	if err != nil {
		return fmt.Errorf("syncjob: simpan rendition %d: %w", rec.id, err)
	}
	return nil
}

// fetchSubtitles mengunduh setiap subtitle ke direktori playable-nya.
func (s *Syncer) fetchSubtitles(ctx context.Context, dbCtx context.Context, st *runState) error {
	for _, task := range st.subs {
		if err := ctx.Err(); err != nil {
			return err
		}

		lang := strings.TrimSpace(task.sub.Lang)
		if lang == "" {
			lang = "und"
		}
		format := subtitleFormat(task.sub.Format, task.sub.URL)
		if strings.TrimSpace(task.sub.URL) == "" {
			s.log.Warn("subtitle tanpa URL dilewati", "playable", task.playableID, "lang", lang)
			continue
		}

		rel := path.Join(task.relDir, "subtitle-"+safePathComponent(lang)+"."+format)
		abs := filepath.Join(st.dir, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			s.log.Warn("subtitle gagal: direktori", "playable", task.playableID, "error", err)
			continue
		}

		status := "ready"
		var size int64
		res, err := s.dl.Download(ctx, []download.Job{{URL: task.sub.URL, Dest: abs}})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			status = "failed"
			s.log.Warn("subtitle gagal diunduh",
				"playable", task.playableID, "lang", lang, "error", err)
		} else {
			size = res.Bytes
			st.bytes += res.Bytes
		}

		if _, err := s.store.Write().ExecContext(dbCtx, `
INSERT INTO subtitles (playable_id, generation_id, lang, label, format, file_path, bytes, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(playable_id, generation_id, lang) DO UPDATE SET
    label     = excluded.label,
    format    = excluded.format,
    file_path = excluded.file_path,
    bytes     = excluded.bytes,
    status    = excluded.status`,
			task.playableID, st.genID, lang, nullString(task.sub.Label), format,
			rel, size, status,
		); err != nil {
			return fmt.Errorf("syncjob: simpan subtitle %d/%s: %w", task.playableID, lang, err)
		}
	}
	return nil
}

// fetchArtwork mengunduh poster/backdrop. Kegagalan hanya dicatat di log.
func (s *Syncer) fetchArtwork(ctx context.Context, dbCtx context.Context, st *runState) error {
	for _, task := range st.arts {
		if err := ctx.Err(); err != nil {
			return err
		}

		abs := filepath.Join(s.artwork, filepath.FromSlash(task.relPath))
		res, err := s.dl.Download(ctx, []download.Job{{URL: task.url, Dest: abs}})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			s.log.Warn("artwork gagal diunduh, dilewati",
				"title", task.titleID, "url", task.url, "error", err)
			continue
		}
		st.bytes += res.Bytes

		if _, err := s.store.Write().ExecContext(dbCtx,
			`UPDATE titles SET `+task.column+` = ?, updated_at = ? WHERE id = ?`,
			task.relPath, s.stamp(), task.titleID,
		); err != nil {
			return fmt.Errorf("syncjob: simpan path artwork title %d: %w", task.titleID, err)
		}
	}
	return nil
}

// verify memastikan setiap rendition yang berstatus 'ready' benar-benar punya
// playlist dan jumlah berkas segmen yang sesuai.
func (s *Syncer) verify(ctx context.Context, st *runState) error {
	for _, rec := range st.rends {
		if rec.status != "ready" {
			continue
		}

		reason := ""
		if _, err := os.Stat(rec.playlistAbs); err != nil {
			reason = "playlist tidak ada: " + err.Error()
		} else if n := countSegmentFiles(rec.dirAbs); n != rec.segmentCount {
			reason = fmt.Sprintf("jumlah segmen %d, seharusnya %d", n, rec.segmentCount)
		}

		if reason != "" {
			rec.status = "failed"
			rec.lastError = reason
			s.log.Warn("verifikasi rendition gagal", "rendition", rec.id, "alasan", reason)
		} else {
			rec.verifiedAt = s.stamp()
		}

		if err := s.saveRendition(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

// switchGeneration menerbitkan staging lalu menukar generation aktif dalam satu
// transaksi. Generation lama di-retire lebih dulu supaya unique index
// ux_generations_one_active tidak dilanggar.
func (s *Syncer) switchGeneration(ctx context.Context, st *runState) error {
	if err := st.build.Commit(); err != nil {
		return fmt.Errorf("syncjob: commit generation %s: %w", st.label, err)
	}

	genDir, err := s.media.GenerationDir(st.label)
	if err != nil {
		return fmt.Errorf("syncjob: direktori generation %s: %w", st.label, err)
	}
	files, bytes, err := dirStats(genDir)
	if err != nil {
		return fmt.Errorf("syncjob: hitung isi generation %s: %w", st.label, err)
	}
	st.fileCnt, st.total = files, bytes

	now := s.stamp()
	tx, err := s.store.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("syncjob: mulai transaksi switch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE generations SET status = 'retired', retired_at = ? WHERE status = 'active' AND id <> ?`,
		now, st.genID,
	); err != nil {
		return fmt.Errorf("syncjob: retire generation lama: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE generations
   SET status = 'active', activated_at = ?, file_count = ?, total_bytes = ?, note = ?
 WHERE id = ?`,
		now, files, bytes, fmt.Sprintf("sync run %d", st.id), st.genID,
	); err != nil {
		return fmt.Errorf("syncjob: aktifkan generation %s: %w", st.label, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncjob: commit switch generation: %w", err)
	}

	s.log.Info("generation aktif", "generation", st.label, "files", files, "bytes", bytes)
	return nil
}

// setRunPhase memperbarui status/phase run selama berjalan.
func (s *Syncer) setRunPhase(ctx context.Context, st *runState, status, phase string) {
	st.phase = phase
	if _, err := s.store.Write().ExecContext(ctx,
		`UPDATE sync_runs SET status = ?, phase = ? WHERE id = ?`,
		status, phase, st.id,
	); err != nil {
		s.log.Warn("gagal memperbarui phase run", "run_id", st.id, "phase", phase, "error", err)
	}
}

// finishRun menutup baris sync_runs beserta seluruh counter-nya.
func (s *Syncer) finishRun(ctx context.Context, st *runState, status string, cause error) error {
	var errMsg any
	if cause != nil {
		errMsg = cause.Error()
	}

	_, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs
   SET status = ?, phase = ?, finished_at = ?, total_providers = ?, total_titles = ?,
       total_playables = ?, total_renditions = ?, renditions_ready = ?,
       renditions_failed = ?, bytes_downloaded = ?, error_message = ?
 WHERE id = ?`,
		status, st.phase, s.stamp(), len(s.providers), st.res.Titles,
		st.res.Playables, len(st.rends), st.res.RenditionsReady,
		st.res.RenditionsFailed, st.bytes, errMsg, st.id,
	)
	if err != nil {
		return fmt.Errorf("syncjob: tutup sync run %d: %w", st.id, err)
	}
	return nil
}

// activeRunID mengembalikan id sync run yang belum terminal, atau 0.
func (s *Syncer) activeRunID(ctx context.Context) (int64, error) {
	var id int64
	err := s.store.Read().QueryRowContext(ctx, `
SELECT id FROM sync_runs
 WHERE status IN ('queued','running','verifying','switching','cleanup')
 ORDER BY id DESC LIMIT 1`).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("syncjob: cek sync run aktif: %w", err)
	}
	return id, nil
}

// createGeneration menyisipkan baris generations 'building' dengan label
// gen-<MAX(id)+1> dan mengembalikan id serta label-nya.
func (s *Syncer) createGeneration(ctx context.Context) (int64, string, error) {
	var maxID sql.NullInt64
	if err := s.store.Read().QueryRowContext(ctx,
		`SELECT MAX(id) FROM generations`).Scan(&maxID); err != nil {
		return 0, "", fmt.Errorf("syncjob: baca MAX(id) generations: %w", err)
	}
	next := maxID.Int64 + 1
	if !maxID.Valid {
		next = 1
	}

	label := fmt.Sprintf(generationLabelFormat, next)
	// root_path disimpan relatif terhadap root media (yaitu labelnya sendiri),
	// bukan path absolut.
	if _, err := s.store.Write().ExecContext(ctx, `
INSERT INTO generations (id, label, root_path, status, created_at)
VALUES (?, ?, ?, 'building', ?)`,
		next, label, label, s.stamp(),
	); err != nil {
		return 0, "", fmt.Errorf("syncjob: buat generation %s: %w", label, err)
	}
	return next, label, nil
}

// insertRun menyisipkan baris sync_runs 'running'.
func (s *Syncer) insertRun(ctx context.Context, genID int64, source string) (int64, error) {
	now := s.stamp()
	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO sync_runs (generation_id, trigger_source, scope, status, phase, triggered_by,
                       total_providers, started_at, created_at)
VALUES (?, ?, ?, 'running', 'discovery', ?, ?, ?, ?) RETURNING id`,
		genID, source, publishScope, source, len(s.providers), now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("syncjob: catat sync run: %w", err)
	}
	return id, nil
}

// markGenerationFailed menandai generation gagal; kegagalan hanya di-log.
func (s *Syncer) markGenerationFailed(ctx context.Context, genID int64, note string) {
	if _, err := s.store.Write().ExecContext(ctx,
		`UPDATE generations SET status = 'failed', note = ? WHERE id = ? AND status = 'building'`,
		truncate(note, 500), genID,
	); err != nil {
		s.log.Warn("gagal menandai generation gagal", "generation_id", genID, "error", err)
	}
}

// upsertProvider memastikan baris providers ada dan mengembalikan id-nya.
func (s *Syncer) upsertProvider(ctx context.Context, code string) (int64, error) {
	now := s.stamp()
	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO providers (code, name, is_enabled, created_at, updated_at)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT(code) DO UPDATE SET updated_at = excluded.updated_at
RETURNING id`,
		code, code, now, now,
	).Scan(&id)
	return id, err
}

// touchProvider mencatat waktu sinkronisasi terakhir sebuah provider.
func (s *Syncer) touchProvider(ctx context.Context, provID int64) error {
	now := s.stamp()
	if _, err := s.store.Write().ExecContext(ctx,
		`UPDATE providers SET last_synced_at = ?, updated_at = ? WHERE id = ?`,
		now, now, provID,
	); err != nil {
		return fmt.Errorf("syncjob: perbarui provider %d: %w", provID, err)
	}
	return nil
}

// now mengembalikan waktu sekarang menurut Config.Now.
func (s *Syncer) now() time.Time {
	if s.nowFn == nil {
		return time.Now()
	}
	return s.nowFn()
}

// stamp mengembalikan waktu sekarang dalam format TEXT ISO-8601 UTC.
func (s *Syncer) stamp() string { return s.stampAt(s.now()) }

// stampAt memformat t sebagai TEXT ISO-8601 UTC.
func (s *Syncer) stampAt(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// countRenditions menghitung rendition siap dan gagal.
func countRenditions(recs []*renditionRecord) (ready, failed int) {
	for _, rec := range recs {
		switch rec.status {
		case "ready":
			ready++
		case "failed":
			failed++
		}
	}
	return ready, failed
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
		if e.IsDir() || !strings.HasPrefix(name, segmentPrefix) {
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
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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

// normalizeTrigger memetakan trigger ke nilai yang diterima skema.
func normalizeTrigger(trigger string) string {
	switch strings.ToLower(strings.TrimSpace(trigger)) {
	case "scheduled":
		return "scheduled"
	case "boot":
		return "boot"
	default:
		return "manual"
	}
}

// subtitleFormat memilih format subtitle yang diterima skema.
func subtitleFormat(format, rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case provider.FormatVTT:
		return provider.FormatVTT
	case provider.FormatSRT:
		return provider.FormatSRT
	}
	switch extFromURL(rawURL, "") {
	case provider.FormatVTT:
		return provider.FormatVTT
	case provider.FormatSRT:
		return provider.FormatSRT
	}
	return provider.FormatVTT
}

// extFromURL mengambil ekstensi berkas dari sebuah URL, atau def.
func extFromURL(raw, def string) string {
	u := raw
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = path.Base(strings.ReplaceAll(u, `\`, "/"))
	i := strings.LastIndex(u, ".")
	if i < 0 || i == len(u)-1 {
		return def
	}
	ext := strings.ToLower(u[i+1:])
	if !isSimpleExt(ext) {
		return def
	}
	return ext
}

// isSimpleExt melaporkan apakah s layak menjadi ekstensi berkas.
func isSimpleExt(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
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
