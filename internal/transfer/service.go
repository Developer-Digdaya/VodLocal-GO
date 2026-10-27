package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

// ErrBusy is returned when a sync, import, or export is already running.
var ErrBusy = errors.New("transfer: masih ada proses sync/transfer aktif")

// ErrNoContent is returned when no exportable content was selected.
var ErrNoContent = errors.New("transfer: tidak ada konten yang dipilih")

// ErrInsufficientSpace is returned when the destination cannot hold the package.
var ErrInsufficientSpace = errors.New("transfer: ruang penyimpanan tidak mencukupi")

// ErrAlreadyExists is returned when a package item is already available in
// another generation on this server.
var ErrAlreadyExists = errors.New("transfer: konten sudah ada di server ini")

// Config wires the transfer service to the app database and media manager.
type Config struct {
	Store  *store.Store
	Media  *mediafs.Manager
	Logger *slog.Logger
	Now    func() time.Time
}

// Service owns portable package export/import and offline package discovery.
type Service struct {
	store *store.Store
	media *mediafs.Manager
	log   *slog.Logger
	now   func() time.Time

	mu   sync.Mutex
	jobs map[int64]context.CancelFunc
}

// New returns a transfer service. Store and Media are required.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("transfer: Store wajib")
	}
	if cfg.Media == nil {
		return nil, errors.New("transfer: Media wajib")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{store: cfg.Store, media: cfg.Media, log: logger, now: now, jobs: make(map[int64]context.CancelFunc)}, nil
}

// Cancel stops a running export/import. It reports an error when runID does not
// belong to a transfer job currently owned by this process.
func (s *Service) Cancel(runID int64) error {
	s.mu.Lock()
	cancel, ok := s.jobs[runID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("transfer: run %d bukan transfer aktif", runID)
	}
	cancel()
	return nil
}

func (s *Service) addJob(runID int64, cancel context.CancelFunc) {
	s.mu.Lock()
	s.jobs[runID] = cancel
	s.mu.Unlock()
}

func (s *Service) removeJob(runID int64) {
	s.mu.Lock()
	delete(s.jobs, runID)
	s.mu.Unlock()
}

func terminalStatus(ctx context.Context) string {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	return "failed"
}

// Recover marks transfer runs left active by a previous process as failed.
// Without this, the one-active-run index would block all later syncs after a
// crash or restart during an export/import.
func (s *Service) Recover(ctx context.Context) error {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT id, generation_id FROM sync_runs
 WHERE status IN ('queued','running','verifying','switching','cleanup')
   AND COALESCE(phase, '') IN ('export','import')`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var runIDs []int64
	var genIDs []int64
	for rows.Next() {
		var runID int64
		var gen sql.NullInt64
		if err := rows.Scan(&runID, &gen); err != nil {
			return err
		}
		runIDs = append(runIDs, runID)
		if gen.Valid {
			genIDs = append(genIDs, gen.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.stamp()
	for _, runID := range runIDs {
		if _, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs SET status = 'failed', phase = 'failed', finished_at = ?,
       error_message = 'transfer terputus saat server berhenti'
 WHERE id = ?`, now, runID); err != nil {
			return err
		}
	}
	for _, genID := range genIDs {
		if _, err := s.store.Write().ExecContext(ctx, `
UPDATE generations SET status = 'failed',
       note = 'import terputus saat server berhenti'
 WHERE id = ? AND status = 'building'`, genID); err != nil {
			return err
		}
	}
	if len(runIDs) > 0 {
		s.log.Warn("transfer: run lama ditandai gagal setelah restart", "runs", len(runIDs))
	}
	return nil
}

// LocalContent is one downloaded title available for export.
type LocalContent struct {
	ItemID          int64  `json:"item_id"`
	GenerationID    int64  `json:"generation_id"`
	GenerationLabel string `json:"generation_label"`
	Provider        string `json:"provider"`
	Category        string `json:"category"`
	ExternalID      string `json:"external_id"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	PosterURL       string `json:"poster_url"`
	EpisodesDone    int64  `json:"episodes_done"`
	EpisodesTotal   int64  `json:"episodes_total"`
	Renditions      int64  `json:"renditions"`
	Bytes           int64  `json:"bytes"`
	// Copies adalah jumlah generation yang memiliki judul yang sama. Hanya
	// generation terbaru yang ditampilkan pada daftar export.
	Copies int `json:"copies"`
	// AlreadyExported menandai judul yang sudah ada di salah satu paket pada
	// drive tujuan. Nilainya hanya diisi saat daftar diminta dengan drive.
	AlreadyExported bool `json:"already_exported"`
	// ExportedPackage adalah package id tempat judul itu ditemukan.
	ExportedPackage string `json:"exported_package,omitempty"`
}

// ListLocalContent returns completed generation items that can be exported.
// page is 1-based and perPage defaults to/max 30/200.
func (s *Service) ListLocalContent(ctx context.Context, page, perPage int) ([]LocalContent, int64, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 200 {
		perPage = 200
	}

	var total int64
	if err := s.store.Read().QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
  SELECT 1 FROM generation_items
   WHERE status = 'done'
   GROUP BY provider_id, series_external_id
)`).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Satu judul hanya boleh muncul sekali: generation terbaru menang. Ini
	// mencegah judul yang sama dipilih dari dua batch lalu diexport dua kali.
	rows, err := s.store.Read().QueryContext(ctx, `
WITH done_items AS (
  SELECT gi.*,
         ROW_NUMBER() OVER (
           PARTITION BY gi.provider_id, gi.series_external_id
           ORDER BY gi.id DESC
         ) AS rn
    FROM generation_items gi
   WHERE gi.status = 'done'
)
SELECT gi.id, gi.generation_id, g.label, COALESCE(pr.code, ''), gi.category,
       gi.series_external_id, gi.series_title, gi.status,
       COALESCE(t.poster_path, ''), gi.episodes_done, gi.total_episodes,
       (SELECT COUNT(*) FROM done_items d
         WHERE d.provider_id = gi.provider_id
           AND d.series_external_id = gi.series_external_id) AS copies,
       COUNT(r.id), COALESCE(SUM(r.total_bytes), 0)
  FROM done_items gi
  JOIN generations g ON g.id = gi.generation_id
  JOIN providers pr ON pr.id = gi.provider_id
  LEFT JOIN titles t ON t.provider_id = gi.provider_id AND t.external_id = gi.series_external_id
  LEFT JOIN playables p ON p.title_id = t.id
  LEFT JOIN renditions r ON r.playable_id = p.id
       AND r.generation_id = gi.generation_id AND r.status = 'ready'
 WHERE gi.rn = 1
 GROUP BY gi.id
 ORDER BY lower(gi.series_title) ASC, gi.id DESC
 LIMIT ? OFFSET ?`, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]LocalContent, 0, perPage)
	for rows.Next() {
		var (
			item   LocalContent
			poster string
		)
		if err := rows.Scan(&item.ItemID, &item.GenerationID, &item.GenerationLabel,
			&item.Provider, &item.Category, &item.ExternalID, &item.Title, &item.Status,
			&poster, &item.EpisodesDone, &item.EpisodesTotal, &item.Copies,
			&item.Renditions, &item.Bytes); err != nil {
			return nil, 0, err
		}
		if poster != "" {
			item.PosterURL = "/media/" + item.GenerationLabel + "/" + strings.TrimLeft(poster, "/")
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// ListPackages scans root/VOD-EXPORT for packages and marks items that already
// have a ready rendition on this server.
func (s *Service) ListPackages(ctx context.Context, root string) ([]PackageInfo, error) {
	packages, err := ScanPackages(root)
	if err != nil {
		return nil, err
	}
	for pi := range packages {
		for ii := range packages[pi].Items {
			item := &packages[pi].Items[ii]
			label, exists, err := s.existingContent(ctx, item.Provider, item.ExternalID)
			if err != nil {
				return nil, err
			}
			item.Existing = exists
			item.ExistingGeneration = label
		}
	}
	return packages, nil
}

// AllLocalContentIDs returns every unique downloadable title id for the
// "export all" action. The result uses the same deduplication rule as the
// paginated list.
func (s *Service) AllLocalContentIDs(ctx context.Context) ([]int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	rows, err := s.store.Read().QueryContext(ctx, `
WITH done_items AS (
  SELECT gi.*,
         ROW_NUMBER() OVER (
           PARTITION BY gi.provider_id, gi.series_external_id
           ORDER BY gi.id DESC
         ) AS rn
    FROM generation_items gi
   WHERE gi.status = 'done'
)
SELECT gi.id
  FROM done_items gi
 WHERE gi.rn = 1
 ORDER BY lower(gi.series_title) ASC, gi.id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LocalContentIDsByBatch returns all deduplicated generation_items ids for a
// generation label, used by the "select whole batch" checkbox.
func (s *Service) LocalContentIDsByBatch(ctx context.Context, label string) ([]int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	rows, err := s.store.Read().QueryContext(ctx, `
WITH done_items AS (
  SELECT gi.*,
         ROW_NUMBER() OVER (
           PARTITION BY gi.provider_id, gi.series_external_id
           ORDER BY gi.id DESC
         ) AS rn
    FROM generation_items gi
   WHERE gi.status = 'done'
)
SELECT gi.id
  FROM done_items gi
  JOIN generations g ON g.id = gi.generation_id
 WHERE gi.rn = 1 AND g.label = ?
 ORDER BY gi.id DESC`, label)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) existingContent(ctx context.Context, provider, externalID string) (string, bool, error) {
	if provider == "" || externalID == "" {
		return "", false, nil
	}
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	var label string
	err := s.store.Read().QueryRowContext(ctx, `
SELECT g.label
  FROM titles t
  JOIN providers pr ON pr.id = t.provider_id
  JOIN playables p ON p.title_id = t.id
  JOIN renditions r ON r.playable_id = p.id
  JOIN generations g ON g.id = r.generation_id
 WHERE pr.code = ? AND t.external_id = ? AND r.status = 'ready'
   AND g.status NOT IN ('building', 'failed')
 ORDER BY g.created_at DESC, g.id DESC
 LIMIT 1`, provider, externalID).Scan(&label)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return label, true, nil
	}
}

// Drives returns platform-appropriate destinations/sources.
func (s *Service) Drives() ([]Drive, error) {
	return ListDrives()
}

func (s *Service) stamp() string {
	return s.now().UTC().Format("2006-01-02T15:04:05Z")
}

func (s *Service) sqlCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

func (s *Service) ensureNoActive(ctx context.Context) error {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	var id int64
	err := s.store.Read().QueryRowContext(ctx, `
SELECT id FROM sync_runs
 WHERE status IN ('queued','running','verifying','switching','cleanup')
 ORDER BY id DESC LIMIT 1`).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	default:
		return ErrBusy
	}
}

func (s *Service) createRun(ctx context.Context, generationID *int64, phase, user string) (int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	now := s.stamp()
	var gen any
	if generationID != nil {
		gen = *generationID
	}
	var id int64
	err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO sync_runs (generation_id, trigger_source, scope, status, phase, triggered_by,
                       total_providers, started_at, created_at)
VALUES (?, 'manual', 'selection', 'running', ?, ?, 1, ?, ?) RETURNING id`,
		gen, phase, user, now, now).Scan(&id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return 0, ErrBusy
		}
		return 0, err
	}
	return id, nil
}

func (s *Service) updateRun(ctx context.Context, runID int64, phase string, totalFiles, doneFiles int, bytes int64) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	_, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs
   SET phase = ?, total_renditions = ?, renditions_ready = ?, bytes_downloaded = ?
 WHERE id = ?`, phase, totalFiles, doneFiles, bytes, runID)
	if err != nil {
		s.log.Warn("transfer: gagal memperbarui progres run", "run_id", runID, "err", err)
	}
}

func (s *Service) finishRun(ctx context.Context, runID int64, status string, totalTitles, totalPlayables, totalFiles, doneFiles int, bytes int64, cause error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	var msg any
	if cause != nil {
		msg = truncateText(cause.Error(), 2000)
	}
	phase := "done"
	if status == "failed" || status == "cancelled" {
		phase = status
	}
	_, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs
   SET status = ?, phase = ?, finished_at = ?, total_titles = ?, total_playables = ?,
       total_renditions = ?, renditions_ready = ?, bytes_downloaded = ?, error_message = ?
 WHERE id = ?`, status, phase, s.stamp(), totalTitles, totalPlayables,
		totalFiles, doneFiles, bytes, msg, runID)
	if err != nil {
		s.log.Warn("transfer: gagal menutup run", "run_id", runID, "err", err)
	}
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nullInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
