package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// FilePlan is one source file that will be copied into a package.
type FilePlan struct {
	Target string
	Source string
	Size   int64
}

type exportPlan struct {
	Manifest Manifest
	Files    []FilePlan
	// itemFiles maps item index -> package paths. The manifest Item.Files field
	// is filled after hashes are known.
	itemFiles [][]string
}

// StartExport validates the selected content and starts a background export to
// destination (a drive root or mounted folder). The returned ID is a sync_runs
// row that can be followed from the existing sync UI.
func (s *Service) StartExport(ctx context.Context, itemIDs []int64, destination, user string) (int64, error) {
	itemIDs = uniquePositive(itemIDs)
	if len(itemIDs) == 0 {
		return 0, ErrNoContent
	}
	if strings.TrimSpace(destination) == "" {
		return 0, errors.New("transfer: drive tujuan belum dipilih")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return 0, fmt.Errorf("transfer: drive tujuan tidak dapat diakses: %s", destination)
	}
	if err := s.ensureNoActive(ctx); err != nil {
		return 0, err
	}

	runID, err := s.createRun(ctx, nil, "export", user)
	if err != nil {
		return 0, err
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	s.addJob(runID, cancel)
	go func() {
		defer s.removeJob(runID)
		if err := s.runExport(jobCtx, runID, itemIDs, abs); err != nil {
			s.log.Error("transfer: export gagal", "run_id", runID, "err", err)
		}
	}()
	return runID, nil
}

func (s *Service) runExport(ctx context.Context, runID int64, itemIDs []int64, destination string) error {
	plan, err := s.buildExportPlan(ctx, itemIDs)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		return err
	}
	if len(plan.Files) == 0 {
		err := errors.New("transfer: konten tidak memiliki file media")
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		return err
	}
	var totalBytes int64
	for _, f := range plan.Files {
		totalBytes += f.Size
	}
	if free, err := freeBytes(destination); err == nil && uint64(totalBytes) > free {
		err := fmt.Errorf("%w: butuh %d byte, tersedia %d", ErrInsufficientSpace, totalBytes, free)
		s.finishRun(ctx, runID, "failed", len(plan.Manifest.Items), 0, len(plan.Files), 0, 0, err)
		return err
	}

	packageRoot := filepath.Join(destination, PackageRootName)
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		return err
	}
	packageID := fmt.Sprintf("VOD-EXPORT-%s-%d", s.now().UTC().Format("20060102-150405"), runID)
	finalDir := filepath.Join(packageRoot, packageID)
	tmpDir := filepath.Join(packageRoot, ".tmp-"+packageID)
	if err := os.RemoveAll(tmpDir); err != nil {
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "media"), 0o755); err != nil {
		s.finishRun(ctx, runID, "failed", 0, 0, 0, 0, 0, err)
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	totalFiles := len(plan.Files)
	plan.Manifest.PackageID = packageID
	plan.Manifest.CreatedAt = s.stamp()
	s.updateRun(ctx, runID, "export", totalFiles, 0, 0)

	var (
		doneFiles int
		totalSize int64
	)
	for i := range plan.Files {
		if err := ctx.Err(); err != nil {
			s.finishRun(ctx, runID, terminalStatus(ctx), len(plan.Manifest.Items), 0, totalFiles, doneFiles, totalSize, err)
			return err
		}
		file := &plan.Files[i]
		dst := filepath.Join(tmpDir, filepath.FromSlash("media/"+file.Target))
		info, err := copyAndHash(file.Source, dst)
		if err != nil {
			s.finishRun(ctx, runID, "failed", len(plan.Manifest.Items), 0, totalFiles, doneFiles, totalSize, err)
			return err
		}
		if info.Size != file.Size {
			err := fmt.Errorf("transfer: ukuran file berubah saat export: %s", file.Target)
			s.finishRun(ctx, runID, "failed", len(plan.Manifest.Items), 0, totalFiles, doneFiles, totalSize, err)
			return err
		}
		plan.Manifest.Files = append(plan.Manifest.Files, File{
			Path:   "media/" + file.Target,
			Target: file.Target,
			Size:   info.Size,
			SHA256: info.SHA256,
		})
		doneFiles++
		totalSize += info.Size
		if doneFiles == totalFiles || doneFiles%progressStep(totalFiles) == 0 {
			s.updateRun(ctx, runID, "export", totalFiles, doneFiles, totalSize)
		}
	}

	// Fill Item.Files after the deterministic file list is complete.
	for i := range plan.Manifest.Items {
		plan.Manifest.Items[i].Files = plan.itemFiles[i]
	}
	plan.Manifest.TotalBytes = totalSize
	if err := WriteManifest(tmpDir, &plan.Manifest); err != nil {
		s.finishRun(ctx, runID, "failed", len(plan.Manifest.Items), 0, totalFiles, doneFiles, totalSize, err)
		return err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		s.finishRun(ctx, runID, "failed", len(plan.Manifest.Items), 0, totalFiles, doneFiles, totalSize, err)
		return err
	}
	s.finishRun(ctx, runID, "completed", len(plan.Manifest.Items), 0, totalFiles, totalFiles, totalSize, nil)
	s.log.Info("transfer: export selesai",
		"run_id", runID, "package", finalDir, "items", len(plan.Manifest.Items),
		"files", totalFiles, "bytes", totalSize)
	return nil
}

func progressStep(total int) int {
	if total <= 100 {
		return 1
	}
	return total / 100
}

// buildExportPlan loads metadata and media paths for the selected
// generation_items ids. It deliberately refuses duplicate titles.
func (s *Service) buildExportPlan(ctx context.Context, itemIDs []int64) (*exportPlan, error) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
	args := make([]any, 0, len(itemIDs))
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT gi.id, gi.generation_id, g.label, pr.code, gi.category, gi.series_external_id,
       t.id, t.kind, t.title, t.slug, COALESCE(t.synopsis, ''),
       COALESCE(t.release_year, 0), COALESCE(t.content_rating, ''), COALESCE(t.language, ''),
       COALESCE(t.country, ''), COALESCE(t.duration_seconds, 0),
       COALESCE(t.poster_path, ''), COALESCE(t.backdrop_path, '')
  FROM generation_items gi
  JOIN generations g ON g.id = gi.generation_id
  JOIN providers pr ON pr.id = gi.provider_id
  JOIN titles t ON t.provider_id = gi.provider_id AND t.external_id = gi.series_external_id
 WHERE gi.id IN (`+placeholders+`) AND gi.status = 'done'
 ORDER BY gi.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		itemID, generationID, titleID int64
		label, provider, category     string
		externalID, kind, name, slug  string
		synopsis, rating, language    string
		country, poster, backdrop     string
		year, duration                int
	}
	selected := make([]row, 0, len(itemIDs))
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.itemID, &r.generationID, &r.label, &r.provider, &r.category,
			&r.externalID, &r.titleID, &r.kind, &r.name, &r.slug, &r.synopsis, &r.year,
			&r.rating, &r.language, &r.country, &r.duration, &r.poster, &r.backdrop); err != nil {
			return nil, err
		}
		selected = append(selected, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(selected) != len(itemIDs) {
		return nil, fmt.Errorf("transfer: %d item export tidak ditemukan atau belum selesai", len(itemIDs)-len(selected))
	}

	plan := &exportPlan{
		Manifest: Manifest{Format: Format},
	}
	seenTitles := make(map[string]struct{})
	fileByTarget := make(map[string]string)
	for i, r := range selected {
		titleKey := r.provider + "\x00" + r.externalID
		if _, exists := seenTitles[titleKey]; exists {
			return nil, fmt.Errorf("transfer: judul %s dipilih lebih dari satu kali", r.externalID)
		}
		seenTitles[titleKey] = struct{}{}

		item := Item{
			Provider: r.provider,
			Category: r.category,
			Title: TitleData{
				ExternalID: r.externalID, Kind: r.kind, Name: r.name, Slug: r.slug,
				Synopsis: r.synopsis, ReleaseYear: r.year, ContentRating: r.rating,
				Language: r.language, Country: r.country, DurationSeconds: r.duration,
				PosterPath: r.poster, BackdropPath: r.backdrop,
			},
		}
		genres, err := s.loadGenres(ctx, r.titleID)
		if err != nil {
			return nil, err
		}
		item.Title.Genres = genres

		episodes, episodeByID, err := s.loadEpisodes(ctx, r.titleID)
		if err != nil {
			return nil, err
		}
		item.Episodes = episodes

		playables, err := s.loadPlayables(ctx, r.titleID, r.generationID, episodeByID)
		if err != nil {
			return nil, err
		}
		item.Playables = playables

		itemFiles := make([]string, 0, 8)
		addPath := func(target, source string) error {
			target, err = cleanRel(target)
			if err != nil {
				return err
			}
			abs := source
			if abs == "" {
				abs, err = s.media.Resolve(r.label, target)
				if err != nil {
					return err
				}
			}
			info, err := os.Stat(abs)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("transfer: bukan file: %s", target)
			}
			if prev, ok := fileByTarget[target]; ok {
				if prev != abs {
					return fmt.Errorf("transfer: target file bentrok: %s", target)
				}
				return nil
			}
			fileByTarget[target] = abs
			plan.Files = append(plan.Files, FilePlan{Target: target, Source: abs, Size: info.Size()})
			itemFiles = append(itemFiles, "media/"+target)
			return nil
		}

		for _, rel := range []string{item.Title.PosterPath, item.Title.BackdropPath} {
			if rel != "" {
				if err := addPath(rel, ""); err != nil {
					return nil, err
				}
			}
		}
		for _, ep := range item.Episodes {
			if ep.StillPath != "" {
				if err := addPath(ep.StillPath, ""); err != nil {
					return nil, err
				}
			}
		}
		for pi := range item.Playables {
			p := &item.Playables[pi]
			for ri := range p.Renditions {
				rend := &p.Renditions[ri]
				abs, err := s.media.Resolve(r.label, rend.PlaylistPath)
				if err != nil {
					return nil, err
				}
				if _, err := os.Stat(abs); err != nil {
					return nil, fmt.Errorf("transfer: playlist %s tidak ada: %w", rend.PlaylistPath, err)
				}
				base := filepath.Dir(abs)
				relBase := path.Dir(rend.PlaylistPath)
				err = filepath.WalkDir(base, func(p string, d os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if d.IsDir() {
						return nil
					}
					if strings.HasSuffix(d.Name(), ".part") {
						return nil
					}
					rel, err := filepath.Rel(base, p)
					if err != nil {
						return err
					}
					target := path.Join(relBase, filepath.ToSlash(rel))
					return addPath(target, p)
				})
				if err != nil {
					return nil, err
				}
			}
			for _, sub := range p.Subtitles {
				if sub.FilePath != "" {
					if err := addPath(sub.FilePath, ""); err != nil {
						return nil, err
					}
				}
			}
		}
		if len(itemFiles) == 0 {
			return nil, fmt.Errorf("transfer: %s tidak memiliki file media", r.name)
		}
		sort.Strings(itemFiles)
		plan.Manifest.Items = append(plan.Manifest.Items, item)
		plan.itemFiles = append(plan.itemFiles, itemFiles)
		_ = i
	}
	return plan, nil
}

func (s *Service) loadGenres(ctx context.Context, titleID int64) ([]Genre, error) {
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT g.name, g.slug
  FROM title_genres tg JOIN genres g ON g.id = tg.genre_id
 WHERE tg.title_id = ? ORDER BY g.name`, titleID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Genre
	for rows.Next() {
		var g Genre
		if err := rows.Scan(&g.Name, &g.Slug); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) loadEpisodes(ctx context.Context, titleID int64) ([]Episode, map[int64]string, error) {
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT id, external_id, season_number, episode_number, COALESCE(name, ''),
       COALESCE(synopsis, ''), COALESCE(duration_seconds, 0), COALESCE(air_date, ''),
       COALESCE(still_path, '')
  FROM episodes WHERE title_id = ? ORDER BY season_number, episode_number`, titleID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var episodes []Episode
	byID := make(map[int64]string)
	for rows.Next() {
		var (
			id int64
			ep Episode
		)
		if err := rows.Scan(&id, &ep.ExternalID, &ep.SeasonNumber, &ep.EpisodeNumber,
			&ep.Name, &ep.Synopsis, &ep.DurationSeconds, &ep.AirDate, &ep.StillPath); err != nil {
			return nil, nil, err
		}
		episodes = append(episodes, ep)
		byID[id] = ep.ExternalID
	}
	return episodes, byID, rows.Err()
}

func (s *Service) loadPlayables(ctx context.Context, titleID, generationID int64, episodeByID map[int64]string) ([]Playable, error) {
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT id, episode_id, COALESCE(duration_seconds, 0), is_encrypted
  FROM playables WHERE title_id = ? ORDER BY id`, titleID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Playable
	for rows.Next() {
		var (
			id        int64
			episodeID sql.NullInt64
			duration  int
			encrypted int
		)
		if err := rows.Scan(&id, &episodeID, &duration, &encrypted); err != nil {
			return nil, err
		}
		p := Playable{DurationSeconds: duration, Encrypted: encrypted != 0}
		if episodeID.Valid {
			p.EpisodeExternalID = episodeByID[episodeID.Int64]
		}
		if err := s.loadRenditions(ctx, &p, id, generationID); err != nil {
			return nil, err
		}
		if len(p.Renditions) > 0 {
			if err := s.loadSubtitles(ctx, &p, id, generationID); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (s *Service) loadRenditions(ctx context.Context, p *Playable, playableID, generationID int64) error {
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT quality, COALESCE(bitrate_kbps, 0), COALESCE(width, 0), COALESCE(height, 0),
       COALESCE(container, 'ts'), COALESCE(playlist_path, ''), COALESCE(segment_count, 0),
       COALESCE(total_bytes, 0), COALESCE(duration_seconds, 0)
  FROM renditions
 WHERE playable_id = ? AND generation_id = ? AND status = 'ready'
 ORDER BY quality`, playableID, generationID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r Rendition
		if err := rows.Scan(&r.Quality, &r.BitrateKbps, &r.Width, &r.Height,
			&r.Container, &r.PlaylistPath, &r.SegmentCount, &r.TotalBytes, &r.DurationSeconds); err != nil {
			return err
		}
		if r.PlaylistPath == "" {
			continue
		}
		p.Renditions = append(p.Renditions, r)
	}
	return rows.Err()
}

func (s *Service) loadSubtitles(ctx context.Context, p *Playable, playableID, generationID int64) error {
	rows, err := s.store.Read().QueryContext(ctx, `
SELECT lang, COALESCE(label, ''), format, file_path, COALESCE(bytes, 0)
  FROM subtitles
 WHERE playable_id = ? AND generation_id = ? AND status = 'ready'
 ORDER BY lang`, playableID, generationID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sub Subtitle
		if err := rows.Scan(&sub.Lang, &sub.Label, &sub.Format, &sub.FilePath, &sub.Bytes); err != nil {
			return err
		}
		p.Subtitles = append(p.Subtitles, sub)
	}
	return rows.Err()
}

func uniquePositive(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, v := range values {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
