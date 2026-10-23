package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vodlocal/internal/response"
)

// likeEscapeClause dipakai seluruh pencarian agar input pengguna tidak pernah
// menjadi wildcard LIKE (§4.1).
const likeEscapeClause = ` ESCAPE '\'`

// Status sync yang dianggap terminal.
const (
	syncStatusCompleted = "completed"
	syncStatusFailed    = "failed"
	syncStatusCancelled = "cancelled"
)

// syncStatuses adalah nilai status sync_runs yang diterima filter §10.3.
var syncStatuses = []string{
	"queued", "running", "verifying", "switching", "cleanup",
	syncStatusCompleted, syncStatusFailed, syncStatusCancelled,
}

// syncRunColumns adalah daftar kolom yang dibaca scanSyncRun.
const syncRunColumns = `id, status, COALESCE(phase, ''), trigger_source,
	COALESCE(triggered_by, ''), COALESCE(total_renditions, 0), COALESCE(renditions_ready, 0),
	COALESCE(renditions_failed, 0), COALESCE(bytes_downloaded, 0), COALESCE(started_at, ''),
	COALESCE(finished_at, ''), created_at, COALESCE(error_message, ''), COALESCE(is_active, 0),
	generation_id`

// syncRunView adalah satu baris sync_runs yang sudah dirapikan untuk template.
type syncRunView struct {
	ID               int64
	Status           string
	Phase            string
	Trigger          string
	TriggeredBy      string
	TotalRenditions  int64
	RenditionsReady  int64
	RenditionsFailed int64
	BytesDownloaded  int64
	StartedAt        string
	FinishedAt       string
	CreatedAt        string
	ErrorMessage     string
	Active           bool
	ProgressPercent  int
	// GenerationID adalah generation yang dibangun run ini; NULL untuk run
	// yang tidak (atau belum) terikat pada sebuah generation.
	GenerationID sql.NullInt64
}

// terminal melaporkan apakah run sudah selesai (tidak bisa dibatalkan lagi).
func (v syncRunView) terminal() bool {
	switch v.Status {
	case syncStatusCompleted, syncStatusFailed, syncStatusCancelled:
		return true
	default:
		return false
	}
}

// scanSyncRun memindai satu baris dengan kolom syncRunColumns.
func scanSyncRun(row interface{ Scan(dest ...any) error }) (*syncRunView, error) {
	var (
		run    syncRunView
		active int
	)
	err := row.Scan(&run.ID, &run.Status, &run.Phase, &run.Trigger, &run.TriggeredBy,
		&run.TotalRenditions, &run.RenditionsReady, &run.RenditionsFailed,
		&run.BytesDownloaded, &run.StartedAt, &run.FinishedAt, &run.CreatedAt,
		&run.ErrorMessage, &active, &run.GenerationID)
	if err != nil {
		return nil, err
	}
	run.Active = active == 1
	run.ProgressPercent = progressPercent(run.RenditionsReady, run.TotalRenditions)
	return &run, nil
}

// latestSyncRun membaca sync run terakhir, atau nil bila belum ada.
func (a *Admin) latestSyncRun(ctx context.Context) (*syncRunView, error) {
	run, err := scanSyncRun(a.read.QueryRowContext(ctx,
		`SELECT `+syncRunColumns+` FROM sync_runs ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// syncRunByID membaca satu sync run, atau nil bila tidak ada.
func (a *Admin) syncRunByID(ctx context.Context, id int64) (*syncRunView, error) {
	run, err := scanSyncRun(a.read.QueryRowContext(ctx,
		`SELECT `+syncRunColumns+` FROM sync_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// activeSyncRun membaca sync run yang masih berjalan, atau nil.
func (a *Admin) activeSyncRun(ctx context.Context) (*syncRunView, error) {
	run, err := scanSyncRun(a.read.QueryRowContext(ctx,
		`SELECT `+syncRunColumns+` FROM sync_runs WHERE is_active = 1 ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// syncRunFilter menyusun WHERE untuk pencarian/status riwayat run.
func syncRunFilter(query, status string) (string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)

	if query != "" {
		if id, err := strconv.ParseInt(query, 10, 64); err == nil && id > 0 {
			where = append(where, `(id = ? OR COALESCE(triggered_by, '') LIKE ?`+likeEscapeClause+`)`)
			args = append(args, id, likeContains(query))
		} else {
			where = append(where, `COALESCE(triggered_by, '') LIKE ?`+likeEscapeClause)
			args = append(args, likeContains(query))
		}
	}
	if status != "" && containsString(syncStatuses, status) {
		where = append(where, "status = ?")
		args = append(args, status)
	}

	return " WHERE " + strings.Join(where, " AND "), args
}

// listSyncRuns membaca satu halaman riwayat sinkronisasi dengan filter
// q/status (§10.3).
func (a *Admin) listSyncRuns(ctx context.Context, query, status string, limit, offset int) ([]syncRunView, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	where, filterArgs := syncRunFilter(query, status)
	args := append(append([]any{}, filterArgs...), limit, offset)
	rows, err := a.read.QueryContext(ctx,
		`SELECT `+syncRunColumns+` FROM sync_runs`+where+
			` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	runs := make([]syncRunView, 0, limit)
	for rows.Next() {
		run, err := scanSyncRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

// countSyncRuns menghitung riwayat yang cocok dengan filter.
func (a *Admin) countSyncRuns(ctx context.Context, query, status string) (int64, error) {
	where, args := syncRunFilter(query, status)
	var total int64
	err := a.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_runs`+where, args...).Scan(&total)
	return total, err
}

// containsString melaporkan apakah values memuat want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// syncStatusPayload adalah bentuk data GET /sync/{id}/status (§10.3).
type syncStatusPayload struct {
	ID               int64  `json:"id"`
	Status           string `json:"status"`
	Phase            string `json:"phase"`
	ProgressPercent  int    `json:"progress_percent"`
	TotalRenditions  int64  `json:"total_renditions"`
	RenditionsReady  int64  `json:"renditions_ready"`
	RenditionsFailed int64  `json:"renditions_failed"`
	BytesDownloaded  int64  `json:"bytes_downloaded"`
	ElapsedSeconds   int64  `json:"elapsed_seconds"`
	ETASeconds       int64  `json:"eta_seconds"`
}

// syncStatusView membungkus payload untuk template HTML partial.
type syncStatusView struct {
	Payload syncStatusPayload
	Active  bool
}

// syncStatusView menghitung payload status (progress, elapsed, ETA) dari run.
// Selama run aktif, progres diambil dari generation_items supaya unduhan per
// segmen terlihat; counter sync_runs baru terisi saat run ditutup.
func (a *Admin) syncStatusView(ctx context.Context, run *syncRunView) syncStatusView {
	payload := syncStatusPayload{
		ID:               run.ID,
		Status:           run.Status,
		Phase:            run.Phase,
		ProgressPercent:  run.ProgressPercent,
		TotalRenditions:  run.TotalRenditions,
		RenditionsReady:  run.RenditionsReady,
		RenditionsFailed: run.RenditionsFailed,
		BytesDownloaded:  run.BytesDownloaded,
	}

	if run.Active && run.GenerationID.Valid {
		if percent, bytes, err := a.generationProgress(ctx, run.GenerationID.Int64); err == nil {
			payload.ProgressPercent = percent
			payload.BytesDownloaded = bytes
		} else {
			a.log.Warn("admin: gagal membaca progres generation aktif",
				"run_id", run.ID, "generation_id", run.GenerationID.Int64, "err", err)
		}
	}

	if start, ok := parseTime(run.StartedAt); ok {
		end := a.nowUTC()
		if finished, ok := parseTime(run.FinishedAt); ok {
			end = finished
		}
		if elapsed := end.Sub(start); elapsed > 0 {
			payload.ElapsedSeconds = int64(elapsed / time.Second)
		}
	}

	// ETA kasar: sisa pekerjaan dikalikan kecepatan yang sudah berjalan.
	if payload.ProgressPercent > 0 && payload.ProgressPercent < 100 && payload.ElapsedSeconds > 0 {
		remaining := int64(100 - payload.ProgressPercent)
		payload.ETASeconds = payload.ElapsedSeconds * remaining / int64(payload.ProgressPercent)
	}

	return syncStatusView{Payload: payload, Active: run.Active}
}

// generationProgress menghitung rata-rata progres dan total byte seluruh
// item pada satu generation. Dipakai status run aktif yang belum punya counter
// akhir di sync_runs.
func (a *Admin) generationProgress(ctx context.Context, generationID int64) (int, int64, error) {
	var percent int
	var bytes int64
	err := a.read.QueryRowContext(ctx, `
SELECT CAST(COALESCE(AVG(progress_percent), 0) AS INTEGER),
       COALESCE(SUM(bytes_downloaded), 0)
  FROM generation_items
 WHERE generation_id = ?`, generationID).Scan(&percent, &bytes)
	return percent, bytes, err
}

// generationItemRow adalah satu baris generation_items: progres ingest satu
// series di dalam sebuah generation.
type generationItemRow struct {
	SeriesTitle      string `json:"series_title"`
	SeriesExternalID string `json:"series_external_id"`
	Category         string `json:"category"`
	Status           string `json:"status"`
	TotalEpisodes    int64  `json:"total_episodes"`
	EpisodesDone     int64  `json:"episodes_done"`
	EpisodesFailed   int64  `json:"episodes_failed"`
	RenditionsReady  int64  `json:"renditions_ready"`
	RenditionsFailed int64  `json:"renditions_failed"`
	BytesDownloaded  int64  `json:"bytes_downloaded"`
	ProgressPercent  int    `json:"progress_percent"`
	CurrentStep      string `json:"current_step"`
	CurrentEpisode   string `json:"current_episode"`
	ErrorMessage     string `json:"error_message"`
}

// generationItemsQuery membaca progres per series sebuah generation (§10.3).
const generationItemsQuery = `
SELECT series_title, series_external_id, category, status,
       total_episodes, episodes_done, episodes_failed,
       renditions_ready, renditions_failed, bytes_downloaded,
       progress_percent, current_step, current_episode, error_message
FROM generation_items
WHERE generation_id = ?
ORDER BY id`

// listGenerationItems membaca satu halaman item konten sebuah generation,
// urut id agar tampilan stabil antar-polling.
func (a *Admin) listGenerationItems(ctx context.Context, generationID int64, limit, offset int) ([]generationItemRow, error) {
	if limit <= 0 {
		limit = defaultPerPage
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := a.read.QueryContext(ctx, generationItemsQuery+`
LIMIT ? OFFSET ?`, generationID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]generationItemRow, 0, limit)
	for rows.Next() {
		var (
			item                generationItemRow
			step, episode, note sql.NullString
		)
		if err := rows.Scan(&item.SeriesTitle, &item.SeriesExternalID, &item.Category, &item.Status,
			&item.TotalEpisodes, &item.EpisodesDone, &item.EpisodesFailed,
			&item.RenditionsReady, &item.RenditionsFailed, &item.BytesDownloaded,
			&item.ProgressPercent, &step, &episode, &note); err != nil {
			return nil, err
		}
		item.CurrentStep = strings.TrimSpace(step.String)
		item.CurrentEpisode = strings.TrimSpace(episode.String)
		item.ErrorMessage = strings.TrimSpace(note.String)
		items = append(items, item)
	}
	return items, rows.Err()
}

// countGenerationItems menghitung seluruh item konten sebuah generation.
func (a *Admin) countGenerationItems(ctx context.Context, generationID int64) (int64, error) {
	var total int64
	err := a.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM generation_items WHERE generation_id = ?`, generationID).Scan(&total)
	return total, err
}

// generationItemSummary menghitung ringkasan seluruh item (bukan hanya halaman
// yang tampil) langsung di database.
func (a *Admin) generationItemSummary(ctx context.Context, generationID int64) (generationItemSummary, error) {
	var (
		summary     generationItemSummary
		seriesTotal int64
		seriesDone  int64
		progressSum int64
	)
	err := a.read.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(episodes_done), 0),
       COALESCE(SUM(total_episodes), 0),
       COALESCE(SUM(progress_percent), 0)
FROM generation_items
WHERE generation_id = ?`, generationID,
	).Scan(&seriesTotal, &seriesDone, &summary.EpisodesDone,
		&summary.EpisodesTotal, &progressSum)
	if err != nil {
		return generationItemSummary{}, err
	}

	summary.SeriesTotal = int(seriesTotal)
	summary.SeriesDone = int(seriesDone)
	if summary.EpisodesTotal > 0 {
		summary.Percent = progressPercent(summary.EpisodesDone, summary.EpisodesTotal)
	} else if seriesTotal > 0 {
		summary.Percent = progressPercent(progressSum, seriesTotal*100)
	}
	return summary, nil
}

// generationItemSummary adalah ringkasan progres seluruh series pada halaman
// detail: berapa series selesai dan berapa episode yang sudah diunduh.
type generationItemSummary struct {
	SeriesTotal   int   `json:"series_total"`
	SeriesDone    int   `json:"series_done"`
	EpisodesDone  int64 `json:"episodes_done"`
	EpisodesTotal int64 `json:"episodes_total"`
	Percent       int   `json:"progress_percent"`
}

// summarizeGenerationItems menghitung ringkasan dari daftar item.
//
// Persentase keseluruhan berbobot episode (total episode selesai dibagi total
// episode seluruh series) karena satu series bisa jauh lebih panjang dari yang
// lain. Bila generation belum melaporkan total episode sama sekali, dipakai
// rata-rata progress_percent per series agar angkanya tetap bermakna.
func summarizeGenerationItems(items []generationItemRow) generationItemSummary {
	summary := generationItemSummary{SeriesTotal: len(items)}
	if len(items) == 0 {
		return summary
	}

	progressSum := 0
	for _, item := range items {
		if item.Status == "done" {
			summary.SeriesDone++
		}
		summary.EpisodesDone += item.EpisodesDone
		summary.EpisodesTotal += item.TotalEpisodes
		progressSum += item.ProgressPercent
	}

	if summary.EpisodesTotal > 0 {
		summary.Percent = progressPercent(summary.EpisodesDone, summary.EpisodesTotal)
	} else {
		summary.Percent = progressPercent(int64(progressSum), int64(len(items))*100)
	}
	return summary
}

// generationItemsView adalah data fragmen "generation_items": tabel progres per
// series beserta ringkasannya untuk halaman /sync/{id}.
type generationItemsView struct {
	RunID  int64
	Status string
	Active bool
	pageInfo
	Items   []generationItemRow
	Summary generationItemSummary
}

// payload membentuk data envelope GET /sync/{id}/items.
func (v generationItemsView) payload() generationItemsPayload {
	items := v.Items
	if items == nil {
		items = []generationItemRow{}
	}
	return generationItemsPayload{
		Items:   items,
		Summary: v.Summary,
		Meta: paginationMeta{
			Page:     v.Page,
			PerPage:  v.PerPage,
			Total:    v.Total,
			LastPage: v.TotalPages,
		},
	}
}

// generationItemsPayload adalah bentuk data GET /sync/{id}/items.
type generationItemsPayload struct {
	Items   []generationItemRow   `json:"items"`
	Summary generationItemSummary `json:"summary"`
	Meta    paginationMeta        `json:"meta"`
}

// generationItemsView menyiapkan satu halaman progres per series milik run.
//
// Run tanpa generation_id (mis. run discovery yang gagal sebelum generation
// dibuat) dilaporkan sebagai daftar kosong, bukan kesalahan.
func (a *Admin) generationItemsView(ctx context.Context, run *syncRunView, page, perPage int) (generationItemsView, error) {
	view := generationItemsView{
		RunID:   run.ID,
		Status:  run.Status,
		Active:  run.Active,
		Items:   []generationItemRow{},
		Summary: generationItemSummary{},
	}
	if !run.GenerationID.Valid || run.GenerationID.Int64 < 1 {
		view.pageInfo = newPageInfo(page, perPage, 0)
		return view, nil
	}

	total, err := a.countGenerationItems(ctx, run.GenerationID.Int64)
	if err != nil {
		return view, err
	}
	view.pageInfo = newPageInfo(page, perPage, total)

	items, err := a.listGenerationItems(ctx, run.GenerationID.Int64, view.PerPage, view.offset())
	if err != nil {
		return view, err
	}
	view.Items = items

	summary, err := a.generationItemSummary(ctx, run.GenerationID.Int64)
	if err != nil {
		return view, err
	}
	view.Summary = summary
	return view, nil
}

// syncListData adalah data halaman /sync.
type syncListData struct {
	base
	pageInfo
	Q        string
	Status   string
	Runs     []syncRunView
	Active   *syncRunView
	Statuses []string
	// TransferEnabled memasang tombol export/offline hanya bila layanan
	// transfer tersedia pada server ini.
	TransferEnabled bool
}

// syncListData menyiapkan data halaman /sync.
func (a *Admin) syncListData(w http.ResponseWriter, r *http.Request, errMessage string) (syncListData, bool) {
	ctx := r.Context()
	values := r.URL.Query()
	data := syncListData{
		base:            a.base(w, r, "Sinkronisasi", "sync"),
		Q:               normalizeQuery(queryGet(values, "q")),
		Status:          strings.TrimSpace(queryGet(values, "status")),
		Statuses:        syncStatuses,
		TransferEnabled: a.transfer != nil,
	}
	if !containsString(syncStatuses, data.Status) {
		data.Status = ""
	}

	page, perPage := parsePageNamed(values, "page", "per_page", 20)
	total, err := a.countSyncRuns(ctx, data.Q, data.Status)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung riwayat sinkronisasi.")
		return data, false
	}
	data.pageInfo = newPageInfo(page, perPage, total)

	runs, err := a.listSyncRuns(ctx, data.Q, data.Status, data.PerPage, data.offset())
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca riwayat sinkronisasi.")
		return data, false
	}
	data.Runs = runs

	active, err := a.activeSyncRun(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca sinkronisasi yang berjalan.")
		return data, false
	}
	data.Active = active
	data.Error = errMessage
	return data, true
}

// handleSyncList melayani GET /sync.
func (a *Admin) handleSyncList(w http.ResponseWriter, r *http.Request) {
	data, ok := a.syncListData(w, r, "")
	if !ok {
		return
	}
	a.renderPage(w, r, "sync", data, http.StatusOK)
}

// handleSyncStart melayani POST /sync.
//
// Sinkronisasi dijalankan di goroutine terpisah: permintaan hanya memastikan
// tidak ada run aktif lalu mengalihkan (303) ke halaman /sync, sehingga
// koneksi HTTP tidak tertahan selama proses berjam-jam (§10.3).
func (a *Admin) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if a.syncer == nil {
		data, ok := a.syncListData(w, r, "Sinkronisasi tidak tersedia pada server ini.")
		if ok {
			a.renderPage(w, r, "sync", data, http.StatusConflict)
		}
		return
	}

	active, err := a.activeSyncRun(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal memeriksa sinkronisasi yang berjalan.")
		return
	}
	if active != nil {
		data, ok := a.syncListData(w, r,
			fmt.Sprintf("Masih ada sinkronisasi yang berjalan (run #%d, status %s).", active.ID, active.Status))
		if ok {
			a.renderPage(w, r, "sync", data, http.StatusConflict)
		}
		return
	}

	syncer := a.syncer
	username := a.sessionUsername(r)
	a.log.Info("admin: sinkronisasi manual diminta", "username", username)

	go func() {
		result, err := syncer.Run(context.Background(), "manual")
		if err != nil {
			// Token dan kredensial tidak pernah masuk log; hanya error domain.
			a.log.Error("admin: sinkronisasi manual gagal", "run_id", result.RunID, "err", err)
			return
		}
		a.log.Info("admin: sinkronisasi manual selesai",
			"run_id", result.RunID,
			"generation", result.GenerationLabel,
			"renditions_ready", result.RenditionsReady,
			"renditions_failed", result.RenditionsFailed,
		)
	}()

	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// syncDetailData adalah data halaman /sync/{id}.
type syncDetailData struct {
	base
	Run        *syncRunView
	StatusView syncStatusView
	ItemsView  generationItemsView
}

// handleSyncDetail melayani GET /sync/{id}.
func (a *Admin) handleSyncDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Sinkronisasi tidak ditemukan.")
		return
	}

	run, err := a.syncRunByID(ctx, id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca sinkronisasi.")
		return
	}
	if run == nil {
		a.renderError(w, r, http.StatusNotFound, "Sinkronisasi tidak ditemukan.")
		return
	}

	page, perPage := parsePage(r.URL.Query())
	itemsView, err := a.generationItemsView(ctx, run, page, perPage)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca progres konten.")
		return
	}

	a.renderPage(w, r, "sync_detail", syncDetailData{
		base:       a.base(w, r, fmt.Sprintf("Sync #%d", run.ID), "sync"),
		Run:        run,
		StatusView: a.syncStatusView(ctx, run),
		ItemsView:  itemsView,
	}, http.StatusOK)
}

// handleSyncStatus melayani GET /sync/{id}/status (§10.3).
func (a *Admin) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r, "id")
	if !ok {
		a.syncStatusNotFound(w, r)
		return
	}

	run, err := a.syncRunByID(ctx, id)
	if err != nil {
		if wantsJSON(r) {
			a.log.Error("admin: gagal membaca status sync", "run_id", id, "err", err)
			response.Error(w, response.CodeInternalError, nil)
			return
		}
		a.internalError(w, r, err, "Gagal membaca sinkronisasi.")
		return
	}
	if run == nil {
		a.syncStatusNotFound(w, r)
		return
	}

	view := a.syncStatusView(ctx, run)

	if wantsJSON(r) {
		if run.terminal() && run.RenditionsFailed > 0 {
			// Detail unit gagal tidak lagi tersedia (tabel task sudah dihapus),
			// tetapi kegagalan sebagian tetap ditandai code 1.
			response.PartialSuccess(w, view.Payload, nil)
			return
		}
		response.Success(w, view.Payload)
		return
	}

	a.renderPartial(w, "sync_status", view)
}

// syncStatusNotFound menjawab status sync yang tidak ada.
func (a *Admin) syncStatusNotFound(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		response.Error(w, response.CodeSyncNotFound, nil)
		return
	}
	a.renderError(w, r, http.StatusNotFound, "Sinkronisasi tidak ditemukan.")
}

// handleSyncItems melayani GET /sync/{id}/items: progres per series sebuah run
// untuk polling halaman detail (§10.3).
//
// Klien yang meminta JSON menerima envelope {items, summary}; selain itu
// dikirim fragmen HTML (template generation_items) yang bisa langsung
// menggantikan container di halaman. Run yang sudah terminal tetap dijawab 200
// agar skrip polling berhenti sendiri tanpa memperlakukan akhir run sebagai
// kesalahan.
func (a *Admin) handleSyncItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r, "id")
	if !ok {
		a.syncStatusNotFound(w, r)
		return
	}

	run, err := a.syncRunByID(ctx, id)
	if err != nil {
		if wantsJSON(r) {
			a.log.Error("admin: gagal membaca item konten", "run_id", id, "err", err)
			response.Error(w, response.CodeInternalError, nil)
			return
		}
		a.internalError(w, r, err, "Gagal membaca sinkronisasi.")
		return
	}
	if run == nil {
		a.syncStatusNotFound(w, r)
		return
	}

	page, perPage := parsePage(r.URL.Query())
	view, err := a.generationItemsView(ctx, run, page, perPage)
	if err != nil {
		if wantsJSON(r) {
			a.log.Error("admin: gagal membaca item generation", "run_id", run.ID, "err", err)
			response.Error(w, response.CodeInternalError, nil)
			return
		}
		a.internalError(w, r, err, "Gagal membaca progres konten.")
		return
	}

	if wantsJSON(r) {
		response.Success(w, view.payload())
		return
	}

	a.renderPartial(w, "generation_items", view)
}

// handleSyncCancel melayani POST /sync/{id}/cancel.
func (a *Admin) handleSyncCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Sinkronisasi tidak ditemukan.")
		return
	}

	run, err := a.syncRunByID(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca sinkronisasi.")
		return
	}
	if run == nil {
		a.renderError(w, r, http.StatusNotFound, "Sinkronisasi tidak ditemukan.")
		return
	}

	var cancelErr error
	if a.syncer != nil {
		cancelErr = a.syncer.Cancel(id)
	} else {
		cancelErr = errors.New("sinkronisasi tidak tersedia")
	}
	if cancelErr != nil && a.transfer != nil {
		if err := a.transfer.Cancel(id); err == nil {
			cancelErr = nil
		}
	}
	if cancelErr != nil {
		a.log.Warn("admin: gagal membatalkan sinkronisasi", "run_id", id, "err", cancelErr)
		a.flashTo(r, "error", fmt.Sprintf("Sinkronisasi #%d tidak dapat dibatalkan: %v", id, cancelErr))
		http.Redirect(w, r, fmt.Sprintf("/sync/%d", id), http.StatusSeeOther)
		return
	}

	a.flashTo(r, "success", fmt.Sprintf("Permintaan pembatalan sinkronisasi #%d dikirim.", id))
	http.Redirect(w, r, fmt.Sprintf("/sync/%d", id), http.StatusSeeOther)
}
