package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

const (
	// defaultProgressLimit dan maxProgressLimit mengikuti §9.2.
	defaultProgressLimit = 20
	maxProgressLimit     = 100
)

// progressRequest adalah body POST /progress (§9.1). Field opsional memakai
// pointer agar bisa membedakan "tidak dikirim" dari nilai nol.
type progressRequest struct {
	PlayableID      *int64 `json:"playable_id"`
	PositionSeconds *int64 `json:"position_seconds"`
	DurationSeconds *int64 `json:"duration_seconds"`
	Completed       *bool  `json:"completed"`
}

// progressItem adalah satu baris riwayat tonton (§9.2).
type progressItem struct {
	PlayableID      int64  `json:"playable_id"`
	TitleID         int64  `json:"title_id"`
	Title           string `json:"title"`
	Episode         any    `json:"episode"`
	PosterURL       any    `json:"poster_url"`
	PositionSeconds int64  `json:"position_seconds"`
	DurationSeconds any    `json:"duration_seconds"`
	ProgressPercent int64  `json:"progress_percent"`
	Completed       bool   `json:"completed"`
	UpdatedAt       any    `json:"updated_at"`
}

// progressEpisode adalah ringkasan episode pada riwayat tonton.
type progressEpisode struct {
	SeasonNumber  int64 `json:"season_number"`
	EpisodeNumber int64 `json:"episode_number"`
}

// progressMeta adalah data.meta GET /progress.
type progressMeta struct {
	Limit int64 `json:"limit"`
	Total int64 `json:"total"`
}

// progressData adalah data GET /progress.
type progressData struct {
	Items []progressItem `json:"items"`
	Meta  progressMeta   `json:"meta"`
}

// handleProgressSave melayani POST /progress (upsert berdasarkan device).
func (s *server) handleProgressSave(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	var req progressRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	fields := map[string]string{}
	switch {
	case req.PlayableID == nil:
		fields["body.playable_id"] = "wajib diisi"
	case *req.PlayableID < 1:
		fields["body.playable_id"] = "tidak valid"
	}
	switch {
	case req.PositionSeconds == nil:
		fields["body.position_seconds"] = "wajib diisi"
	case *req.PositionSeconds < 0:
		fields["body.position_seconds"] = "minimal 0"
	}
	if req.DurationSeconds != nil && *req.DurationSeconds < 0 {
		fields["body.duration_seconds"] = "minimal 0"
	}
	if len(fields) > 0 {
		response.Error(w, response.CodeProgressInvalid, detailsFromFields(fields))
		return
	}

	ctx := r.Context()
	playableID := *req.PlayableID

	var exists int
	err := s.read.QueryRowContext(ctx, `SELECT 1 FROM playables WHERE id = ?`, playableID).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodePlayableNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}

	completed := false
	if req.Completed != nil {
		completed = *req.Completed
	}

	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO playback_progress (device_id, playable_id, position_seconds, duration_seconds, completed, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id, playable_id) DO UPDATE SET
		     position_seconds = excluded.position_seconds,
		     duration_seconds = excluded.duration_seconds,
		     completed = excluded.completed,
		     updated_at = excluded.updated_at`,
		device.ID, playableID, *req.PositionSeconds, durationArg(req.DurationSeconds),
		boolToInt(completed), s.timestamp(),
	); err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, nil)
}

// handleProgressList melayani GET /progress.
func (s *server) handleProgressList(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	fields := map[string]string{}
	limit := int64(defaultProgressLimit)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			fields["limit"] = "harus berupa angka"
		case n < 1:
			fields["limit"] = "minimal 1"
		case n > maxProgressLimit:
			fields["limit"] = "maksimal 100"
		default:
			limit = n
		}
	}
	if s.validationFailed(w, fields) {
		return
	}

	ctx := r.Context()
	var total int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playback_progress WHERE device_id = ?`, device.ID,
	).Scan(&total); err != nil {
		s.internalError(w, r, err)
		return
	}

	rows, err := s.read.QueryContext(ctx,
		progressSelect+` WHERE pp.device_id = ?
		 ORDER BY pp.updated_at DESC, pp.playable_id DESC
		 LIMIT ?`, device.ID, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer rows.Close()

	items := make([]progressItem, 0, limit)
	for rows.Next() {
		item, err := scanProgressItem(rows)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, progressData{Items: items, Meta: progressMeta{Limit: limit, Total: total}})
}

// handleProgressItem melayani GET /progress/{playable_id}.
func (s *server) handleProgressItem(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	playableID, valid := pathID(r, "playable_id")
	if !valid {
		response.Error(w, response.CodeProgressNotFound, nil)
		return
	}

	row := s.read.QueryRowContext(r.Context(),
		progressSelect+` WHERE pp.device_id = ? AND pp.playable_id = ?`,
		device.ID, playableID)

	item, err := scanProgressItem(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodeProgressNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}

	response.Success(w, item)
}

// progressSelect menggabungkan progress dengan judul dan episode.
const progressSelect = `SELECT pp.playable_id, pp.position_seconds, pp.duration_seconds,
		pp.completed, pp.updated_at,
		t.id, t.title, t.poster_path,
		e.season_number, e.episode_number
	FROM playback_progress pp
	JOIN playables pl ON pl.id = pp.playable_id
	JOIN titles t ON t.id = pl.title_id
	LEFT JOIN episodes e ON e.id = pl.episode_id`

// scanProgressItem membaca satu baris progressSelect.
func scanProgressItem(sc rowScanner) (progressItem, error) {
	var (
		item      progressItem
		duration  sql.NullInt64
		updated   sql.NullString
		poster    sql.NullString
		season    sql.NullInt64
		episode   sql.NullInt64
		completed int
	)
	err := sc.Scan(&item.PlayableID, &item.PositionSeconds, &duration, &completed, &updated,
		&item.TitleID, &item.Title, &poster, &season, &episode)
	if err != nil {
		return progressItem{}, err
	}
	item.DurationSeconds = nullInt64(duration)
	item.Completed = completed == 1
	item.UpdatedAt = isoTime(updated.String)
	item.PosterURL = artworkURL(poster.String)
	if season.Valid && episode.Valid {
		item.Episode = progressEpisode{SeasonNumber: season.Int64, EpisodeNumber: episode.Int64}
	}
	item.ProgressPercent = progressPercent(item.PositionSeconds, duration)
	return item, nil
}

// progressPercent menghitung persentase tonton. Durasi tidak diketahui
// menghasilkan 0, bukan pembagian dengan nol.
func progressPercent(position int64, duration sql.NullInt64) int64 {
	if !duration.Valid || duration.Int64 <= 0 || position <= 0 {
		return 0
	}
	percent := position * 100 / duration.Int64
	if percent > 100 {
		return 100
	}
	return percent
}

// durationArg mengubah durasi opsional menjadi argumen SQL.
func durationArg(duration *int64) any {
	if duration == nil {
		return nil
	}
	return *duration
}

// boolToInt mengubah bool menjadi 0/1 untuk kolom CHECK (0,1).
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
