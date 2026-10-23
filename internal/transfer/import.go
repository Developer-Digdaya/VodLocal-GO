package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ImportResult is the outcome of one background import.
type ImportResult struct {
	RunID        int64
	GenerationID int64
	Label        string
}

// StartImport validates an offline package and starts importing selected items
// into a new generation. An empty indexes list means all items in the package.
func (s *Service) StartImport(ctx context.Context, packageDir string, indexes []int, user string) (int64, error) {
	abs, err := filepath.Abs(strings.TrimSpace(packageDir))
	if err != nil {
		return 0, err
	}
	manifest, err := ReadManifest(abs)
	if err != nil {
		return 0, err
	}
	selected, err := selectItems(manifest, indexes)
	if err != nil {
		return 0, err
	}
	if err := s.ensureNoActive(ctx); err != nil {
		return 0, err
	}
	if free, err := freeBytes(s.media.Root()); err == nil && uint64(manifest.TotalBytes) > free {
		return 0, fmt.Errorf("%w: butuh %d byte, tersedia %d", ErrInsufficientSpace, manifest.TotalBytes, free)
	}
	for _, idx := range selected {
		item := manifest.Items[idx]
		var exists int
		if err := s.store.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM providers WHERE code = ?`, item.Provider).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			return 0, fmt.Errorf("%w: provider %q tidak terdaftar di server ini", ErrInvalidPackage, item.Provider)
		}
	}

	// Reject duplicate logical titles before creating any database row.
	seenTitles := make(map[string]struct{}, len(selected))
	for _, idx := range selected {
		item := manifest.Items[idx]
		key := item.Provider + "\x00" + item.Title.ExternalID
		if _, ok := seenTitles[key]; ok {
			return 0, fmt.Errorf("transfer: judul %s muncul lebih dari sekali di paket", item.Title.Name)
		}
		seenTitles[key] = struct{}{}
	}

	genID, label, err := s.createGeneration(ctx)
	if err != nil {
		return 0, err
	}
	runID, err := s.createRun(ctx, &genID, "import", user)
	if err != nil {
		s.markGenerationFailed(genID, err)
		return 0, err
	}

	itemIDs, err := s.seedImportItems(ctx, genID, manifest, selected)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		s.markGenerationFailed(genID, err)
		return 0, err
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	s.addJob(runID, cancel)
	go func() {
		defer s.removeJob(runID)
		if err := s.runImport(jobCtx, runID, genID, label, abs, manifest, selected, itemIDs); err != nil {
			s.log.Error("transfer: import gagal", "run_id", runID, "package", abs, "err", err)
		}
	}()
	return runID, nil
}

func selectItems(m *Manifest, indexes []int) ([]int, error) {
	if len(indexes) == 0 {
		out := make([]int, len(m.Items))
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	seen := make(map[int]struct{}, len(indexes))
	out := make([]int, 0, len(indexes))
	for _, idx := range indexes {
		if idx < 0 || idx >= len(m.Items) {
			return nil, fmt.Errorf("%w: item index %d di luar paket", ErrInvalidPackage, idx)
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	if len(out) == 0 {
		return nil, ErrNoContent
	}
	return out, nil
}

func (s *Service) seedImportItems(ctx context.Context, genID int64, m *Manifest, selected []int) (map[int]int64, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	now := s.stamp()
	out := make(map[int]int64, len(selected))
	for _, idx := range selected {
		item := m.Items[idx]
		var provID int64
		if err := s.store.Read().QueryRowContext(ctx,
			`SELECT id FROM providers WHERE code = ?`, item.Provider).Scan(&provID); err != nil {
			return nil, fmt.Errorf("transfer: provider %q tidak dikenal", item.Provider)
		}
		var id int64
		totalEpisodes := len(item.Episodes)
		if item.Title.Kind == "movie" && totalEpisodes == 0 {
			totalEpisodes = 1
		}
		err := s.store.Write().QueryRowContext(ctx, `
INSERT INTO generation_items (generation_id, provider_id, category, series_external_id,
                              series_title, status, total_episodes, current_step,
                              created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?, 'import', ?, ?)
ON CONFLICT(generation_id, provider_id, series_external_id) DO UPDATE SET
    series_title = excluded.series_title,
    category = excluded.category,
    total_episodes = excluded.total_episodes,
    current_step = 'import',
    updated_at = excluded.updated_at
RETURNING id`, genID, provID, item.Category, item.Title.ExternalID,
			item.Title.Name, totalEpisodes, now, now).Scan(&id)
		if err != nil {
			return nil, err
		}
		out[idx] = id
	}
	return out, nil
}

func (s *Service) runImport(ctx context.Context, runID, genID int64, label, packageDir string, m *Manifest, selected []int, itemIDs map[int]int64) error {
	build, err := s.media.NewStaging(label)
	if err != nil {
		s.finishRun(ctx, runID, "failed", len(selected), 0, 0, 0, 0, err)
		s.markGenerationFailed(genID, err)
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()

	totalFiles := 0
	for _, idx := range selected {
		totalFiles += len(m.Items[idx].Files)
	}
	s.updateRun(ctx, runID, "import", totalFiles, 0, 0)
	for _, idx := range selected {
		s.setItemStatus(ctx, itemIDs[idx], "running", 0)
	}

	var (
		doneFiles  int
		totalSize  int64
		fileByPath = make(map[string]File, len(m.Files))
	)
	for _, f := range m.Files {
		fileByPath[f.Path] = f
	}

	for _, idx := range selected {
		item := m.Items[idx]
		itemTotal := len(item.Files)
		var itemSize int64
		for fi, rel := range item.Files {
			if err := ctx.Err(); err != nil {
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			file, ok := fileByPath[rel]
			if !ok {
				err := fmt.Errorf("transfer: file %q tidak ada di manifest", rel)
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			src, err := joinPackage(packageDir, file.Path)
			if err != nil {
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			dst, err := joinTarget(build.Dir(), file.Target)
			if err != nil {
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			info, err := copyAndHash(src, dst)
			if err != nil {
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			if info.Size != file.Size || info.SHA256 != file.SHA256 {
				err := fmt.Errorf("transfer: checksum berubah saat import: %s", file.Path)
				s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
				return err
			}
			doneFiles++
			itemSize += info.Size
			totalSize += info.Size
			progress := 0
			if itemTotal > 0 {
				progress = (fi + 1) * 100 / itemTotal
			}
			if fi+1 == itemTotal || (fi+1)%progressStep(itemTotal) == 0 {
				s.setItemProgress(ctx, itemIDs[idx], progress, itemSize, "import")
			}
			if doneFiles == totalFiles || doneFiles%progressStep(totalFiles) == 0 {
				s.updateRun(ctx, runID, "import", totalFiles, doneFiles, totalSize)
			}
		}
	}

	if err := build.Commit(); err != nil {
		s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
		return err
	}
	committed = true

	if err := s.persistImport(ctx, genID, m, selected, itemIDs, totalSize); err != nil {
		// The published directory is not referenced by a usable generation, so
		// remove it rather than leave orphan media behind.
		_ = s.media.Remove(label)
		s.failImport(ctx, runID, genID, itemIDs, selected, err, totalFiles, doneFiles)
		return err
	}

	totalPlayables := 0
	totalRenditions := 0
	for _, idx := range selected {
		for _, p := range m.Items[idx].Playables {
			totalPlayables++
			totalRenditions += len(p.Renditions)
		}
	}
	s.finishRun(ctx, runID, "completed", len(selected), totalPlayables, totalFiles, totalFiles, totalSize, nil)
	s.log.Info("transfer: import selesai",
		"run_id", runID, "generation", label, "items", len(selected),
		"files", totalFiles, "bytes", totalSize)
	return nil
}

func (s *Service) failImport(ctx context.Context, runID, genID int64, itemIDs map[int]int64, selected []int, cause error, totalFiles, doneFiles int) {
	for _, idx := range selected {
		if id, ok := itemIDs[idx]; ok {
			s.setItemFailed(ctx, id, cause)
		}
	}
	s.finishRun(ctx, runID, terminalStatus(ctx), len(selected), 0, totalFiles, doneFiles, 0, cause)
	s.markGenerationFailed(genID, cause)
}

func (s *Service) persistImport(ctx context.Context, genID int64, m *Manifest, selected []int, itemIDs map[int]int64, totalBytes int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	tx, err := s.store.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.stamp()

	for _, idx := range selected {
		item := m.Items[idx]
		provID, err := providerIDTx(ctx, tx, item.Provider)
		if err != nil {
			return err
		}
		titleID, err := upsertTitleTx(ctx, tx, provID, item.Title, now)
		if err != nil {
			return fmt.Errorf("transfer: simpan judul %s: %w", item.Title.ExternalID, err)
		}
		for _, genre := range item.Title.Genres {
			if err := upsertGenreTx(ctx, tx, titleID, genre); err != nil {
				return err
			}
		}

		episodeByExternal := make(map[string]int64, len(item.Episodes))
		for _, ep := range item.Episodes {
			id, err := upsertEpisodeTx(ctx, tx, titleID, ep, now)
			if err != nil {
				return fmt.Errorf("transfer: simpan episode %s: %w", ep.ExternalID, err)
			}
			episodeByExternal[ep.ExternalID] = id
		}

		for _, playable := range item.Playables {
			var episodeID *int64
			if playable.EpisodeExternalID != "" {
				id, ok := episodeByExternal[playable.EpisodeExternalID]
				if !ok {
					return fmt.Errorf("transfer: playable episode %q tidak dikenal", playable.EpisodeExternalID)
				}
				episodeID = &id
			}
			playableID, err := upsertPlayableTx(ctx, tx, titleID, episodeID, playable, now)
			if err != nil {
				return err
			}
			for _, r := range playable.Renditions {
				if err := upsertRenditionTx(ctx, tx, playableID, genID, r, now); err != nil {
					return err
				}
			}
			for _, sub := range playable.Subtitles {
				if err := upsertSubtitleTx(ctx, tx, playableID, genID, sub); err != nil {
					return err
				}
			}
		}

		if id := itemIDs[idx]; id > 0 {
			renditionCount := 0
			for _, playable := range item.Playables {
				renditionCount += len(playable.Renditions)
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE generation_items
   SET status = 'done', current_step = 'done', progress_percent = 100,
       episodes_done = total_episodes, renditions_ready = ?,
       bytes_downloaded = ?, finished_at = ?, updated_at = ?, error_message = NULL
 WHERE id = ?`, renditionCount, itemBytes(m, item), now, now, id); err != nil {
				return err
			}
		}
	}

	var fileCount int
	for _, idx := range selected {
		fileCount += len(m.Items[idx].Files)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE generations SET status = 'retired', retired_at = ? WHERE status = 'active' AND id <> ?`,
		now, genID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE generations
   SET status = 'active', activated_at = ?, file_count = ?, total_bytes = ?, note = ?
 WHERE id = ?`, now, fileCount, totalBytes, "import offline", genID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func providerIDTx(ctx context.Context, tx *sql.Tx, code string) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE code = ?`, code).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("transfer: provider %q tidak terdaftar", code)
		}
		return 0, err
	}
	return id, nil
}

func upsertTitleTx(ctx context.Context, tx *sql.Tx, provID int64, t TitleData, now string) (int64, error) {
	external := strings.TrimSpace(t.ExternalID)
	slug := strings.TrimSpace(t.Slug)
	if slug == "" {
		slug = external
	}
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = external
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO titles (provider_id, external_id, kind, title, slug, synopsis, release_year,
                    content_rating, language, country, poster_path, backdrop_path,
                    duration_seconds, is_published, fetched_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(provider_id, external_id) DO UPDATE SET
    kind = excluded.kind,
    title = excluded.title,
    slug = excluded.slug,
    synopsis = excluded.synopsis,
    release_year = excluded.release_year,
    content_rating = excluded.content_rating,
    language = excluded.language,
    country = excluded.country,
    poster_path = COALESCE(excluded.poster_path, titles.poster_path),
    backdrop_path = COALESCE(excluded.backdrop_path, titles.backdrop_path),
    duration_seconds = excluded.duration_seconds,
    is_published = 1,
    fetched_at = excluded.fetched_at,
    updated_at = excluded.updated_at
RETURNING id`,
		provID, external, t.Kind, name, slug, nullString(t.Synopsis), nullInt(t.ReleaseYear),
		nullString(t.ContentRating), nullString(t.Language), nullString(t.Country),
		nullString(t.PosterPath), nullString(t.BackdropPath), nullInt(t.DurationSeconds),
		now, now, now).Scan(&id)
	return id, err
}

func upsertGenreTx(ctx context.Context, tx *sql.Tx, titleID int64, genre Genre) error {
	name := strings.TrimSpace(genre.Name)
	slug := strings.TrimSpace(genre.Slug)
	if name == "" || slug == "" {
		return nil
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
INSERT INTO genres (name, slug) VALUES (?, ?)
ON CONFLICT(slug) DO UPDATE SET name = excluded.name
RETURNING id`, name, slug).Scan(&id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO title_genres (title_id, genre_id) VALUES (?, ?)`, titleID, id)
	return err
}

func upsertEpisodeTx(ctx context.Context, tx *sql.Tx, titleID int64, ep Episode, now string) (int64, error) {
	season := ep.SeasonNumber
	if season <= 0 {
		season = 1
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO episodes (title_id, external_id, season_number, episode_number, name, synopsis,
                      duration_seconds, air_date, still_path, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(title_id, external_id) DO UPDATE SET
    season_number = excluded.season_number,
    episode_number = excluded.episode_number,
    name = excluded.name,
    synopsis = excluded.synopsis,
    duration_seconds = excluded.duration_seconds,
    air_date = excluded.air_date,
    still_path = COALESCE(excluded.still_path, episodes.still_path),
    updated_at = excluded.updated_at
RETURNING id`,
		titleID, ep.ExternalID, season, ep.EpisodeNumber, nullString(ep.Name),
		nullString(ep.Synopsis), nullInt(ep.DurationSeconds), nullString(ep.AirDate),
		nullString(ep.StillPath), now, now).Scan(&id)
	return id, err
}

func upsertPlayableTx(ctx context.Context, tx *sql.Tx, titleID int64, episodeID *int64, p Playable, now string) (int64, error) {
	var (
		id  int64
		err error
	)
	if episodeID == nil {
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM playables WHERE title_id = ? AND episode_id IS NULL`, titleID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, NULL, ?, ?, ?, ?) RETURNING id`,
				titleID, nullInt(p.DurationSeconds), boolInt(p.Encrypted), now, now).Scan(&id)
			return id, err
		}
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM playables WHERE episode_id = ?`, *episodeID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				titleID, *episodeID, nullInt(p.DurationSeconds), boolInt(p.Encrypted), now, now).Scan(&id)
			return id, err
		}
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE playables SET duration_seconds = ?, is_encrypted = ?, updated_at = ? WHERE id = ?`,
		nullInt(p.DurationSeconds), boolInt(p.Encrypted), now, id)
	return id, err
}

func upsertRenditionTx(ctx context.Context, tx *sql.Tx, playableID, genID int64, r Rendition, now string) error {
	container := strings.TrimSpace(r.Container)
	if container == "" {
		container = "ts"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
                        container, playlist_path, segment_count, segments_done,
                        total_bytes, bytes_done, duration_seconds, status, verified_at,
                        created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?, ?)
ON CONFLICT(playable_id, generation_id, quality) DO UPDATE SET
    bitrate_kbps = excluded.bitrate_kbps,
    width = excluded.width,
    height = excluded.height,
    container = excluded.container,
    playlist_path = excluded.playlist_path,
    segment_count = excluded.segment_count,
    segments_done = excluded.segments_done,
    total_bytes = excluded.total_bytes,
    bytes_done = excluded.bytes_done,
    duration_seconds = excluded.duration_seconds,
    status = 'ready',
    verified_at = excluded.verified_at,
    last_error = NULL,
    updated_at = excluded.updated_at`,
		playableID, genID, r.Quality, nullInt(r.BitrateKbps), nullInt(r.Width), nullInt(r.Height),
		container, r.PlaylistPath, r.SegmentCount, r.SegmentCount,
		r.TotalBytes, r.TotalBytes, nullInt(r.DurationSeconds), now, now, now)
	return err
}

func upsertSubtitleTx(ctx context.Context, tx *sql.Tx, playableID, genID int64, sub Subtitle) error {
	format := strings.TrimSpace(sub.Format)
	if format == "" {
		format = "vtt"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO subtitles (playable_id, generation_id, lang, label, format, file_path, bytes, status)
VALUES (?, ?, ?, ?, ?, ?, ?, 'ready')
ON CONFLICT(playable_id, generation_id, lang) DO UPDATE SET
    label = excluded.label,
    format = excluded.format,
    file_path = excluded.file_path,
    bytes = excluded.bytes,
    status = 'ready'`,
		playableID, genID, sub.Lang, nullString(sub.Label), format, sub.FilePath, sub.Bytes)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Service) setItemStatus(ctx context.Context, id int64, status string, total int64) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	if _, err := s.store.Write().ExecContext(ctx, `
UPDATE generation_items SET status = ?, current_step = 'import', updated_at = ? WHERE id = ?`,
		status, s.stamp(), id); err != nil {
		s.log.Warn("transfer: gagal memperbarui item", "item_id", id, "err", err)
	}
}

func (s *Service) setItemProgress(ctx context.Context, id int64, percent int, bytes int64, step string) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	if _, err := s.store.Write().ExecContext(ctx, `
UPDATE generation_items
   SET status = 'running', progress_percent = ?, bytes_downloaded = ?,
       current_step = ?, updated_at = ?
 WHERE id = ?`, percent, bytes, step, s.stamp(), id); err != nil {
		s.log.Warn("transfer: gagal progres item", "item_id", id, "err", err)
	}
}

func (s *Service) setItemFailed(ctx context.Context, id int64, cause error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	_, err := s.store.Write().ExecContext(ctx, `
UPDATE generation_items SET status = 'failed', current_step = 'import',
       error_message = ?, finished_at = ?, updated_at = ? WHERE id = ?`,
		truncateText(cause.Error(), 2000), s.stamp(), s.stamp(), id)
	if err != nil {
		s.log.Warn("transfer: gagal menandai item gagal", "item_id", id, "err", err)
	}
}

func (s *Service) markGenerationFailed(genID int64, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.store.Write().ExecContext(ctx,
		`UPDATE generations SET status = 'failed', note = ? WHERE id = ?`,
		truncateText(cause.Error(), 2000), genID)
	if err != nil {
		s.log.Warn("transfer: gagal menandai generation gagal", "generation_id", genID, "err", err)
	}
}

func (s *Service) createGeneration(ctx context.Context) (int64, string, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()
	var maxID sql.NullInt64
	if err := s.store.Write().QueryRowContext(ctx, `SELECT MAX(id) FROM generations`).Scan(&maxID); err != nil {
		return 0, "", err
	}
	next := maxID.Int64 + 1
	if !maxID.Valid || next < 1 {
		next = 1
	}
	label := fmt.Sprintf("gen-%06d", next)
	if _, err := s.store.Write().ExecContext(ctx, `
INSERT INTO generations (id, label, root_path, status, created_at)
VALUES (?, ?, ?, 'building', ?)`, next, label, label, s.stamp()); err != nil {
		return 0, "", err
	}
	return next, label, nil
}

// Ensure fmt/os imports remain used on all platforms.
