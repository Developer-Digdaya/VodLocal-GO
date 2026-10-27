package admin

import (
	"context"
	"encoding/json"
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
// Modal punya tiga tab — Movie (Densgo), Series (Densgo), dan Short
// (Dramahits) — sehingga satu provider bisa muncul pada lebih dari satu
// kategori.
const (
	// providerDramahits adalah kode provider untuk kategori short.
	providerDramahits = "dramahits"
	// providerDensgo adalah kode provider untuk kategori movie dan series.
	providerDensgo = "densgo"
	// categoryMovie adalah kategori film Densgo dan tab bawaan modal.
	categoryMovie = catalog.CategoryMovie
	// categorySeries adalah kategori serial Densgo.
	categorySeries = catalog.CategorySeries
	// categoryShort adalah kategori Dramahits.
	categoryShort = catalog.CategoryShort
)

// maxCachedSeries membatasi cache slug agar satu sesi panjang tidak menahan
// memori tanpa batas. Nilainya jauh di atas kebutuhan normal (20 series per
// halaman) sehingga beberapa halaman yang dipilih bersamaan tetap terlayani.
const maxCachedSeries = 2000

// maxMissingSelections membatasi jumlah pilihan bermasalah yang disebut pada
// pesan 422 agar halaman tetap terbaca saat pengguna memilih banyak item.
const maxMissingSelections = 5

// SeriesLister membaca satu halaman daftar konten dari provider untuk satu
// kategori.
//
// Implementasi konkretnya adalah catalog.SourceSet yang dipasang server:
// SourceSet memilih catalog.Source berdasarkan kategori lalu memanggil
// ListSeries-nya. Panel hanya membutuhkan kontrak ini agar bisa diuji dengan
// stub.
type SeriesLister interface {
	// ListSeries mengembalikan halaman page (1-based) pada kategori category
	// (catalog.CategoryMovie, CategorySeries, atau CategoryShort) beserta
	// metadatanya.
	ListSeries(ctx context.Context, category string, page int) (catalog.SeriesPage, error)
}

// IngestStarter mengantrekan series pilihan pengguna ke worker ingest.
//
// Implementasi konkret berada di luar paket ini. Nilai balik pertama adalah ID
// sync run yang dibuat worker; ErrAlreadyRunning dilaporkan bila masih ada run
// yang belum terminal.
type IngestStarter interface {
	Start(ctx context.Context, selections []catalog.Series) (int64, error)
}

// contentCache adalah cache kunci konten→series yang diisi setiap kali satu
// halaman daftar konten diambil.
//
// Kunci cache memuat provider dan kategori, bukan slug saja: Densgo memakai ID
// provider sebagai slug untuk movie maupun series, sehingga slug yang sama bisa
// menunjuk konten berbeda, dan pilihan pengguna boleh mencampur beberapa tab.
// Kunci disusun lewat contentKey (provider + kategori + slug).
//
// Formulir hanya mengirim identitas pilihan, jadi data lengkap konten (judul,
// poster, jumlah episode) harus bisa ditemukan kembali saat submit. Cache
// diperbarui pada setiap pengambilan halaman: entri untuk konten yang muncul
// kembali diganti data terbarunya, sementara entri halaman/tab lain
// dipertahankan supaya pilihan yang dicentang di halaman berbeda tetap bisa
// disimpan.
type contentCache struct {
	mu    sync.Mutex
	items map[string]catalog.Series
	order []string
}

// newContentCache membuat cache kosong yang siap dipakai.
func newContentCache() *contentCache {
	return &contentCache{items: make(map[string]catalog.Series)}
}

// contentKey menyusun kunci cache/lookup dari provider, kategori, dan slug.
//
// Pemisah NUL dipakai karena tidak mungkin muncul pada kode provider, kategori,
// maupun slug sehingga kunci tidak ambigu.
func contentKey(provider, category, slug string) string {
	return strings.TrimSpace(provider) + "\x00" + strings.TrimSpace(category) + "\x00" + strings.TrimSpace(slug)
}

// refresh memasukkan seluruh konten satu halaman ke cache, mengganti entri
// lama dengan data terbaru, lalu membuang entri tertua bila melewati batas.
//
// Entri tanpa provider, kategori, atau slug dilewati karena tidak bisa
// diresolusi kembali saat formulir dikirim.
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
		if strings.TrimSpace(item.Provider) == "" ||
			strings.TrimSpace(item.Category) == "" ||
			strings.TrimSpace(item.Slug) == "" {
			continue
		}
		key := contentKey(item.Provider, item.Category, item.Slug)
		if _, seen := c.items[key]; !seen {
			c.order = append(c.order, key)
		}
		c.items[key] = item
	}

	for len(c.order) > maxCachedSeries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

// lookup mengembalikan konten dengan provider, kategori, dan slug tersebut
// beserta status ditemukannya.
func (c *contentCache) lookup(provider, category, slug string) (catalog.Series, bool) {
	if c == nil {
		return catalog.Series{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.items[contentKey(provider, category, slug)]
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

// contentItem adalah satu konten pada respons GET /content.
type contentItem struct {
	Provider     string `json:"provider"`
	Category     string `json:"category"`
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	PosterURL    string `json:"poster_url"`
	Year         int    `json:"year"`
	EpisodeCount int    `json:"episode_count"`
	AgeRating    string `json:"age_rating"`
	PublishedAt  string `json:"published_at"`
	// AlreadyAdded menandai konten yang sudah punya rendition di database,
	// sehingga modal tidak menawarkannya untuk dipilih (dan di-ingest) lagi.
	AlreadyAdded bool `json:"already_added"`
}

// contentMeta adalah bagian meta pada respons GET /content.
type contentMeta struct {
	Page     int `json:"page"`
	PerPage  int `json:"per_page"`
	Total    int `json:"total"`
	LastPage int `json:"last_page"`
}

// contentPage adalah bentuk data pada respons GET /content.
type contentPage struct {
	Items []contentItem `json:"items"`
	Meta  contentMeta   `json:"meta"`
}

// normalizeSeries mengisi Provider dan Category yang kosong sesuai kategori
// sumber: dramahits/short untuk short, densgo/movie untuk movie, dan
// densgo/series untuk series. Nilai yang sudah diisi provider dibiarkan apa
// adanya.
func normalizeSeries(series catalog.Series, category string) catalog.Series {
	if strings.TrimSpace(series.Provider) == "" {
		series.Provider = defaultProvider(category)
	}
	if strings.TrimSpace(series.Category) == "" {
		series.Category = category
	}
	return series
}

// defaultProvider mengembalikan kode provider bawaan sebuah kategori.
func defaultProvider(category string) string {
	switch category {
	case categoryMovie, categorySeries:
		return providerDensgo
	case categoryShort:
		return providerDramahits
	default:
		return providerDramahits
	}
}

// newContentItem menormalkan satu catalog.Series (yang sudah lewat
// normalizeSeries) untuk JSON.
func newContentItem(series catalog.Series) contentItem {
	return contentItem{
		Provider:     strings.TrimSpace(series.Provider),
		Category:     strings.TrimSpace(series.Category),
		ID:           series.ID,
		Slug:         series.Slug,
		Title:        series.Title,
		Description:  series.Description,
		PosterURL:    series.PosterURL,
		Year:         series.Year,
		EpisodeCount: series.EpisodeCount,
		AgeRating:    series.AgeRating,
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

// handleContent melayani GET /content?category=movie|series|short&page=N.
//
// Handler selalu menjawab JSON envelope karena endpoint ini hanya dipanggil
// JavaScript modal "List Content". Setiap halaman yang berhasil diambil
// memperbarui cache konten agar POST /content/select bisa meresolusi pilihan.
func (a *Admin) handleContent(w http.ResponseWriter, r *http.Request) {
	a.serveContent(w, r, "")
}

// handleContentShort melayani GET /content/short?page=N.
//
// Endpoint lama ini dipertahankan sebagai pembungkus supaya pemanggil versi
// sebelumnya tetap bekerja; perilakunya sama dengan
// GET /content?category=short.
func (a *Admin) handleContentShort(w http.ResponseWriter, r *http.Request) {
	a.serveContent(w, r, categoryShort)
}

// serveContent menjawab satu halaman daftar konten sebagai JSON envelope.
//
// category kosong berarti kategori dibaca dari query (bawaan movie); nilai
// tidak kosong memaksa kategori tersebut (dipakai handleContentShort).
func (a *Admin) serveContent(w http.ResponseWriter, r *http.Request, category string) {
	w.Header().Set("Cache-Control", "no-store")

	if a.lister == nil {
		response.ErrorMsg(w, response.CodeServiceUnavailable,
			"Daftar konten tidak tersedia pada server ini.")
		return
	}

	if category == "" {
		raw := normalizeCategory(queryGet(r.URL.Query(), "category"))
		switch {
		case raw == "":
			category = categoryMovie
		case validCategory(raw):
			category = raw
		default:
			response.ErrorMsg(w, response.CodeValidationError, fmt.Sprintf(
				"Kategori %q tidak dikenal; gunakan movie, series, atau short.", raw))
			return
		}
	}

	page := 1
	if raw := strings.TrimSpace(queryGet(r.URL.Query(), "page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}

	result, err := a.lister.ListSeries(r.Context(), category, page)
	if err != nil {
		a.log.Error("admin: gagal mengambil daftar konten",
			"category", category, "page", page, "err", err)
		response.ErrorMsg(w, contentErrorCode(err), "Gagal mengambil daftar konten dari penyedia.")
		return
	}

	// Provider/kategori dilengkapi lebih dulu supaya cache dan JSON memakai
	// identitas yang sama (Densgo movie dan series boleh berbagi slug).
	seriesItems := make([]catalog.Series, 0, len(result.Items))
	for _, series := range result.Items {
		seriesItems = append(seriesItems, normalizeSeries(series, category))
	}

	// Cache hanya diperbarui setelah data benar-benar diterima, sehingga
	// kegagalan sementara tidak menghapus pilihan yang sudah dibuat.
	a.content.refresh(seriesItems)
	a.log.Debug("admin: cache konten diperbarui",
		"category", category, "page", page, "cached", a.content.size())

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

	items := make([]contentItem, 0, len(seriesItems))
	for _, series := range seriesItems {
		items = append(items, newContentItem(series))
	}

	// Tandai konten yang sudah ada di database agar modal tidak menawarkannya
	// untuk dipilih lagi. Satu query untuk seluruh slug halaman.
	a.markAlreadyAdded(r.Context(), items)

	response.Success(w, contentPage{Items: items, Meta: meta})
}

// normalizeCategory merapikan nilai kategori dari query atau formulir.
//
// Nilai kosong dikembalikan sebagai string kosong agar pemanggil bisa memakai
// bawaannya sendiri.
func normalizeCategory(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validCategory melaporkan apakah category termasuk kategori yang dilayani
// modal "List Content".
func validCategory(category string) bool {
	switch category {
	case categoryMovie, categorySeries, categoryShort:
		return true
	default:
		return false
	}
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

// contentSelection adalah satu entri field "selections" pada POST
// /content/select. Modal mengirim seluruh pilihan sebagai satu field JSON:
//
//	[{"provider":"densgo","category":"movie","slug":"<movie-id>"}]
type contentSelection struct {
	Provider string `json:"provider"`
	Category string `json:"category"`
	Slug     string `json:"slug"`
}

// normalized melengkapi provider/kategori yang kosong pada satu pilihan.
//
// Pilihan tanpa kategori dianggap movie bila providernya Densgo dan short bila
// bukan (Dramahits), sehingga formulir versi lama — yang hanya mengirim
// provider + slug — tetap bekerja. Pilihan tanpa provider memakai provider
// bawaan kategorinya.
func (s contentSelection) normalized() (provider, category, slug string) {
	provider = strings.TrimSpace(s.Provider)
	category = normalizeCategory(s.Category)
	slug = strings.TrimSpace(s.Slug)

	if category == "" {
		if strings.EqualFold(provider, providerDensgo) {
			category = categoryMovie
		} else {
			category = categoryShort
		}
	}
	if provider == "" {
		provider = defaultProvider(category)
	}
	return provider, category, slug
}

// parseSelectionJSON mem-parse field "selections" (array JSON). Field kosong
// berarti formulir versi lama, jadi tidak ada pilihan yang dihasilkan.
func parseSelectionJSON(raw string) ([]contentSelection, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var selections []contentSelection
	if err := json.Unmarshal([]byte(raw), &selections); err != nil {
		return nil, err
	}
	return selections, nil
}

// legacySelections menyusun pilihan dari formulir versi lama: field provider
// (opsional) plus slug berulang. Kategori tidak dikirim formulir lama sehingga
// contentSelection.normalized yang menentukannya.
func legacySelections(r *http.Request) []contentSelection {
	provider := strings.TrimSpace(r.PostFormValue("provider"))
	category := normalizeCategory(r.PostFormValue("category"))

	slugs := uniqueSlugs(r.PostForm["slug"])
	if len(slugs) == 0 {
		return nil
	}

	selections := make([]contentSelection, 0, len(slugs))
	for _, slug := range slugs {
		selections = append(selections, contentSelection{Provider: provider, Category: category, Slug: slug})
	}
	return selections
}

// handleContentSelect melayani POST /content/select.
//
// Formulir membawa field "selections" berisi JSON pilihan (provider, kategori,
// slug). Formulir versi lama yang hanya mengirim provider + slug berulang tetap
// diterima. Setiap pilihan diresolusi dari cache berdasarkan
// provider+kategori+slug, lalu diteruskan ke worker ingest. Sukses mengalihkan
// (303) ke /sync dengan flash berisi jumlah konten dan ID run.
//
// Pilihan lintas provider sengaja tidak ditolak di sini: ingest yang menentukan
// dukungannya.
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

	selections, err := parseSelectionJSON(r.PostFormValue("selections"))
	if err != nil {
		a.log.Warn("admin: field selections tidak dapat dibaca", "err", err)
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Format pilihan konten tidak dapat dibaca. Muat ulang daftar lalu pilih kembali.")
		return
	}
	if len(selections) == 0 {
		selections = legacySelections(r)
	}
	if len(selections) == 0 {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Pilih minimal satu konten sebelum menyimpan.")
		return
	}

	chosen := make([]catalog.Series, 0, len(selections))
	missing := make([]string, 0)
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		provider, category, slug := selection.normalized()
		if slug == "" {
			continue
		}

		key := contentKey(provider, category, slug)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		series, ok := a.content.lookup(provider, category, slug)
		if !ok {
			missing = append(missing, provider+"/"+category+"/"+slug)
			continue
		}
		if strings.TrimSpace(series.Provider) == "" {
			series.Provider = provider
		}
		if strings.TrimSpace(series.Category) == "" {
			series.Category = category
		}
		chosen = append(chosen, series)
	}

	if len(missing) > 0 {
		a.log.Warn("admin: konten pilihan tidak ada di cache",
			"requested", len(selections), "missing", missing)
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%d konten tidak ditemukan pada daftar terbaru (%s). Muat ulang daftar lalu pilih kembali.",
			len(missing), strings.Join(limitList(missing, maxMissingSelections), ", ")))
		return
	}
	if len(chosen) == 0 {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Pilih minimal satu konten sebelum menyimpan.")
		return
	}

	runID, err := a.ingest.Start(r.Context(), chosen)
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
		a.internalError(w, r, err, "Gagal mengantrekan konten pilihan.")
		return
	}

	a.log.Info("admin: konten dimasukkan ke antrean",
		"providers", selectionProviders(chosen),
		"series", len(chosen),
		"run_id", runID,
		"username", a.sessionUsername(r),
	)
	a.flashTo(r, "success", fmt.Sprintf("%d series dimasukkan ke antrean (run #%d)", len(chosen), runID))
	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// selectionProviders mengembalikan daftar provider unik pada satu pilihan,
// dipakai untuk log.
func selectionProviders(series []catalog.Series) []string {
	seen := make(map[string]struct{}, len(series))
	providers := make([]string, 0, len(series))
	for _, item := range series {
		provider := strings.TrimSpace(item.Provider)
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	return providers
}

// limitList mengembalikan maksimal n nilai pertama beserta penanda sisanya.
func limitList(values []string, n int) []string {
	if n <= 0 || len(values) <= n {
		return values
	}
	limited := make([]string, 0, n+1)
	limited = append(limited, values[:n]...)
	return append(limited, fmt.Sprintf("… (+%d lainnya)", len(values)-n))
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
// media.
//
// Karena destruktif, permintaan ditolak selama masih ada sync run non-terminal
// (is_active = 1) dan wajib menyertakan field confirm=PURGE. Tabel di luar
// storage dan katalog — providers, auth_users, devices, app_settings,
// audit_log — tidak pernah disentuh.
//
// Kegagalan menghapus folder menghentikan proses sebelum database diubah, agar
// baris generation tidak hilang sementara medianya masih tertinggal di disk.
// Langkah best-effort (menyapu direktori tak tercatat dan pembersihan katalog)
// hanya dicatat ke log dan tidak membatalkan sisa proses.
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

	// 8. Poster ikut terhapus bersama folder generation; tidak ada cache
	// gambar terpisah.

	a.log.Info("admin: semua batch dihapus",
		"generations", generations,
		"catalog_rows", catalogRows,
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
// MediaRooter dilewati, dan direktori lain di media root tidak disentuh karena
// namanya tidak memakai awalan generation.
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
