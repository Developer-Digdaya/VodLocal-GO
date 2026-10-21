package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"vodlocal/internal/hls"
	"vodlocal/internal/response"
)

// episodeRef adalah ringkasan episode di dalam detail playable (§8.1).
type episodeRef struct {
	SeasonNumber  int64 `json:"season_number"`
	EpisodeNumber int64 `json:"episode_number"`
	Name          any   `json:"name"`
}

// variantItem adalah satu rendisi siap pada detail playable (§8.1).
type variantItem struct {
	Quality     string `json:"quality"`
	Width       any    `json:"width"`
	Height      any    `json:"height"`
	BitrateKbps any    `json:"bitrate_kbps"`
	Ready       bool   `json:"ready"`
}

// subtitleItem adalah satu subtitle siap pada detail playable (§8.1).
type subtitleItem struct {
	Lang   string `json:"lang"`
	Label  any    `json:"label"`
	Format string `json:"format"`
	URL    string `json:"url"`
}

// playableData adalah data GET /playables/{id}.
type playableData struct {
	ID                int64          `json:"id"`
	TitleID           int64          `json:"title_id"`
	Title             string         `json:"title"`
	Episode           any            `json:"episode"`
	DurationSeconds   any            `json:"duration_seconds"`
	Generation        string         `json:"generation"`
	MasterPlaylistURL string         `json:"master_playlist_url"`
	Variants          []variantItem  `json:"variants"`
	Subtitles         []subtitleItem `json:"subtitles"`
}

// playableRow adalah hasil pencarian satu playable.
type playableRow struct {
	ID              int64
	TitleID         int64
	Title           string
	Published       bool
	DurationSeconds sql.NullInt64
	SeasonNumber    sql.NullInt64
	EpisodeNumber   sql.NullInt64
	EpisodeName     sql.NullString
}

// renditionRow adalah satu rendisi ready milik playable pada generation aktif.
type renditionRow struct {
	Quality      string
	Width        sql.NullInt64
	Height       sql.NullInt64
	BitrateKbps  sql.NullInt64
	PlaylistPath string
}

// loadPlayable mencari satu playable beserta judul dan episodenya.
func (s *server) loadPlayable(ctx context.Context, id int64) (playableRow, error) {
	var (
		row       playableRow
		published int
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT pl.id, pl.title_id, t.title, t.is_published, pl.duration_seconds,
		        e.season_number, e.episode_number, e.name
		 FROM playables pl
		 JOIN titles t ON t.id = pl.title_id
		 LEFT JOIN episodes e ON e.id = pl.episode_id
		 WHERE pl.id = ?`, id,
	).Scan(&row.ID, &row.TitleID, &row.Title, &published, &row.DurationSeconds,
		&row.SeasonNumber, &row.EpisodeNumber, &row.EpisodeName)
	if err != nil {
		return playableRow{}, err
	}
	row.Published = published == 1
	return row, nil
}

// readyRenditions mengambil rendisi berstatus ready pada generation aktif,
// diurutkan dari resolusi tertinggi (§8.1).
func (s *server) readyRenditions(ctx context.Context, gen activeGeneration, playableID int64) ([]renditionRow, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT quality, width, height, bitrate_kbps, COALESCE(playlist_path, '')
		 FROM renditions
		 WHERE playable_id = ? AND generation_id = ? AND status = 'ready'
		 ORDER BY height DESC, bitrate_kbps DESC, quality ASC`,
		playableID, gen.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	renditions := make([]renditionRow, 0, 4)
	for rows.Next() {
		var row renditionRow
		if err := rows.Scan(&row.Quality, &row.Width, &row.Height, &row.BitrateKbps, &row.PlaylistPath); err != nil {
			return nil, err
		}
		renditions = append(renditions, row)
	}
	return renditions, rows.Err()
}

// subtitlesFor mengambil subtitle siap pada generation aktif.
func (s *server) subtitlesFor(ctx context.Context, gen activeGeneration, playableID int64) ([]subtitleItem, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT lang, label, format, file_path
		 FROM subtitles
		 WHERE playable_id = ? AND generation_id = ? AND status = 'ready'
		 ORDER BY lang ASC`,
		playableID, gen.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	subtitles := make([]subtitleItem, 0, 2)
	for rows.Next() {
		var (
			item     subtitleItem
			label    sql.NullString
			filePath string
		)
		if err := rows.Scan(&item.Lang, &label, &item.Format, &filePath); err != nil {
			return nil, err
		}
		item.Label = nullString(label)
		item.URL = mediaURL(gen.Label, filePath)
		subtitles = append(subtitles, item)
	}
	return subtitles, rows.Err()
}

// handlePlayable melayani GET /playables/{id}.
func (s *server) handlePlayable(w http.ResponseWriter, r *http.Request) {
	gen, ok := s.requireGeneration(w, r)
	if !ok {
		return
	}

	id, valid := pathID(r, "id")
	if !valid {
		response.Error(w, response.CodePlayableNotFound, nil)
		return
	}

	ctx := r.Context()
	playable, err := s.loadPlayable(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodePlayableNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case !playable.Published:
		response.Error(w, response.CodeContentNotPublished, nil)
		return
	}

	renditions, err := s.readyRenditions(ctx, gen, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(renditions) == 0 {
		response.Error(w, response.CodePlayableNotReady, nil)
		return
	}

	subtitles, err := s.subtitlesFor(ctx, gen, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	variants := make([]variantItem, 0, len(renditions))
	for _, rendition := range renditions {
		variants = append(variants, variantItem{
			Quality:     rendition.Quality,
			Width:       nullInt64(rendition.Width),
			Height:      nullInt64(rendition.Height),
			BitrateKbps: nullInt64(rendition.BitrateKbps),
			Ready:       true,
		})
	}

	var episode any
	if playable.SeasonNumber.Valid && playable.EpisodeNumber.Valid {
		episode = episodeRef{
			SeasonNumber:  playable.SeasonNumber.Int64,
			EpisodeNumber: playable.EpisodeNumber.Int64,
			Name:          nullString(playable.EpisodeName),
		}
	}

	response.Success(w, playableData{
		ID:                playable.ID,
		TitleID:           playable.TitleID,
		Title:             playable.Title,
		Episode:           episode,
		DurationSeconds:   nullInt64(playable.DurationSeconds),
		Generation:        gen.Label,
		MasterPlaylistURL: "/api/v1/playables/" + itoa(id) + "/master.m3u8",
		Variants:          variants,
		Subtitles:         subtitles,
	})
}

// handleMasterPlaylist melayani GET /playables/{id}/master.m3u8.
//
// Response-nya bukan envelope JSON: isinya harus playlist HLS valid
// (docs/api-spec.md §11). Kegagalan tetap memakai envelope supaya klien bisa
// membaca kode 3002/3003/4009.
func (s *server) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	gen, ok := s.requireGeneration(w, r)
	if !ok {
		return
	}

	id, valid := pathID(r, "id")
	if !valid {
		response.Error(w, response.CodePlayableNotFound, nil)
		return
	}

	ctx := r.Context()
	playable, err := s.loadPlayable(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodePlayableNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case !playable.Published:
		response.Error(w, response.CodeContentNotPublished, nil)
		return
	}

	renditions, err := s.readyRenditions(ctx, gen, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	variants := make([]hls.Variant, 0, len(renditions))
	for _, rendition := range renditions {
		if strings.TrimSpace(rendition.PlaylistPath) == "" {
			// Tanpa playlist_path tidak ada URI yang bisa ditawarkan.
			continue
		}
		bitrate := 0
		if rendition.BitrateKbps.Valid && rendition.BitrateKbps.Int64 > 0 {
			bitrate = int(rendition.BitrateKbps.Int64) * 1000
		}
		variant := hls.Variant{
			URI:              mediaURL(gen.Label, rendition.PlaylistPath),
			Bandwidth:        bitrate,
			AverageBandwidth: bitrate,
		}
		if rendition.Width.Valid {
			variant.Width = int(rendition.Width.Int64)
		}
		if rendition.Height.Valid {
			variant.Height = int(rendition.Height.Int64)
		}
		variants = append(variants, variant)
	}

	if len(variants) == 0 {
		response.Error(w, response.CodePlayableNotReady, nil)
		return
	}

	playlist := hls.BuildMaster(variants)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(playlist)
}

// itoa memformat ID playable menjadi string desimal untuk URL.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
