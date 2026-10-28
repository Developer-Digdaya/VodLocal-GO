package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"vodlocal/internal/catalog"
	"vodlocal/internal/mediafs"
)

// generationView adalah satu baris generations untuk ringkasan di halaman
// /storage.
type generationView struct {
	ID         int64
	Label      string
	Status     string
	FileCount  int64
	TotalBytes int64
	CreatedAt  string
	RetiredAt  string
}

// StatusLabel menampilkan status batch tanpa istilah "retired".
func (g generationView) StatusLabel() string {
	switch g.Status {
	case "retired":
		return "lama"
	case "active":
		return "aktif"
	case "building":
		return "membangun"
	case "failed":
		return "gagal"
	default:
		return g.Status
	}
}

// GenerationStatusLabel adalah label status batch untuk baris konten.
func (c contentItemView) GenerationStatusLabel() string {
	switch c.GenerationStatus {
	case "retired":
		return "lama"
	case "active":
		return "aktif"
	case "building":
		return "membangun"
	case "failed":
		return "gagal"
	default:
		return c.GenerationStatus
	}
}

// contentItemView adalah satu baris generation_items yang siap dirender:
// satu baris = satu konten (series) pada sebuah generation.
type contentItemView struct {
	ID               int64
	SeriesTitle      string
	SeriesExternalID string
	Category         string
	Status           string
	EpisodesDone     int64
	TotalEpisodes    int64
	ProgressPercent  int
	GenerationLabel  string
	GenerationStatus string
	Bytes            int64
}

// storageFilter adalah filter daftar konten di /storage.
type storageFilter struct {
	Query    string
	Category string
	Status   string
}

// where menyusun klausa WHERE untuk generation_items.
func (f storageFilter) where() (string, []any) {
	clauses := []string{"1 = 1"}
	args := make([]any, 0, 4)

	if query := strings.TrimSpace(f.Query); query != "" {
		pattern := likeContains(query)
		clauses = append(clauses, `(gi.series_title LIKE ?`+likeEscapeClause+
			` OR gi.series_external_id LIKE ?`+likeEscapeClause+`)`)
		args = append(args, pattern, pattern)
	}
	if category := strings.TrimSpace(f.Category); category != "" {
		clauses = append(clauses, "gi.category = ?")
		args = append(args, category)
	}
	if status := strings.TrimSpace(f.Status); status != "" {
		clauses = append(clauses, "gi.status = ?")
		args = append(args, status)
	}

	return " WHERE " + strings.Join(clauses, " AND "), args
}

// storageData adalah data halaman /storage.
type storageData struct {
	base
	// pageInfo adalah pagination tabel konten.
	pageInfo
	// GenPageInfo adalah pagination tabel generation.
	GenPageInfo pageInfo
	Q           string
	Category    string
	Status      string
	// Items adalah konten (series) pada halaman ini.
	Items []contentItemView
	// Generations adalah ringkasan batch pada halaman generation saat ini.
	Generations []generationView
	// TotalBytes adalah jumlah ukuran konten pada halaman ini.
	TotalBytes int64
	// StorageBytes adalah jumlah total_bytes seluruh generation.
	StorageBytes int64
	// GenerationCount adalah jumlah seluruh generation.
	GenerationCount int64
	ActiveLabel     string
}

// handleStorage melayani GET /storage.
func (a *Admin) handleStorage(w http.ResponseWriter, r *http.Request) {
	a.renderStorage(w, r, http.StatusOK, "")
}

// renderStorage menampilkan daftar konten (beserta ringkasan generation)
// dengan pesan error opsional.
func (a *Admin) renderStorage(w http.ResponseWriter, r *http.Request, status int, errMessage string) {
	ctx := r.Context()
	values := r.URL.Query()

	filter := storageFilter{
		Query:    normalizeQuery(queryGet(values, "q")),
		Category: strings.ToLower(strings.TrimSpace(queryGet(values, "category"))),
		Status:   strings.ToLower(strings.TrimSpace(queryGet(values, "status"))),
	}
	if !validStorageCategory(filter.Category) {
		filter.Category = ""
	}
	if !validStorageStatus(filter.Status) {
		filter.Status = ""
	}

	page, perPage := parsePage(values)
	total, err := a.countContentItems(ctx, filter)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung konten.")
		return
	}
	info := newPageInfo(page, perPage, total)

	items, err := a.listContentItems(ctx, filter, info.PerPage, info.offset())
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar konten.")
		return
	}

	genPage, genPerPage := parsePageNamed(values, "gen_page", "gen_per_page", 10)
	genTotal, err := a.countGenerations(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung generation.")
		return
	}
	genInfo := newPageInfo(genPage, genPerPage, genTotal)
	generations, err := a.listGenerations(ctx, genInfo.PerPage, genInfo.offset())
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar generation.")
		return
	}

	storageBytes, err := a.storageBytes(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung ukuran storage.")
		return
	}

	active, err := a.activeGenerationView(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca generation aktif.")
		return
	}

	data := storageData{
		base:            a.base(w, r, "Storage", "storage"),
		pageInfo:        info,
		GenPageInfo:     genInfo,
		Q:               filter.Query,
		Category:        filter.Category,
		Status:          filter.Status,
		Items:           items,
		Generations:     generations,
		StorageBytes:    storageBytes,
		GenerationCount: genTotal,
	}
	if active != nil {
		data.ActiveLabel = active.Label
	}
	for _, item := range items {
		data.TotalBytes += item.Bytes
	}
	data.Error = errMessage

	a.renderPage(w, r, "storage", data, status)
}

// validStorageCategory melaporkan kategori yang bisa difilter.
func validStorageCategory(category string) bool {
	switch category {
	case "", catalog.CategoryShort, catalog.CategoryMovie, catalog.CategorySeries:
		return true
	default:
		return false
	}
}

// validStorageStatus melaporkan status generation_items yang bisa difilter.
func validStorageStatus(status string) bool {
	switch status {
	case "", "pending", "running", "done", "failed", "skipped":
		return true
	default:
		return false
	}
}

// listContentItems membaca satu halaman konten (generation_items) beserta
// label/status generation dan total ukuran rendition-nya, terbaru lebih dulu.
func (a *Admin) listContentItems(ctx context.Context, filter storageFilter, limit, offset int) ([]contentItemView, error) {
	where, filterArgs := filter.where()
	args := append(append([]any{}, filterArgs...), limit, offset)

	rows, err := a.read.QueryContext(ctx, `
SELECT gi.id, gi.series_title, gi.series_external_id, gi.category, gi.status,
       gi.episodes_done, gi.total_episodes, gi.progress_percent,
       g.label, g.status,
       COALESCE(SUM(r.total_bytes), 0) AS bytes
FROM generation_items gi
JOIN generations g ON g.id = gi.generation_id
LEFT JOIN titles t ON t.provider_id = gi.provider_id AND t.external_id = gi.series_external_id
LEFT JOIN playables p ON p.title_id = t.id
LEFT JOIN renditions r ON r.playable_id = p.id AND r.generation_id = gi.generation_id`+
		where+`
GROUP BY gi.id
ORDER BY g.created_at DESC, gi.id
LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]contentItemView, 0, limit)
	for rows.Next() {
		var item contentItemView
		if err := rows.Scan(&item.ID, &item.SeriesTitle, &item.SeriesExternalID,
			&item.Category, &item.Status, &item.EpisodesDone, &item.TotalEpisodes,
			&item.ProgressPercent, &item.GenerationLabel, &item.GenerationStatus,
			&item.Bytes); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// countContentItems menghitung baris yang cocok dengan filter.
func (a *Admin) countContentItems(ctx context.Context, filter storageFilter) (int64, error) {
	where, args := filter.where()
	var total int64
	err := a.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM generation_items gi`+where, args...).Scan(&total)
	return total, err
}

// listGenerations membaca satu halaman generation, terbaru lebih dulu.
func (a *Admin) listGenerations(ctx context.Context, limit, offset int) ([]generationView, error) {
	rows, err := a.read.QueryContext(ctx, `
SELECT id, label, status, file_count, total_bytes, created_at, COALESCE(retired_at, '')
FROM generations
ORDER BY created_at DESC, id DESC
LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	generations := make([]generationView, 0, limit)
	for rows.Next() {
		var generation generationView
		if err := rows.Scan(&generation.ID, &generation.Label, &generation.Status,
			&generation.FileCount, &generation.TotalBytes, &generation.CreatedAt,
			&generation.RetiredAt); err != nil {
			return nil, err
		}
		generations = append(generations, generation)
	}
	return generations, rows.Err()
}

// countGenerations menghitung seluruh generation.
func (a *Admin) countGenerations(ctx context.Context) (int64, error) {
	var total int64
	err := a.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM generations`).Scan(&total)
	return total, err
}

// storageBytes menjumlahkan total_bytes seluruh generation.
func (a *Admin) storageBytes(ctx context.Context) (int64, error) {
	var total int64
	err := a.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_bytes), 0) FROM generations`).Scan(&total)
	return total, err
}

// storageReturnURL mempertahankan halaman/filter setelah aksi hapus.
func storageReturnURL(r *http.Request) string {
	keys := []string{"q", "category", "status", "page", "per_page", "gen_page", "gen_per_page"}
	values := url.Values{}
	for _, key := range keys {
		if value := strings.TrimSpace(r.FormValue(key)); value != "" {
			values.Set(key, value)
		}
	}
	if len(values) == 0 {
		return "/storage"
	}
	return "/storage?" + values.Encode()
}

// generationByID membaca satu generation, atau nil bila tidak ada.
func (a *Admin) generationByID(ctx context.Context, id int64) (*generationView, error) {
	var generation generationView
	err := a.read.QueryRowContext(ctx, `
SELECT id, label, status, file_count, total_bytes, created_at, COALESCE(retired_at, '')
FROM generations WHERE id = ?`, id,
	).Scan(&generation.ID, &generation.Label, &generation.Status, &generation.FileCount,
		&generation.TotalBytes, &generation.CreatedAt, &generation.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &generation, nil
}

// activeGenerationView membaca generation berstatus active, atau nil.
func (a *Admin) activeGenerationView(ctx context.Context) (*generationView, error) {
	var generation generationView
	err := a.read.QueryRowContext(ctx, `
SELECT id, label, status, file_count, total_bytes, created_at, COALESCE(retired_at, '')
FROM generations WHERE status = 'active'`,
	).Scan(&generation.ID, &generation.Label, &generation.Status, &generation.FileCount,
		&generation.TotalBytes, &generation.CreatedAt, &generation.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &generation, nil
}

// handleStorageDelete melayani POST /storage/generations/{id}/delete (10.5).
//
// Semua status boleh dihapus: active, building, retired, dan failed. Tidak ada
// lagi validasi "last batch" / generation.Deletable(); satu-satunya syarat
// adalah field confirm=DELETE. Folder di disk dihapus lebih dulu (bila
// pengelola media tersedia), lalu baris database dihapus sehingga rendisi dan
// subtitle ikut terhapus lewat ON DELETE CASCADE. Folder yang sudah tidak ada
// tidak menghalangi penghapusan baris. Setelah itu CleanupOrphans membuang
// playable/episode/title yang medianya tidak tersisa.
//
// Handler ini dipertahankan untuk kompatibilitas (dan test lama); tombolnya
// tidak lagi ditampilkan di halaman /storage, yang sekarang menghapus per
// konten lewat handleStorageItemDelete.
func (a *Admin) handleStorageDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Generation tidak ditemukan.")
		return
	}

	if strings.TrimSpace(r.FormValue("confirm")) != "DELETE" {
		a.renderStorage(w, r, http.StatusBadRequest,
			"Konfirmasi tidak sesuai: kirim field confirm=DELETE untuk menghapus generation.")
		return
	}

	generation, err := a.generationByID(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca generation.")
		return
	}
	if generation == nil {
		a.renderError(w, r, http.StatusNotFound, "Generation tidak ditemukan.")
		return
	}

	if a.media != nil {
		if err := a.media.Remove(generation.Label); err != nil && !isMissingFile(err) {
			a.log.Error("admin: gagal menghapus folder generation",
				"generation", generation.Label, "err", err)
			a.renderStorage(w, r, http.StatusInternalServerError, fmt.Sprintf(
				"Gagal menghapus folder media generation %s.", generation.Label))
			return
		}
	}

	if _, err := a.write.ExecContext(r.Context(),
		`DELETE FROM generations WHERE id = ?`, id,
	); err != nil {
		a.internalError(w, r, err, "Gagal menghapus generation.")
		return
	}

	// Baris renditions sudah ikut terhapus lewat ON DELETE CASCADE. Katalog yang
	// medianya tidak lagi tersisa (playable/episode/title) dibuang menyusul;
	// kegagalan langkah ini tidak membatalkan penghapusan generation yang sudah
	// berhasil.
	if deleted, err := a.store.CleanupOrphans(r.Context()); err != nil {
		a.log.Warn("admin: gagal membersihkan katalog tanpa media",
			"generation", generation.Label, "err", err)
	} else if deleted > 0 {
		a.log.Info("admin: baris katalog tanpa media dihapus",
			"generation", generation.Label, "rows", deleted)
	}

	a.log.Info("admin: generation dihapus",
		"generation", generation.Label, "status", generation.Status, "username", a.sessionUsername(r))
	a.flashTo(r, "success", fmt.Sprintf("Generation %s dihapus.", generation.Label))
	http.Redirect(w, r, storageReturnURL(r), http.StatusSeeOther)
}

// errUnsafeMediaPath menandai folder series yang, setelah dinormalkan, keluar
// dari direktori generation (upaya path traversal).
var errUnsafeMediaPath = errors.New("admin: path media konten keluar dari direktori generation")

// storageItem adalah satu baris generation_items beserta konteks yang
// dibutuhkan penghapusan per konten.
type storageItem struct {
	ID               int64
	GenerationID     int64
	GenerationLabel  string
	ProviderID       int64
	ProviderCode     string
	Category         string
	SeriesExternalID string
	SeriesTitle      string
}

// storageItemByID membaca satu generation_items beserta generation dan provider
// code-nya, atau nil bila tidak ada.
func (a *Admin) storageItemByID(ctx context.Context, id int64) (*storageItem, error) {
	var item storageItem
	err := a.read.QueryRowContext(ctx, `
SELECT gi.id, gi.generation_id, g.label, gi.provider_id, COALESCE(p.code, ''),
       gi.category, gi.series_external_id, gi.series_title
FROM generation_items gi
JOIN generations g ON g.id = gi.generation_id
LEFT JOIN providers p ON p.id = gi.provider_id
WHERE gi.id = ?`, id,
	).Scan(&item.ID, &item.GenerationID, &item.GenerationLabel, &item.ProviderID,
		&item.ProviderCode, &item.Category, &item.SeriesExternalID, &item.SeriesTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// seriesPlaylistPath mencari satu playlist rendition milik title (provider +
// series_external_id) pada generation tertentu. String kosong berarti tidak ada
// rendition yang punya playlist_path (mis. konten gagal diunduh).
func (a *Admin) seriesPlaylistPath(ctx context.Context, generationID, providerID int64, externalID string) (string, error) {
	var playlist sql.NullString
	err := a.read.QueryRowContext(ctx, `
SELECT r.playlist_path
FROM renditions r
JOIN playables p ON p.id = r.playable_id
JOIN titles t ON t.id = p.title_id
WHERE r.generation_id = ? AND t.provider_id = ? AND t.external_id = ?
  AND r.playlist_path IS NOT NULL AND r.playlist_path <> ''
LIMIT 1`, generationID, providerID, externalID,
	).Scan(&playlist)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(playlist.String), nil
}

// seriesRootFromPlaylist menurunkan folder konten relatif dari playlist_path.
// Series memakai 5 level (provider/series/<uuid>/<episode>/<quality>/index.m3u8),
// movie memakai 4 level (provider/movie/<id>/<quality>/index.m3u8).
func seriesRootFromPlaylist(playlistPath string, movie bool) string {
	trimmed := strings.TrimSpace(playlistPath)
	if trimmed == "" {
		return ""
	}

	// playlist_path selalu memakai pemisah '/' relatif ke root generation.
	root := path.Dir(path.Dir(path.Dir(filepath.ToSlash(trimmed))))
	if movie {
		root = path.Dir(path.Dir(filepath.ToSlash(trimmed)))
	}
	switch {
	case root == "", root == ".", root == "/":
		return ""
	case root == "..", strings.HasPrefix(root, "../"):
		return ""
	}
	return root
}

// withinDir melaporkan apakah target berada di dalam base (bukan base itu
// sendiri). Dipakai sebagai pagar anti traversal.
func withinDir(base, target string) bool {
	if target == base {
		return false
	}
	return strings.HasPrefix(target, base+string(os.PathSeparator))
}

// removeSeriesMedia menghapus `<mediaRoot>/<label>/<seriesRoot>` dari disk.
//
// Folder yang sudah tidak ada tidak dianggap kegagalan. Pengelola media yang
// tidak memenuhi MediaRooter dilewati (tidak ada akar direktori yang bisa
// dipakai). Hasil filepath.Abs wajib berawalan `<mediaRoot>/<label>`; bila
// tidak, errUnsafeMediaPath dikembalikan agar handler membatalkan permintaan.
func (a *Admin) removeSeriesMedia(label, playlistPath string, movie bool) error {
	rooter, ok := a.media.(MediaRooter)
	if a.media == nil || !ok {
		a.log.Debug("admin: pengelola media tanpa Root(); folder series tidak dihapus",
			"generation", label)
		return nil
	}

	mediaRoot := strings.TrimSpace(rooter.Root())
	if mediaRoot == "" {
		return nil
	}

	seriesRoot := seriesRootFromPlaylist(playlistPath, movie)
	if seriesRoot == "" {
		return nil
	}

	base, err := filepath.Abs(filepath.Join(mediaRoot, label))
	if err != nil {
		return fmt.Errorf("admin: path generation %s tidak valid: %w", label, err)
	}
	target, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(seriesRoot)))
	if err != nil {
		return fmt.Errorf("admin: path konten %s tidak valid: %w", seriesRoot, err)
	}
	if !withinDir(base, target) {
		a.log.Error("admin: path konten di luar direktori generation",
			"generation", label, "series_root", seriesRoot, "target", target)
		return errUnsafeMediaPath
	}

	if err := os.RemoveAll(target); err != nil && !isMissingFile(err) {
		return err
	}
	a.log.Info("admin: folder series dihapus", "generation", label, "dir", seriesRoot)
	return nil
}

// deleteItemRows menghapus renditions, subtitles, dan baris generation_items
// milik satu konten dalam satu transaksi, lalu menghapus baris generation_items.
//
// Rendition/subtitle milik title yang sama pada generation lain tidak
// tersentuh karena seluruh perintah dibatasi generation_id.
func (a *Admin) deleteItemRows(ctx context.Context, item *storageItem) error {
	tx, err := a.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const playableOfTitle = `SELECT p.id FROM playables p
	JOIN titles t ON t.id = p.title_id
	WHERE t.provider_id = ? AND t.external_id = ?`

	for _, query := range []string{
		`DELETE FROM renditions WHERE generation_id = ? AND playable_id IN (` + playableOfTitle + `)`,
		`DELETE FROM subtitles WHERE generation_id = ? AND playable_id IN (` + playableOfTitle + `)`,
	} {
		if _, err := tx.ExecContext(ctx, query,
			item.GenerationID, item.ProviderID, item.SeriesExternalID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM generation_items WHERE id = ?`, item.ID); err != nil {
		return err
	}

	return tx.Commit()
}

// refreshGenerationBytes menghitung ulang generations.total_bytes dari jumlah
// rendition yang tersisa. Best-effort: kegagalan hanya dicatat.
func (a *Admin) refreshGenerationBytes(ctx context.Context, generationID int64) {
	if _, err := a.write.ExecContext(ctx, `
UPDATE generations
   SET total_bytes = (SELECT COALESCE(SUM(total_bytes), 0) FROM renditions WHERE generation_id = ?)
 WHERE id = ?`, generationID, generationID); err != nil {
		a.log.Warn("admin: gagal memperbarui total_bytes generation",
			"generation_id", generationID, "err", err)
	}
}

// handleStorageItemDelete melayani POST /storage/items/{id}/delete: hapus satu
// konten (series) dari sebuah generation.
//
// Urutannya: baca item → tentukan folder series dari playlist_path lalu hapus
// dari disk (bila pengelola media menyediakan Root) → hapus renditions,
// subtitles, dan generation_items dalam satu transaksi → CleanupOrphans untuk
// katalog yang medianya habis → perbarui total_bytes generation. Folder yang
// sudah tidak ada tidak menghalangi penghapusan baris, dan konten tanpa
// rendition hanya menghapus baris database tanpa error.
func (a *Admin) handleStorageItemDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Konten tidak ditemukan.")
		return
	}

	if strings.TrimSpace(r.FormValue("confirm")) != "DELETE" {
		a.renderStorage(w, r, http.StatusBadRequest,
			"Konfirmasi tidak sesuai: kirim field confirm=DELETE untuk menghapus konten.")
		return
	}

	item, err := a.storageItemByID(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca konten.")
		return
	}
	if item == nil {
		a.renderError(w, r, http.StatusNotFound, "Konten tidak ditemukan.")
		return
	}

	playlist, err := a.seriesPlaylistPath(r.Context(),
		item.GenerationID, item.ProviderID, item.SeriesExternalID)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca data media konten.")
		return
	}
	if playlist != "" {
		if err := a.removeSeriesMedia(item.GenerationLabel, playlist, item.Category == "movie"); err != nil {
			if errors.Is(err, errUnsafeMediaPath) {
				a.renderStorage(w, r, http.StatusBadRequest,
					"Path media konten tidak valid; penghapusan dibatalkan.")
				return
			}
			a.log.Error("admin: gagal menghapus folder series",
				"generation", item.GenerationLabel,
				"series", item.SeriesExternalID, "err", err)
			a.renderStorage(w, r, http.StatusInternalServerError, fmt.Sprintf(
				"Gagal menghapus folder media konten %s.", item.SeriesTitle))
			return
		}
	}

	if err := a.deleteItemRows(r.Context(), item); err != nil {
		a.internalError(w, r, err, "Gagal menghapus konten.")
		return
	}

	// Katalog yang medianya tidak lagi tersisa (playable/episode/title) dibuang
	// menyusul; kegagalan langkah ini tidak membatalkan penghapusan yang sudah
	// berhasil.
	if deleted, err := a.store.CleanupOrphans(r.Context()); err != nil {
		a.log.Warn("admin: gagal membersihkan katalog tanpa media",
			"generation", item.GenerationLabel, "err", err)
	} else if deleted > 0 {
		a.log.Info("admin: baris katalog tanpa media dihapus",
			"generation", item.GenerationLabel, "rows", deleted)
	}

	a.refreshGenerationBytes(r.Context(), item.GenerationID)

	a.log.Info("admin: konten dihapus",
		"generation", item.GenerationLabel,
		"provider", item.ProviderCode,
		"series", item.SeriesExternalID,
		"item", item.ID,
		"username", a.sessionUsername(r))
	a.flashTo(r, "success", fmt.Sprintf("Konten %s dihapus dari %s.",
		item.SeriesTitle, item.GenerationLabel))
	http.Redirect(w, r, storageReturnURL(r), http.StatusSeeOther)
}

// isMissingFile melaporkan apakah err berarti berkas/direktori sudah tidak ada.
func isMissingFile(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, mediafs.ErrNotFound)
}
