package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vodlocal/internal/admin"
	"vodlocal/internal/api"
	"vodlocal/internal/catalog"
	"vodlocal/internal/config"
	"vodlocal/internal/densgo"
	"vodlocal/internal/download"
	"vodlocal/internal/dramahits"
	"vodlocal/internal/httpserver"
	"vodlocal/internal/ingest"
	"vodlocal/internal/mediafs"
	"vodlocal/internal/notify"
	"vodlocal/internal/scheduler"
	"vodlocal/internal/store"
	"vodlocal/internal/syncjob"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		slog.Error("server berhenti", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Error("menutup database", "err", cerr)
		}
	}()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if n, err := st.BackfillGenerationItems(ctx); err != nil {
		logger.Warn("gagal backfill generation_items", "err", err)
	} else if n > 0 {
		logger.Info("backfill generation_items", "rows", n)
	}

	media, err := mediafs.New(cfg.MediaRoot)
	if err != nil {
		return err
	}

	downloader := download.New(download.Options{
		MaxParallel: cfg.SyncMaxParallel,
		Retries:     cfg.SyncRetries,
		Backoff:     cfg.SyncRetryBackoff,
	})

	var notifiers notify.Multi
	if cfg.NotifyWebhookURL != "" {
		notifiers = append(notifiers, notify.NewWebhook(cfg.NotifyWebhookURL, 10*time.Second))
	}
	if cfg.TelegramToken != "" {
		notifiers = append(notifiers, notify.NewTelegram(cfg.TelegramToken, cfg.TelegramChatID, 10*time.Second))
	}
	var notifier notify.Notifier
	if len(notifiers) > 0 {
		notifier = notifiers
		logger.Info("notifikasi sync aktif", "jumlah", len(notifiers))
	}

	dramaClient := dramahits.NewClient(cfg.DramahitsBaseURL, 30*time.Second)
	densgoMovie := densgo.NewClient(cfg.DensgoBaseURL, 30*time.Second, catalog.CategoryMovie)
	densgoSeries := densgo.NewClient(cfg.DensgoBaseURL, 30*time.Second, catalog.CategorySeries)
	sources := catalog.SourceSet{dramaClient, densgoMovie, densgoSeries}

	ingestSvc, err := ingest.New(ingest.Config{
		Store:       st,
		Media:       media,
		Downloader:  downloader,
		Sources:     sources,
		Logger:      logger,
		Now:         time.Now,
		ArtworkRoot: cfg.ArtworkRoot,
		MaxEpisodes: cfg.IngestMaxEpisodes,
		// Pemeriksaan kapasitas disk sebelum ingest dimulai. Ambang 0 (tidak
		// mungkin dari config, yang membatasi 1..100) berarti pemeriksaan
		// dilewati.
		DiskUsage:           diskUsage,
		StorageBlockPercent: cfg.StorageBlockSyncPercent,
	})
	if err != nil {
		return err
	}

	syncer, err := syncjob.New(syncjob.Config{
		Store:       st,
		Media:       media,
		Downloader:  downloader,
		ArtworkRoot: cfg.ArtworkRoot,
		Logger:      logger,
		Now:         time.Now,
		Notifier:    notifier,
	})
	if err != nil {
		return err
	}

	if err := syncer.Recover(ctx); err != nil {
		logger.Warn("pemulihan setelah boot gagal", "err", err)
	}

	// Sekali saat boot: buang playable/episode/title yang medianya sudah tidak
	// ada (mis. generationnya terhapus saat server mati). Best effort.
	if deleted, err := st.CleanupOrphans(ctx); err != nil {
		logger.Warn("pembersihan katalog tanpa media gagal", "err", err)
	} else if deleted > 0 {
		logger.Info("baris katalog tanpa media dihapus saat boot", "rows", deleted)
	}

	sched, err := scheduler.New(scheduler.Config{
		Store:    st,
		Syncer:   syncer,
		Logger:   logger,
		Now:      time.Now,
		Location: time.Local,
	})
	if err != nil {
		return err
	}
	go func() {
		if err := sched.Run(ctx); err != nil {
			logger.Error("scheduler berhenti", "err", err)
		}
	}()

	sessions := httpserver.NewSessionManager([]byte(cfg.SessionSecret), 12*time.Hour)
	deviceAuth := api.NewAuthenticator(st.Write(), time.Now)

	adminPanel, err := admin.New(admin.Config{
		Store:    st,
		Syncer:   syncer,
		Media:    media,
		Sessions: sessions,
		Logger:   logger,
		Now:      time.Now,
		Version:  version,
		Lister:   sources,
		Ingest:   ingestSvc,
		// ArtworkRoot dipakai aksi "Remove all batch" untuk mengosongkan cache
		// artwork setelah katalog dihapus.
		ArtworkRoot: cfg.ArtworkRoot,
		StorageUsage: func() (int64, int64, error) {
			return diskUsage(cfg.MediaRoot)
		},
	})
	if err != nil {
		return err
	}

	if cfg.AdminUser != "" {
		if err := adminPanel.SeedAdmin(ctx, cfg.AdminUser, cfg.AdminPassword); err != nil {
			logger.Warn("seed admin gagal", "err", err)
		}
	}

	apiRouter := api.NewRouter(api.Config{
		Read:      st.Read(),
		Write:     st.Write(),
		Logger:    logger,
		AdminOnly: httpserver.RequireAdmin(sessions),
		Now:       time.Now,
	})

	handler := httpserver.NewRouter(httpserver.Config{
		Version:  version,
		Logger:   logger,
		Media:    mediaResolver{media: media, artworkRoot: cfg.ArtworkRoot},
		Ready:    readiness{store: st, media: media},
		Devices:  deviceAuth,
		Sessions: sessions,
		API:      apiRouter,
		Register: adminPanel.Mount,
		Now:      time.Now,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server mendengarkan", "addr", cfg.HTTPAddr, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("sinyal berhenti diterima")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("server berhenti")
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// mediaResolver menjembatani handler media dengan layout direktori di disk.
type mediaResolver struct {
	media       *mediafs.Manager
	artworkRoot string
}

func (m mediaResolver) ResolveMedia(_ context.Context, generation, rel string) (string, error) {
	return m.media.Resolve(generation, rel)
}

func (m mediaResolver) ResolveArtwork(_ context.Context, rel string) (string, error) {
	return safeJoin(m.artworkRoot, rel)
}

func safeJoin(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", httpserver.ErrInvalidMediaPath
	}

	normalized := strings.ReplaceAll(rel, "\\", "/")
	clean := path.Clean("/" + normalized)
	if clean == "/" {
		return "", httpserver.ErrInvalidMediaPath
	}

	full := filepath.Join(rootAbs, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", httpserver.ErrInvalidMediaPath
	}

	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", httpserver.ErrInvalidMediaPath
	}
	return fullAbs, nil
}

// readiness memeriksa database dan keberadaan batch yang punya media siap.
type readiness struct {
	store *store.Store
	media *mediafs.Manager
}

func (r readiness) Ready(ctx context.Context) (bool, string) {
	if err := r.store.Read().PingContext(ctx); err != nil {
		return false, "db_unavailable"
	}

	var label string
	err := r.store.Read().QueryRowContext(ctx, `
SELECT g.label
FROM generations g
WHERE g.status NOT IN ('building', 'failed')
  AND EXISTS (
    SELECT 1 FROM renditions r
    WHERE r.generation_id = g.id AND r.status = 'ready'
  )
ORDER BY CASE WHEN g.status = 'active' THEN 0 ELSE 1 END,
         g.created_at DESC, g.id DESC
LIMIT 1`).Scan(&label)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, "no_media"
	case err != nil:
		return false, "db_unavailable"
	}

	if !r.media.Exists(label) {
		return false, "generation_missing_on_disk"
	}
	return true, label
}
