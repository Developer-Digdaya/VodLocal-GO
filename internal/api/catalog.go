package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"vodlocal/internal/response"
)

// titlesSelect mengambil satu baris judul lengkap dengan jumlah episode dan
// label generation yang menyimpan artwork. Label dipilih dari rendition ready
// pada generation mana pun yang sudah selesai (bukan building/failed), sehingga
// batch lama tetap bisa dipakai.
const titlesSelect = `SELECT t.id, COALESCE(pr.code, ''), t.kind, t.title, t.slug,
	t.synopsis, t.release_year, t.content_rating, t.language,
	t.poster_path, t.backdrop_path, t.duration_seconds,
	(SELECT COUNT(*) FROM episodes e WHERE e.title_id = t.id),
	COALESCE((
		SELECT g.label
		FROM playables pl
		JOIN renditions rd ON rd.playable_id = pl.id
		JOIN generations g ON g.id = rd.generation_id
		WHERE pl.title_id = t.id AND rd.status = 'ready'
		  AND g.status NOT IN ('building', 'failed')
		ORDER BY g.created_at DESC, g.id DESC
		LIMIT 1
	), '')`

// titlesFrom adalah sumber data judul beserta kode providernya.
const titlesFrom = ` FROM titles t JOIN providers pr ON pr.id = t.provider_id`

// titleAvailable mensyaratkan adanya rendition ready pada generation mana pun
// yang sudah selesai. Generation tidak lagi menentukan visibilitas katalog.
const titleAvailable = `EXISTS (
	SELECT 1 FROM playables pl
	JOIN renditions rd ON rd.playable_id = pl.id
	JOIN generations g ON g.id = rd.generation_id
	WHERE pl.title_id = t.id AND rd.status = 'ready'
	  AND g.status NOT IN ('building', 'failed')
)`

// titleSortOrders adalah whitelist sort → klausa ORDER BY. Input user tidak
// pernah masuk ke SQL; hanya kunci map ini yang dipakai.
var titleSortOrders = map[string]string{
	"":              "t.updated_at DESC, t.id DESC",
	"-updated_at":   "t.updated_at DESC, t.id DESC",
	"updated_at":    "t.updated_at ASC, t.id ASC",
	"title":         "t.title COLLATE NOCASE ASC, t.id ASC",
	"-title":        "t.title COLLATE NOCASE DESC, t.id DESC",
	"-release_year": "COALESCE(t.release_year, 0) DESC, t.title COLLATE NOCASE ASC, t.id ASC",
	"release_year":  "COALESCE(t.release_year, 0) ASC, t.title COLLATE NOCASE ASC, t.id ASC",
}

// titleItem adalah bentuk judul pada endpoint katalog (§7.2).
type titleItem struct {
	ID              int64    `json:"id"`
	Provider        string   `json:"provider"`
	Kind            string   `json:"kind"`
	Title           string   `json:"title"`
	Slug            string   `json:"slug"`
	Synopsis        any      `json:"synopsis"`
	ReleaseYear     any      `json:"release_year"`
	ContentRating   any      `json:"content_rating"`
	Language        any      `json:"language"`
	PosterURL       any      `json:"poster_url"`
	BackdropURL     any      `json:"backdrop_url"`
	DurationSeconds any      `json:"duration_seconds"`
	EpisodeCount    int64    `json:"episode_count"`
	Genres          []string `json:"genres"`
	Playable        bool     `json:"playable"`
}

// seasonItem adalah satu baris data.seasons pada detail judul (§7.3).
type seasonItem struct {
	SeasonNumber int64 `json:"season_number"`
	EpisodeCount int64 `json:"episode_count"`
}

// titleDetail adalah data GET /titles/{id}.
type titleDetail struct {
	titleItem
	Seasons         []seasonItem `json:"seasons"`
	FirstPlayableID any          `json:"first_playable_id"`
}

// episodeItem adalah satu baris daftar episode (§7.4).
type episodeItem struct {
	ID              int64 `json:"id"`
	SeasonNumber    int64 `json:"season_number"`
	EpisodeNumber   int64 `json:"episode_number"`
	Name            any   `json:"name"`
	Synopsis        any   `json:"synopsis"`
	DurationSeconds any   `json:"duration_seconds"`
	AirDate         any   `json:"air_date"`
	StillURL        any   `json:"still_url"`
	PlayableID      any   `json:"playable_id"`
	Ready           bool  `json:"ready"`
}

// genreItem adalah satu baris daftar genre (§7.1).
type genreItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// rowScanner adalah irisan yang dipenuhi *sql.Row maupun *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// generationMediaURL menyusun URL isi sebuah generation untuk poster, backdrop,
// dan still (§7.2). Poster/backdrop kini tersimpan di dalam folder generation
// (satu folder dengan m3u8/ts), jadi kolom poster_path/backdrop_path/still_path
// berisi path relatif terhadap root generation dan harus tampil sebagai
// /media/<label>/<path>, bukan /artwork/<path>.
//
// Path dibersihkan dengan path.Clean (bukan filepath.Clean) supaya hasilnya
// selalu berbentuk URL POSIX dan tidak pernah keluar dari root generation:
// seluruh ".." sudah dinormalisasi ke root sebelum digabung. Generation atau
// path kosong menghasilkan nil, sehingga JSON-nya null dan bukan string kosong.
//
// Nama fungsi berbeda dari mediaURL karena helpers.go sudah punya mediaURL
// untuk subtitle dan playlist HLS; file itu di luar cakupan perubahan ini.
func generationMediaURL(generation, rel string) any {
	rel = strings.TrimSpace(rel)
	if generation == "" || rel == "" {
		return nil
	}
	return "/media/" + generation + "/" + strings.TrimPrefix(path.Clean("/"+rel), "/")
}

// scanTitleItem membaca satu baris titlesSelect ke titleItem. Label generation
// untuk poster/backdrop sudah ikut di SELECT, jadi tidak lagi bergantung pada
// generation aktif.
func scanTitleItem(sc rowScanner) (titleItem, error) {
	var (
		item     titleItem
		synopsis sql.NullString
		rating   sql.NullString
		language sql.NullString
		poster   sql.NullString
		backdrop sql.NullString
		year     sql.NullInt64
		duration sql.NullInt64
		artLabel string
	)
	err := sc.Scan(
		&item.ID, &item.Provider, &item.Kind, &item.Title, &item.Slug,
		&synopsis, &year, &rating, &language, &poster, &backdrop, &duration,
		&item.EpisodeCount, &artLabel,
	)
	if err != nil {
		return titleItem{}, err
	}
	item.Synopsis = nullString(synopsis)
	item.ReleaseYear = nullInt64(year)
	item.ContentRating = nullString(rating)
	item.Language = nullString(language)
	item.PosterURL = generationMediaURL(artLabel, poster.String)
	item.BackdropURL = generationMediaURL(artLabel, backdrop.String)
	item.DurationSeconds = nullInt64(duration)
	item.Genres = []string{}
	item.Playable = true
	return item, nil
}

// handleGenres melayani GET /genres.
func (s *server) handleGenres(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fields := map[string]string{}
	page := parsePagination(q, fields)
	if s.validationFailed(w, fields) {
		return
	}

	ctx := r.Context()
	var total int64
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM genres`).Scan(&total); err != nil {
		s.internalError(w, r, err)
		return
	}

	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, slug FROM genres ORDER BY name COLLATE NOCASE ASC LIMIT ? OFFSET ?`,
		page.PerPage, page.offset(),
	)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer rows.Close()

	items := make([]genreItem, 0, page.PerPage)
	for rows.Next() {
		var item genreItem
		if err := rows.Scan(&item.ID, &item.Name, &item.Slug); err != nil {
			s.internalError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, listData[genreItem]{Items: items, Meta: page.meta(total)})
}

// listData adalah bentuk data endpoint list (§4, R12).
type listData[T any] struct {
	Items []T      `json:"items"`
	Meta  pageMeta `json:"meta"`
}

// handleTitles melayani GET /titles.
func (s *server) handleTitles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fields := map[string]string{}
	page := parsePagination(q, fields)

	search := normalizeSearchText(q.Get("q"))
	switch {
	case q.Get("q") != "" && search == "":
		response.ErrorField(w, response.CodeSearchQueryInvalid, "q", "minimal 2 karakter")
		return
	case search != "" && len([]rune(search)) < minSearchLength:
		response.ErrorField(w, response.CodeSearchQueryInvalid, "q", "minimal 2 karakter")
		return
	}

	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "" && kind != "movie" && kind != "series" {
		fields["kind"] = "harus movie atau series"
	}

	genre := strings.TrimSpace(q.Get("genre"))

	order, okOrder := titleSortOrders[strings.TrimSpace(q.Get("sort"))]
	if !okOrder {
		fields["sort"] = "nilai tidak didukung"
	}

	if s.validationFailed(w, fields) {
		return
	}

	conditions := []string{"t.is_published = 1", titleAvailable}
	args := make([]any, 0, 4)

	if search != "" {
		conditions = append(conditions, `t.title LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(search))
	}
	if kind != "" {
		conditions = append(conditions, "t.kind = ?")
		args = append(args, kind)
	}
	if genre != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM title_genres tg
			JOIN genres g ON g.id = tg.genre_id
			WHERE tg.title_id = t.id AND g.slug = ?
		)`)
		args = append(args, genre)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")
	ctx := r.Context()

	var total int64
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*)`+titlesFrom+where, args...).Scan(&total); err != nil {
		s.internalError(w, r, err)
		return
	}

	listArgs := append(append([]any{}, args...), page.PerPage, page.offset())
	rows, err := s.read.QueryContext(ctx,
		titlesSelect+titlesFrom+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer rows.Close()

	items := make([]titleItem, 0, page.PerPage)
	for rows.Next() {
		item, err := scanTitleItem(rows)
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

	genres, err := s.genresByTitle(ctx, items)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range items {
		if names, found := genres[items[i].ID]; found {
			items[i].Genres = names
		}
	}

	response.Success(w, listData[titleItem]{Items: items, Meta: page.meta(total)})
}

// genresByTitle mengambil nama genre untuk sekumpulan judul sekaligus, terurut
// menurut nama agar output deterministik.
func (s *server) genresByTitle(ctx context.Context, items []titleItem) (map[int64][]string, error) {
	out := make(map[int64][]string, len(items))
	if len(items) == 0 {
		return out, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	args := make([]any, len(items))
	for i := range items {
		args[i] = items[i].ID
	}

	rows, err := s.read.QueryContext(ctx,
		`SELECT tg.title_id, g.name
		 FROM title_genres tg JOIN genres g ON g.id = tg.genre_id
		 WHERE tg.title_id IN (`+placeholders+`)
		 ORDER BY tg.title_id, g.name COLLATE NOCASE ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			titleID int64
			name    string
		)
		if err := rows.Scan(&titleID, &name); err != nil {
			return nil, err
		}
		out[titleID] = append(out[titleID], name)
	}
	return out, rows.Err()
}

// loadTitle mengambil satu judul beserta status ketersediaannya pada generation
// aktif. sql.ErrNoRows berarti judul tidak ada atau tidak dipublikasikan.
func (s *server) loadTitle(ctx context.Context, id int64) (titleItem, bool, error) {
	row := s.read.QueryRowContext(ctx,
		titlesSelect+titlesFrom+` WHERE t.id = ? AND t.is_published = 1`, id)

	item, err := scanTitleItem(row)
	if err != nil {
		return titleItem{}, false, err
	}

	var ready int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM playables pl JOIN renditions rd ON rd.playable_id = pl.id
		 JOIN generations g ON g.id = rd.generation_id
		 WHERE pl.title_id = ? AND rd.status = 'ready'
		   AND g.status NOT IN ('building', 'failed')`,
		id,
	).Scan(&ready); err != nil {
		return titleItem{}, false, err
	}

	return item, ready > 0, nil
}

// handleTitleDetail melayani GET /titles/{id}.
func (s *server) handleTitleDetail(w http.ResponseWriter, r *http.Request) {
	id, valid := pathID(r, "id")
	if !valid {
		response.Error(w, response.CodeTitleNotFound, nil)
		return
	}

	ctx := r.Context()
	item, available, err := s.loadTitle(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodeTitleNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case !available:
		response.Error(w, response.CodeContentNotPublished, nil)
		return
	}

	seasons, err := s.seasonsFor(ctx, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	firstPlayable, err := s.firstPlayableID(ctx, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	genres, err := s.genresByTitle(ctx, []titleItem{item})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if names, found := genres[item.ID]; found {
		item.Genres = names
	}

	response.Success(w, titleDetail{
		titleItem:       item,
		Seasons:         seasons,
		FirstPlayableID: firstPlayable,
	})
}

// seasonsFor menghitung jumlah episode per season.
func (s *server) seasonsFor(ctx context.Context, titleID int64) ([]seasonItem, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT season_number, COUNT(*) FROM episodes
		 WHERE title_id = ? GROUP BY season_number ORDER BY season_number ASC`,
		titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seasons := make([]seasonItem, 0, 4)
	for rows.Next() {
		var item seasonItem
		if err := rows.Scan(&item.SeasonNumber, &item.EpisodeCount); err != nil {
			return nil, err
		}
		seasons = append(seasons, item)
	}
	return seasons, rows.Err()
}

// firstPlayableID mencari playable siap pertama menurut urutan tayang.
func (s *server) firstPlayableID(ctx context.Context, titleID int64) (any, error) {
	var id int64
	err := s.read.QueryRowContext(ctx,
		`SELECT pl.id
		 FROM playables pl
		 JOIN renditions rd ON rd.playable_id = pl.id
		 JOIN generations g ON g.id = rd.generation_id
		 LEFT JOIN episodes e ON e.id = pl.episode_id
		 WHERE pl.title_id = ? AND rd.status = 'ready'
		   AND g.status NOT IN ('building', 'failed')
		 GROUP BY pl.id
		 ORDER BY COALESCE(e.season_number, 0) ASC, COALESCE(e.episode_number, 0) ASC, pl.id ASC
		 LIMIT 1`,
		titleID,
	).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return id, nil
}

// handleTitleEpisodes melayani GET /titles/{id}/episodes.
func (s *server) handleTitleEpisodes(w http.ResponseWriter, r *http.Request) {
	id, valid := pathID(r, "id")
	if !valid {
		response.Error(w, response.CodeTitleNotFound, nil)
		return
	}

	q := r.URL.Query()
	fields := map[string]string{}
	page := parsePagination(q, fields)

	var (
		season    int64
		hasSeason bool
	)
	if raw := strings.TrimSpace(q.Get("season")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fields["season"] = "harus berupa angka"
		} else {
			season, hasSeason = n, true
		}
	}
	if s.validationFailed(w, fields) {
		return
	}

	ctx := r.Context()
	_, available, err := s.loadTitle(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodeTitleNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case !available:
		response.Error(w, response.CodeContentNotPublished, nil)
		return
	}

	conditions := []string{"e.title_id = ?"}
	args := []any{id}
	if hasSeason {
		conditions = append(conditions, "e.season_number = ?")
		args = append(args, season)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")

	var total int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM episodes e`+where, args...).Scan(&total); err != nil {
		s.internalError(w, r, err)
		return
	}

	listArgs := append(append([]any{}, args...), page.PerPage, page.offset())
	rows, err := s.read.QueryContext(ctx,
		`SELECT e.id, e.season_number, e.episode_number, e.name, e.synopsis,
		        e.duration_seconds, e.air_date, e.still_path,
		        (SELECT pl.id FROM playables pl WHERE pl.episode_id = e.id),
		        EXISTS (
		            SELECT 1 FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.episode_id = e.id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		        ),
		        COALESCE((
		            SELECT g.label
		            FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.episode_id = e.id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		            ORDER BY g.created_at DESC, g.id DESC
		            LIMIT 1
		        ), (
		            SELECT g.label
		            FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.title_id = e.title_id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		            ORDER BY g.created_at DESC, g.id DESC
		            LIMIT 1
		        ), '')
		 FROM episodes e`+where+`
		 ORDER BY e.season_number ASC, e.episode_number ASC
		 LIMIT ? OFFSET ?`,
		listArgs...)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer rows.Close()

	items := make([]episodeItem, 0, page.PerPage)
	for rows.Next() {
		var (
			item     episodeItem
			name     sql.NullString
			synopsis sql.NullString
			duration sql.NullInt64
			airDate  sql.NullString
			still    sql.NullString
			playable sql.NullInt64
			artLabel string
		)
		if err := rows.Scan(&item.ID, &item.SeasonNumber, &item.EpisodeNumber, &name, &synopsis,
			&duration, &airDate, &still, &playable, &item.Ready, &artLabel); err != nil {
			s.internalError(w, r, err)
			return
		}
		item.Name = nullString(name)
		item.Synopsis = nullString(synopsis)
		item.DurationSeconds = nullInt64(duration)
		item.AirDate = nullString(airDate)
		item.StillURL = generationMediaURL(artLabel, still.String)
		item.PlayableID = nullInt64(playable)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, listData[episodeItem]{Items: items, Meta: page.meta(total)})
}

// Batas hasil pencarian gabungan (§7.5).
const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
)

// searchTitleItem adalah satu judul pada hasil GET /search (§7.5).
type searchTitleItem struct {
	ID           int64    `json:"id"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	ReleaseYear  any      `json:"release_year"`
	PosterURL    any      `json:"poster_url"`
	Genres       []string `json:"genres"`
	EpisodeCount int64    `json:"episode_count"`
	Match        string   `json:"match"`
}

// searchEpisodeItem adalah satu episode pada hasil GET /search (§7.5).
type searchEpisodeItem struct {
	ID            int64  `json:"id"`
	TitleID       int64  `json:"title_id"`
	Title         string `json:"title"`
	SeasonNumber  int64  `json:"season_number"`
	EpisodeNumber int64  `json:"episode_number"`
	Name          any    `json:"name"`
	PlayableID    any    `json:"playable_id"`
	Ready         bool   `json:"ready"`
	StillURL      any    `json:"still_url"`
	Match         string `json:"match"`
}

// searchMeta adalah data.meta GET /search.
type searchMeta struct {
	Limit         int64 `json:"limit"`
	TitlesTotal   int64 `json:"titles_total"`
	EpisodesTotal int64 `json:"episodes_total"`
}

// searchData adalah data GET /search.
type searchData struct {
	Titles   []searchTitleItem   `json:"titles"`
	Episodes []searchEpisodeItem `json:"episodes"`
	Meta     searchMeta          `json:"meta"`
}

// handleSearch melayani GET /search.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := normalizeSearchText(q.Get("q"))
	if len([]rune(search)) < minSearchLength {
		response.ErrorField(w, response.CodeSearchQueryInvalid, "q", "minimal 2 karakter")
		return
	}

	fields := map[string]string{}

	searchType := strings.TrimSpace(q.Get("type"))
	if searchType == "" {
		searchType = "all"
	}
	switch searchType {
	case "all", "title", "episode":
	default:
		fields["type"] = "harus all, title, atau episode"
	}

	limit := int64(defaultSearchLimit)
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			fields["limit"] = "harus berupa angka"
		case n < 1:
			fields["limit"] = "minimal 1"
		case n > maxSearchLimit:
			fields["limit"] = "maksimal 50"
		default:
			limit = n
		}
	}

	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "" && kind != "movie" && kind != "series" {
		fields["kind"] = "harus movie atau series"
	}
	genre := strings.TrimSpace(q.Get("genre"))

	if s.validationFailed(w, fields) {
		return
	}

	data := searchData{
		Titles:   []searchTitleItem{},
		Episodes: []searchEpisodeItem{},
		Meta:     searchMeta{Limit: limit},
	}

	if searchType == "all" || searchType == "title" {
		titles, total, err := s.searchTitles(r.Context(), search, kind, genre, limit)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		data.Titles = titles
		data.Meta.TitlesTotal = total
	}

	if searchType == "all" || searchType == "episode" {
		episodes, total, err := s.searchEpisodes(r.Context(), search, kind, genre, limit)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		data.Episodes = episodes
		data.Meta.EpisodesTotal = total
	}

	response.Success(w, data)
}

// searchTitles mengembalikan judul yang cocok pada kolom titles.title, terurut
// menurut aturan §4.1 (prefix lebih dulu, lalu terbaru).
func (s *server) searchTitles(ctx context.Context, search, kind, genre string, limit int64) ([]searchTitleItem, int64, error) {
	conditions := []string{
		"t.is_published = 1",
		titleAvailable,
		`t.title LIKE ? ESCAPE '\'`,
	}
	args := []any{likeContains(search)}

	if kind != "" {
		conditions = append(conditions, "t.kind = ?")
		args = append(args, kind)
	}
	if genre != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM title_genres tg
			JOIN genres g ON g.id = tg.genre_id
			WHERE tg.title_id = t.id AND g.slug = ?
		)`)
		args = append(args, genre)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	var total int64
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*)`+titlesFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listArgs := append(append([]any{}, args...), likePrefix(search), limit)
	rows, err := s.read.QueryContext(ctx,
		titlesSelect+titlesFrom+where+`
		 ORDER BY CASE WHEN lower(t.title) LIKE ? ESCAPE '\' THEN 0 ELSE 1 END,
		          t.updated_at DESC, t.id DESC
		 LIMIT ?`, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]searchTitleItem, 0, limit)
	titles := make([]titleItem, 0, limit)
	for rows.Next() {
		item, err := scanTitleItem(rows)
		if err != nil {
			return nil, 0, err
		}
		titles = append(titles, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	genres, err := s.genresByTitle(ctx, titles)
	if err != nil {
		return nil, 0, err
	}
	for _, item := range titles {
		entry := searchTitleItem{
			ID:           item.ID,
			Kind:         item.Kind,
			Title:        item.Title,
			ReleaseYear:  item.ReleaseYear,
			PosterURL:    item.PosterURL,
			Genres:       genres[item.ID],
			EpisodeCount: item.EpisodeCount,
			Match:        "title",
		}
		if entry.Genres == nil {
			entry.Genres = []string{}
		}
		items = append(items, entry)
	}

	return items, total, nil
}

// searchEpisodes mengembalikan episode yang namanya cocok. Episode yang hanya
// cocok karena judulnya cocok tidak diikutkan, sehingga satu episode tidak
// pernah muncul dua kali (§7.5).
func (s *server) searchEpisodes(ctx context.Context, search, kind, genre string, limit int64) ([]searchEpisodeItem, int64, error) {
	conditions := []string{
		"t.is_published = 1",
		titleAvailable,
		`e.name LIKE ? ESCAPE '\'`,
	}
	args := []any{likeContains(search)}

	if kind != "" {
		conditions = append(conditions, "t.kind = ?")
		args = append(args, kind)
	}
	if genre != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM title_genres tg
			JOIN genres g ON g.id = tg.genre_id
			WHERE tg.title_id = t.id AND g.slug = ?
		)`)
		args = append(args, genre)
	}

	const episodesFrom = ` FROM episodes e JOIN titles t ON t.id = e.title_id`
	where := " WHERE " + strings.Join(conditions, " AND ")

	var total int64
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*)`+episodesFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listArgs := append(append([]any{}, args...), likePrefix(search), limit)
	rows, err := s.read.QueryContext(ctx,
		`SELECT e.id, e.title_id, t.title, e.season_number, e.episode_number, e.name,
		        e.still_path,
		        (SELECT pl.id FROM playables pl WHERE pl.episode_id = e.id),
		        EXISTS (
		            SELECT 1 FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.episode_id = e.id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		        ),
		        COALESCE((
		            SELECT g.label
		            FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.episode_id = e.id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		            ORDER BY g.created_at DESC, g.id DESC
		            LIMIT 1
		        ), (
		            SELECT g.label
		            FROM playables pl
		            JOIN renditions rd ON rd.playable_id = pl.id
		            JOIN generations g ON g.id = rd.generation_id
		            WHERE pl.title_id = e.title_id AND rd.status = 'ready'
		              AND g.status NOT IN ('building', 'failed')
		            ORDER BY g.created_at DESC, g.id DESC
		            LIMIT 1
		        ), '')`+episodesFrom+where+`
		 ORDER BY CASE WHEN lower(e.name) LIKE ? ESCAPE '\' THEN 0 ELSE 1 END,
		          e.updated_at DESC, e.id DESC
		 LIMIT ?`,
		listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]searchEpisodeItem, 0, limit)
	for rows.Next() {
		var (
			item     searchEpisodeItem
			name     sql.NullString
			still    sql.NullString
			playable sql.NullInt64
			artLabel string
		)
		if err := rows.Scan(&item.ID, &item.TitleID, &item.Title, &item.SeasonNumber,
			&item.EpisodeNumber, &name, &still, &playable, &item.Ready, &artLabel); err != nil {
			return nil, 0, err
		}
		item.Name = nullString(name)
		item.StillURL = generationMediaURL(artLabel, still.String)
		item.PlayableID = nullInt64(playable)
		item.Match = "episode"
		items = append(items, item)
	}
	return items, total, rows.Err()
}
