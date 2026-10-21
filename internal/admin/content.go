package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"vodlocal/internal/catalog"
	"vodlocal/internal/ingest"
	"vodlocal/internal/response"
	"vodlocal/internal/syncjob"
)

// Provider dan kategori yang dilayani modal "List Content".
//
// Tab VOD (Densgo) belum tersedia sehingga panel hanya melayani daftar series
// Dramahits, yang berkategori short.
const (
	// providerDramahits adalah kode provider untuk kategori short.
	providerDramahits = "dramahits"
	// providerDensgo adalah kode provider untuk kategori VOD (belum tersedia).
	providerDensgo = "densgo"
	// categoryShort adalah kategori default daftar series pada modal.
	categoryShort = catalog.CategoryShort
)

// maxCachedSeries membatasi cache slug agar satu sesi panjang tidak menahan
// memori tanpa batas. Nilainya jauh di atas kebutuhan normal (20 series per
// halaman) sehingga beberapa halaman yang dipilih bersamaan tetap terlayani.
const maxCachedSeries = 2000

// SeriesLister membaca satu halaman daftar series dari provider.
//
// Implementasi konkret berada di luar paket ini (klien provider Dramahits);
// panel hanya membutuhkan kontrak ini agar bisa diuji dengan stub.
type SeriesLister interface {
	// ListSeries mengembalikan halaman page (1-based) beserta metadatanya.
	ListSeries(ctx context.Context, page int) (catalog.SeriesPage, error)
}

// IngestStarter mengantrekan series pilihan pengguna ke worker ingest.
//
// Implementasi konkret berada di luar paket ini. Nilai balik pertama adalah ID
// sync run yang dibuat worker; ErrAlreadyRunning dilaporkan bila masih ada run
// yang belum terminal.
type IngestStarter interface {
	Start(ctx context.Context, selections []catalog.Series) (int64, error)
}

// contentCache adalah cache in-memory slug→series yang diisi setiap kali satu
// halaman daftar series diambil.
//
// Formulir hanya mengirim slug, jadi data lengkap series (judul, poster,
// episode) harus bisa ditemukan kembali saat submit. Cache diperbarui pada
// setiap pengambilan halaman: entri untuk series yang muncul kembali diganti
// data terbarunya, sementara entri halaman lain dipertahankan supaya pilihan
// yang dicentang di halaman berbeda tetap bisa disimpan.
type contentCache struct {
	mu    sync.Mutex
	items map[string]catalog.Series
	order []string
}

// newContentCache membuat cache kosong yang siap dipakai.
func newContentCache() *contentCache {
	return &contentCache{items: make(map[string]catalog.Series)}
}

// refresh memasukkan seluruh series satu halaman ke cache, mengganti entri
// lama dengan data terbaru, lalu membuang entri tertua bila melewati batas.
func (c *contentCache) refresh(items []catalog.Series) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.items == nil {
		c.items = make(map[string]catalog.Series, len(items))
	}
	for _, item := range items {
		slug := strings.TrimSpace(item.Slug)
		if slug == "" {
			continue
		}
		if _, seen := c.items[slug]; !seen {
			c.order = append(c.order, slug)
		}
		c.items[slug] = item
	}

	for len(c.order) > maxCachedSeries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

// lookup mengembalikan series dengan slug tersebut beserta status ditemukannya.
func (c *contentCache) lookup(slug string) (catalog.Series, bool) {
	if c == nil {
		return catalog.Series{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.items[strings.TrimSpace(slug)]
	return item, ok
}

// size melaporkan jumlah series yang tersimpan (dipakai test internal).
func (c *contentCache) size() int {
	if c == nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.items)
}

// contentItem adalah satu seri pada respons GET /content/short.
type contentItem struct {
	Provider     string   `json:"provider"`
	Category     string   `json:"category"`
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	PosterURL    string   `json:"poster_url"`
	Year         int      `json:"year"`
	EpisodeCount int      `json:"episode_count"`
	AgeRating    string   `json:"age_rating"`
	Genres       []string `json:"genres"`
	PublishedAt  string   `json:"published_at"`
	// AlreadyAdded menandai series yang sudah punya rendition di database,
	// sehingga modal tidak menawarkannya untuk dipilih (dan di-ingest) lagi.
	AlreadyAdded bool `json:"already_added"`
}

// contentMeta adalah bagian meta pada respons GET /content/short.
type contentMeta struct {
	Page     int `json:"page"`
	PerPage  int `json:"per_page"`
	Total    int `json:"total"`
	LastPage int `json:"last_page"`
}

// contentPage adalah bentuk data pada respons GET /content/short.
type contentPage struct {
	Items []contentItem `json:"items"`
	Meta  contentMeta   `json:"meta"`
}

// newContentItem menormalkan satu catalog.Series untuk JSON: provider/kategori
// diisi bila kosong dan genres tidak pernah null.
func newContentItem(series catalog.Series, provider string) contentItem {
	itemProvider := strings.TrimSpace(series.Provider)
	if itemProvider == "" {
		itemProvider = provider
	}
	category := strings.TrimSpace(series.Category)
	if category == "" {
		category = categoryShort
	}

	genres := make([]string, 0, len(series.Genres))
	for _, genre := range series.Genres {
		if trimmed := strings.TrimSpace(genre); trimmed != "" {
			genres = append(genres, trimmed)
		}
	}

	return contentItem{
		Provider:     itemProvider,
		Category:     category,
		ID:           series.ID,
		Slug:         series.Slug,
		Title:        series.Title,
		Description:  series.Description,
		PosterURL:    series.PosterURL,
		Year:         series.Year,
		EpisodeCount: series.EpisodeCount,
		AgeRating:    series.AgeRating,
		Genres:       genres,
		PublishedAt:  series.PublishedAt,
	}
}

// alreadyAddedQuery adalah pola query penanda konten yang sudah ada.
//
// Sebuah series dianggap sudah ada bila title-nya (dicocokkan lewat provider
// code + titles.external_id/slug provider) punya minimal satu rendition pada
// generation apa pun: baris renditions hanya dibuat untuk konten yang sudah
// pernah atau sedang di-ingest. Placeholder "%s" diisi sebanyak jumlah slug
// halaman, jadi satu halaman cukup satu query.
const alreadyAddedQuery = `
SELECT t.external_id
FROM titles t
JOIN playables p ON p.title_id = t.id
JOIN renditions r ON r.playable_id = p.id
JOIN providers pr ON pr.id = t.provider_id
WHERE pr.code = ? AND t.external_id IN (%s)
GROUP BY t.external_id`

// slugPlaceholders menyusun daftar placeholder "?,?,?" sebanyak n.
func slugPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// existingSlugs mengembalikan slug milik provider code yang sudah punya
// rendition di database.
//
// Seluruh slug dikirim dalam satu query (bukan satu query per item) supaya
// biaya per halaman tetap konstan.
func (a *Admin) existingSlugs(ctx context.Context, provider string, slugs []string) (map[string]struct{}, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" || len(slugs) == 0 {
		return map[string]struct{}{}, nil
	}

	query := fmt.Sprintf(alreadyAddedQuery, slugPlaceholders(len(slugs)))
	args := make([]any, 0, len(slugs)+1)
	args = append(args, provider)
	for _, slug := range slugs {
		args = append(args, slug)
	}

	rows, err := a.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	found := make(map[string]struct{}, len(slugs))
	for rows.Next() {
		var externalID string
		if err := rows.Scan(&externalID); err != nil {
			return nil, err
		}
		found[externalID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

// markAlreadyAdded mengisi contentItem.AlreadyAdded untuk satu halaman.
//
// Slug dikumpulkan per provider code, jadi halaman yang seluruhnya berasal dari
// satu provider (kasus normal modal ini) hanya memerlukan satu query. Bila
// query gagal, item provider tersebut ditandai sudah ada (fail-closed) supaya
// konten yang mungkin sudah masuk database tidak ditawarkan ulang; kegagalan
// hanya dicatat ke log dan tidak menggagalkan daftar.
func (a *Admin) markAlreadyAdded(ctx context.Context, items []contentItem) {
	grouped := make(map[string][]string, 1)
	order := make([]string, 0, 1)
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		provider := strings.TrimSpace(item.Provider)
		slug := strings.TrimSpace(item.Slug)
		if provider == "" || slug == "" {
			continue
		}
		key := provider + "\x00" + slug
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := grouped[provider]; !ok {
			order = append(order, provider)
		}
		grouped[provider] = append(grouped[provider], slug)
	}

	added := make(map[string]struct{}, len(items))
	for _, provider := range order {
		found, err := a.existingSlugs(ctx, provider, grouped[provider])
		if err != nil {
			a.log.Warn("admin: gagal memeriksa konten yang sudah ada",
				"provider", provider, "err", err)
			for _, slug := range grouped[provider] {
				added[provider+"\x00"+slug] = struct{}{}
			}
			continue
		}
		for slug := range found {
			added[provider+"\x00"+strings.TrimSpace(slug)] = struct{}{}
		}
	}

	for i := range items {
		_, ok := added[strings.TrimSpace(items[i].Provider)+"\x00"+strings.TrimSpace(items[i].Slug)]
		items[i].AlreadyAdded = ok
	}
}

// handleContentShort melayani GET /content/short?page=N.
//
// Handler selalu menjawab JSON envelope karena endpoint ini hanya dipanggil
// JavaScript modal "List Content". Setiap halaman yang berhasil diambil
// memperbarui cache slug agar POST /content/select bisa meresolusi pilihan.
func (a *Admin) handleContentShort(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if a.lister == nil {
		response.ErrorMsg(w, response.CodeServiceUnavailable,
			"Daftar konten tidak tersedia pada server ini.")
		return
	}

	page := 1
	if raw := strings.TrimSpace(queryGet(r.URL.Query(), "page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}

	result, err := a.lister.ListSeries(r.Context(), page)
	if err != nil {
		a.log.Error("admin: gagal mengambil daftar series",
			"provider", providerDramahits, "page", page, "err", err)
		response.ErrorMsg(w, contentErrorCode(err), "Gagal mengambil daftar series dari penyedia.")
		return
	}

	// Cache hanya diperbarui setelah data benar-benar diterima, sehingga
	// kegagalan sementara tidak menghapus pilihan yang sudah dibuat.
	a.content.refresh(result.Items)
	a.log.Debug("admin: cache konten diperbarui",
		"provider", providerDramahits, "page", page, "cached", a.content.size())

	meta := contentMeta{
		Page:     result.Page,
		PerPage:  result.PerPage,
		Total:    result.Total,
		LastPage: result.LastPage,
	}
	if meta.Page < 1 {
		meta.Page = page
	}
	if meta.LastPage < meta.Page {
		meta.LastPage = meta.Page
	}
	if meta.PerPage < 1 {
		meta.PerPage = len(result.Items)
	}

	items := make([]contentItem, 0, len(result.Items))
	for _, series := range result.Items {
		items = append(items, newContentItem(series, providerDramahits))
	}

	// Tandai series yang sudah ada di database agar modal tidak menawarkannya
	// untuk dipilih lagi. Satu query untuk seluruh slug halaman.
	a.markAlreadyAdded(r.Context(), items)

	response.Success(w, contentPage{Items: items, Meta: meta})
}

// contentErrorCode memetakan kegagalan lister ke kode envelope yang sesuai.
//
// Paket response tidak punya satu kode "upstream error" generik, jadi
// kegagalan yang jelas berupa timeout memakai CodeUpstreamTimeout dan sisanya
// CodeUpstreamUnavailable.
func contentErrorCode(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return response.CodeUpstreamTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return response.CodeUpstreamTimeout
	}
	return response.CodeUpstreamUnavailable
}

// handleContentSelect melayani POST /content/select.
//
// Formulir membawa provider dan slug berulang; slug diresolusi dari cache lalu
// diteruskan ke worker ingest. Sukses mengalihkan (303) ke /sync dengan flash
// berisi jumlah series dan ID run.
func (a *Admin) handleContentSelect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderError(w, r, http.StatusBadRequest, "Formulir tidak dapat dibaca.")
		return
	}

	if a.ingest == nil {
		a.renderContentMessage(w, r, http.StatusConflict,
			"Pemilihan konten tidak tersedia pada server ini.")
		return
	}

	provider := strings.TrimSpace(r.PostFormValue("provider"))
	if provider == "" {
		provider = providerDramahits
	}
	if provider == providerDensgo {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Densgo belum tersedia; hanya series Dramahits yang bisa diantrekan.")
		return
	}

	slugs := uniqueSlugs(r.PostForm["slug"])
	if len(slugs) == 0 {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Pilih minimal satu series sebelum menyimpan.")
		return
	}

	selections := make([]catalog.Series, 0, len(slugs))
	missing := make([]string, 0)
	for _, slug := range slugs {
		series, ok := a.content.lookup(slug)
		if !ok {
			missing = append(missing, slug)
			continue
		}
		if strings.TrimSpace(series.Provider) == "" {
			series.Provider = provider
		}
		selections = append(selections, series)
	}
	if len(missing) > 0 {
		a.log.Warn("admin: slug konten tidak ada di cache",
			"provider", provider, "missing", missing)
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%d series tidak ditemukan pada daftar terbaru (%s). Muat ulang daftar lalu pilih kembali.",
			len(missing), strings.Join(missing, ", ")))
		return
	}

	runID, err := a.ingest.Start(r.Context(), selections)
	if err != nil {
		if alreadyRunning(err) {
			a.renderContentMessage(w, r, http.StatusConflict,
				"Masih ada sinkronisasi yang berjalan; tunggu hingga selesai lalu coba lagi.")
			return
		}
		if errors.Is(err, ingest.ErrInsufficientStorage) {
			a.renderContentMessage(w, r, http.StatusInsufficientStorage,
				"Ruang penyimpanan tidak mencukupi; hapus konten lama terlebih dahulu.")
			return
		}
		a.internalError(w, r, err, "Gagal mengantrekan series pilihan.")
		return
	}

	a.log.Info("admin: series dimasukkan ke antrean",
		"provider", provider,
		"series", len(selections),
		"run_id", runID,
		"username", a.sessionUsername(r),
	)
	a.flashTo(r, "success", fmt.Sprintf("%d series dimasukkan ke antrean (run #%d)", len(selections), runID))
	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// Aksi "Remove all batch" pada modal "List Content".
const (
	// purgeConfirm adalah satu-satunya nilai field confirm yang diterima aksi
	// "Remove all batch".
	purgeConfirm = "PURGE"

	// generationDirPrefix dan stagingDirPrefix adalah awalan direktori di media
	// root yang ikut disapu purge: direktori generation hasil publish dan sisa
	// staging build yang terputus.
	generationDirPrefix = "gen-"
	stagingDirPrefix    = ".building-"
)

// handleContentPurge melayani POST /content/purge: aksi "Remove all batch" pada
// modal "List Content".
//
// Aksi ini menghapus SEMUA batch tanpa memandang status generation (active,
// building, retired, maupun failed): setiap direktori generation di media root,
// seluruh baris tabel generations (renditions, subtitles, dan generation_items
// ikut lewat ON DELETE CASCADE), playable/episode/title yang tidak lagi punya
// media, lalu cache artwork.
//
// Karena destruktif, permintaan ditolak selama masih ada sync run non-terminal
// (is_active = 1) dan wajib menyertakan field confirm=PURGE. Tabel di luar
// storage dan katalog — providers, genres, auth_users, devices, app_settings,
// audit_log — tidak pernah disentuh.
//
// Kegagalan menghapus folder menghentikan proses sebelum database diubah, agar
// baris generation tidak hilang sementara medianya masih tertinggal di disk.
// Langkah best-effort (menyapu direktori tak tercatat, artwork, dan pembersihan
// katalog) hanya dicatat ke log dan tidak membatalkan sisa proses.
func (a *Admin) handleContentPurge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Media tidak boleh dihapus saat worker masih menulis.
	active, err := a.activeSyncRun(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal memeriksa sinkronisasi yang berjalan.")
		return
	}
	if active != nil {
		a.renderContentMessage(w, r, http.StatusConflict, fmt.Sprintf(
			"Masih ada proses sync/ingest berjalan. Tunggu atau batalkan run #%d (status %s) sebelum menghapus semua batch.",
			active.ID, active.Status))
		return
	}

	// 2. Konfirmasi eksplisit: tindakan ini tidak bisa dibatalkan.
	if strings.TrimSpace(r.FormValue("confirm")) != purgeConfirm {
		a.renderContentMessage(w, r, http.StatusBadRequest,
			"Konfirmasi tidak sesuai: kirim field confirm=PURGE untuk menghapus semua batch.")
		return
	}

	// 3. Label yang tercatat di database.
	labels, err := a.generationLabels(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar generation.")
		return
	}

	// 4. Hapus satu per satu direktori generation. Kegagalan selain "tidak ada"
	// menghentikan proses sebelum database disentuh.
	if a.media != nil {
		for _, label := range labels {
			if err := a.media.Remove(label); err != nil && !isMissingFile(err) {
				a.log.Error("admin: gagal menghapus folder generation saat purge",
					"generation", label, "err", err)
				a.renderContentMessage(w, r, http.StatusInternalServerError, fmt.Sprintf(
					"Gagal menghapus folder media generation %s; belum ada data yang dihapus.", label))
				return
			}
		}
	}

	// 5. Sisa direktori gen-*/.building-* yang tidak lagi tercatat di database.
	recorded := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		recorded[label] = struct{}{}
	}
	a.purgeUnrecordedDirs(recorded)

	// 6. Kosongkan tabel generations; renditions/subtitles/generation_items ikut
	// terhapus lewat ON DELETE CASCADE.
	result, err := a.write.ExecContext(ctx, `DELETE FROM generations`)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghapus baris generation.")
		return
	}
	generations, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		a.log.Warn("admin: gagal menghitung generation terhapus", "err", rowsErr)
	}

	// 7. Buang katalog yang tidak lagi punya media (playables/episodes/titles).
	var catalogRows int64
	if a.store != nil {
		deleted, err := a.store.CleanupOrphans(ctx)
		if err != nil {
			a.log.Warn("admin: gagal membersihkan katalog setelah purge", "err", err)
		} else {
			catalogRows = deleted
		}
	}

	// 8. Kosongkan cache artwork supaya poster konten yang sudah hilang tidak
	// tampil lagi.
	a.purgeArtwork()

	a.log.Info("admin: semua batch dihapus",
		"generations", generations,
		"catalog_rows", catalogRows,
		"artwork_root", a.artworkRoot,
		"username", a.sessionUsername(r),
	)
	a.flashTo(r, "success", fmt.Sprintf(
		"Semua batch dihapus: %d generation dan %d baris katalog.", generations, catalogRows))
	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// generationLabels membaca seluruh label generation yang tercatat.
func (a *Admin) generationLabels(ctx context.Context) ([]string, error) {
	rows, err := a.read.QueryContext(ctx, `SELECT label FROM generations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	labels := make([]string, 0, 16)
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// purgeUnrecordedDirs membuang direktori gen-*/.building-* di media root yang
// tidak ada di recorded.
//
// Langkah ini best-effort: masalah filesystem hanya dicatat ke log agar
// penghapusan katalog tetap berjalan. Pengelola media yang tidak memenuhi
// MediaRooter dilewati, dan direktori lain di media root (mis. artwork yang
// letaknya di bawah media root) tidak disentuh karena namanya tidak memakai
// awalan generation.
func (a *Admin) purgeUnrecordedDirs(recorded map[string]struct{}) {
	rooter, ok := a.media.(MediaRooter)
	if !ok {
		return
	}
	root := strings.TrimSpace(rooter.Root())
	if root == "" {
		return
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if !isMissingFile(err) {
			a.log.Warn("admin: gagal membaca media root saat purge", "root", root, "err", err)
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !hasGenerationPrefix(name) {
			continue
		}
		if _, ok := recorded[name]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			a.log.Warn("admin: gagal menghapus direktori media sisa", "dir", name, "err", err)
			continue
		}
		a.log.Info("admin: direktori media sisa dihapus", "dir", name)
	}
}

// hasGenerationPrefix melaporkan apakah name adalah direktori generation atau
// staging build.
func hasGenerationPrefix(name string) bool {
	return strings.HasPrefix(name, generationDirPrefix) || strings.HasPrefix(name, stagingDirPrefix)
}

// purgeArtwork mengosongkan folder artwork lalu membuatnya kembali kosong.
//
// Best-effort: kegagalan hanya dicatat (Warn) karena katalog sudah terlanjur
// dihapus dan artwork yang tersisa tidak merusak konsistensi database.
func (a *Admin) purgeArtwork() {
	root := strings.TrimSpace(a.artworkRoot)
	if root == "" {
		return
	}
	if err := os.RemoveAll(root); err != nil {
		a.log.Warn("admin: gagal menghapus folder artwork", "root", root, "err", err)
		return
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		a.log.Warn("admin: gagal membuat ulang folder artwork", "root", root, "err", err)
	}
}

// renderContentMessage menampilkan halaman /sync dengan pesan pada status
// HTTP tertentu, sehingga pengguna tetap melihat riwayat run saat pemilihan
// konten ditolak.
func (a *Admin) renderContentMessage(w http.ResponseWriter, r *http.Request, status int, message string) {
	data, ok := a.syncListData(w, r, message)
	if !ok {
		return
	}
	a.renderPage(w, r, "sync", data, status)
}

// alreadyRunning melaporkan apakah err menandakan run yang masih berjalan.
//
// Panel sengaja tidak mengimpor paket worker, jadi selain sentinel
// syncjob.ErrAlreadyRunning, error dari worker lain yang memakai pesan
// "... masih berjalan" (mis. ingest.ErrAlreadyRunning) juga dikenali agar
// pengguna tetap mendapat 409, bukan 500.
func alreadyRunning(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syncjob.ErrAlreadyRunning) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "masih berjalan")
}

// uniqueSlugs merapikan daftar slug: trim, buang yang kosong, dan hilangkan
// duplikat dengan mempertahankan urutan kemunculan pertama.
func uniqueSlugs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	slugs := make([]string, 0, len(values))
	for _, value := range values {
		slug := strings.TrimSpace(value)
		if slug == "" {
			continue
		}
		if _, ok := seen[slug]; ok {
			continue
		}
		seen[slug] = struct{}{}
		slugs = append(slugs, slug)
	}
	return slugs
}
