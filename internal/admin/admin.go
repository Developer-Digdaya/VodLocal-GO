// Package admin menyediakan panel administrasi server-rendered untuk sistem
// hiburan bus.
//
// Seluruh halaman dirender dengan html/template + Pico CSS yang di-embed ke
// binary (tanpa CDN) dan mengikuti docs/api-spec.md §10: autentikasi lewat
// cookie sesi httpserver.SessionManager, token CSRF double-submit pada setiap
// POST, dan Cache-Control: no-store pada semua halaman.
//
// Paket ini hanya membaca/menulis tabel yang sudah ada (auth_users,
// app_settings, devices, generations, sync_runs, generation_items) dan memakai
// SyncRunner serta MediaManager sebagai antarmuka ke lapisan lain, sehingga
// panel tidak bergantung pada implementasi konkret syncjob atau mediafs.
package admin

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/store"
	"vodlocal/internal/syncjob"
	"vodlocal/internal/transfer"
)

//go:embed assets/*
var adminAssets embed.FS

const (
	// timeLayout adalah format waktu ISO-8601 UTC yang dipakai seluruh tabel.
	timeLayout = "2006-01-02T15:04:05Z"

	// onlineWindow adalah batas sebuah perangkat dianggap online (§10.4).
	onlineWindow = 5 * time.Minute

	// defaultPerPage dan maxPerPage mengikuti docs/api-spec.md §4.
	defaultPerPage = 10
	maxPerPage     = 200

	// maxQueryLength adalah batas panjang pencarian di panel admin (§4.1).
	maxQueryLength = 100
)

// SyncRunner adalah sebagian antarmuka internal/syncjob.Syncer yang dipakai
// panel admin: memulai sinkronisasi penuh dan membatalkan satu run.
type SyncRunner interface {
	// Run menjalankan satu sinkronisasi penuh dengan sumber pemicu trigger
	// ("manual", "scheduled", "boot").
	Run(ctx context.Context, trigger string) (syncjob.Result, error)
	// Cancel menghentikan run runID.
	Cancel(runID int64) error
}

// MediaManager adalah sebagian antarmuka internal/mediafs.Manager yang dipakai
// panel admin untuk menghapus direktori sebuah generation.
type MediaManager interface {
	// Remove menghapus direktori generation berlabel label.
	Remove(label string) error
}

// MediaRooter adalah antarmuka opsional yang dipakai aksi "Remove all batch"
// untuk menemukan akar direktori media.
//
// mediafs.Manager memenuhinya. Implementasi MediaManager lain yang tidak
// menyediakan Root() tetap sah: penghapusan per generation berjalan seperti
// biasa, hanya sapuan direktori sisa (gen-*/.building-*) yang dilewati.
type MediaRooter interface {
	// Root mengembalikan akar direktori media tempat direktori generation dan
	// staging build berada.
	Root() string
}

// UsageFunc melaporkan pemakaian ruang penyimpanan media: byte terpakai dan
// kapasitas total yang tersedia.
//
// Hook ini opsional (bukan bagian API wajib). Bila nil, dashboard tetap
// menampilkan total byte seluruh generation tetapi tidak menghitung persentase
// maupun peringatan storage, karena paket ini tidak memegang path media root.
type UsageFunc func() (usedBytes, totalBytes int64, err error)

// Config adalah seluruh dependensi panel admin.
type Config struct {
	// Store adalah database aplikasi; wajib.
	Store *store.Store
	// Syncer menjalankan dan membatalkan sinkronisasi; nil berarti halaman sync
	// menolak memulai sinkronisasi.
	Syncer SyncRunner
	// Media menghapus direktori generation; nil berarti penghapusan hanya
	// menghapus baris database. Implementasi yang juga memenuhi MediaRooter
	// (seperti mediafs.Manager) memungkinkan aksi "Remove all batch" menyapu
	// direktori media yang tidak tercatat di database.
	Media MediaManager
	// Sessions mengelola cookie sesi admin dan token CSRF; wajib.
	Sessions *httpserver.SessionManager
	// Logger menerima log panel; nil berarti slog.Default().
	Logger *slog.Logger
	// Now memasok waktu sekarang; nil berarti time.Now.
	Now func() time.Time
	// Version ditampilkan di footer setiap halaman.
	Version string
	// StorageUsage opsional; lihat UsageFunc.
	StorageUsage UsageFunc
	// Lister membaca daftar series provider untuk modal "List Content"; nil
	// berarti rute /content tidak dipasang.
	Lister SeriesLister
	// Ingest mengantrekan series terpilih dari modal "List Content"; nil berarti
	// rute /content tidak dipasang.
	Ingest IngestStarter
	// Transfer menangani export/import paket HDD; nil berarti fitur transfer
	// tidak dipasang.
	Transfer *transfer.Service
}

// Admin adalah panel administrasi. Nilainya aman dipakai banyak goroutine.
type Admin struct {
	store     *store.Store
	read      *sql.DB
	write     *sql.DB
	syncer    SyncRunner
	media     MediaManager
	sessions  *httpserver.SessionManager
	log       *slog.Logger
	now       func() time.Time
	version   string
	usage     UsageFunc
	lister    SeriesLister
	ingest    IngestStarter
	transfer  *transfer.Service
	templates map[string]*template.Template

	// content menyimpan hasil ListSeries terakhir agar slug yang dipilih pada
	// modal "List Content" bisa diresolusi saat formulir dikirim.
	content *contentCache

	// mu melindungi flashes, peta pesan sekali-tampil per nama pengguna.
	mu      sync.Mutex
	flashes map[string]flash
}

// flash adalah pesan sekali-tampil setelah redirect POST (§10).
type flash struct {
	Kind string
	Text string
}

// New menyiapkan panel admin dari cfg.
func New(cfg Config) (*Admin, error) {
	if cfg.Store == nil {
		return nil, errors.New("admin: Config.Store wajib diisi")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("admin: Config.Sessions wajib diisi")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := cfg.Now
	if clock == nil {
		clock = time.Now
	}

	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	return &Admin{
		store:     cfg.Store,
		read:      cfg.Store.Read(),
		write:     cfg.Store.Write(),
		syncer:    cfg.Syncer,
		media:     cfg.Media,
		sessions:  cfg.Sessions,
		log:       logger,
		now:       clock,
		version:   cfg.Version,
		usage:     cfg.StorageUsage,
		lister:    cfg.Lister,
		ingest:    cfg.Ingest,
		transfer:  cfg.Transfer,
		templates: templates,
		content:   newContentCache(),
		flashes:   make(map[string]flash),
	}, nil
}

// handlePicoCSS menyajikan CSS Pico yang di-embed ke binary.
func (a *Admin) handlePicoCSS(w http.ResponseWriter, r *http.Request) {
	data, err := adminAssets.ReadFile("assets/pico.min.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// Mount memasang seluruh route panel admin pada r.
//
// Route publik hanya GET/POST /login; sisanya berada di bawah middleware
// RequireAdmin, dan setiap POST memakai middleware CSRF.
func (a *Admin) Mount(r chi.Router) {
	if r == nil {
		return
	}
	csrf := httpserver.CSRF(a.sessions)

	// Aset CSS Pico di-vendor ke binary agar panel tetap berfungsi tanpa
	// internet. Route ini publik dan tidak memerlukan sesi admin.
	r.Get("/assets/pico.min.css", a.handlePicoCSS)

	r.Get("/login", a.handleLoginForm)
	r.With(csrf).Post("/login", a.handleLoginSubmit)

	r.Group(func(pr chi.Router) {
		pr.Use(httpserver.RequireAdmin(a.sessions))

		pr.With(csrf).Post("/logout", a.handleLogout)
		pr.Get("/", a.handleDashboard)

		pr.Get("/sync", a.handleSyncList)
		pr.With(csrf).Post("/sync", a.handleSyncStart)
		pr.Get("/sync/{id}", a.handleSyncDetail)
		pr.Get("/sync/{id}/status", a.handleSyncStatus)
		pr.Get("/sync/{id}/items", a.handleSyncItems)
		pr.With(csrf).Post("/sync/{id}/cancel", a.handleSyncCancel)

		// "Remove all batch" tidak bergantung pada provider, jadi route-nya
		// selalu dipasang walaupun Lister/Ingest tidak tersedia.
		pr.With(csrf).Post("/content/purge", a.handleContentPurge)

		// Modal "List Content" hanya tersedia bila lister dan ingest sudah
		// dipasang; tanpa keduanya tombolnya tidak berguna sehingga route tidak
		// dipasang sama sekali.
		if a.lister != nil && a.ingest != nil {
			pr.With(csrf).Get("/content", a.handleContent)
			pr.With(csrf).Get("/content/short", a.handleContentShort)
			pr.With(csrf).Post("/content/select", a.handleContentSelect)
		}

		pr.Get("/devices", a.handleDevices)
		pr.With(csrf).Post("/devices/{id}/disable", a.handleDeviceDisable)
		pr.With(csrf).Post("/devices/{id}/enable", a.handleDeviceEnable)
		pr.With(csrf).Post("/devices/{id}/rotate-token", a.handleDeviceRotate)

		pr.Get("/storage", a.handleStorage)
		pr.With(csrf).Post("/storage/items/{id}/delete", a.handleStorageItemDelete)
		pr.With(csrf).Post("/storage/generations/{id}/delete", a.handleStorageDelete)

		if a.transfer != nil {
			pr.With(csrf).Get("/transfer/drives", a.handleTransferDrives)
			pr.With(csrf).Get("/transfer/packages", a.handleTransferPackages)
			pr.With(csrf).Get("/transfer/batch-items", a.handleTransferBatchItems)
			pr.With(csrf).Get("/transfer/package-poster", a.handleTransferPackagePoster)
			pr.With(csrf).Get("/transfer/local-content", a.handleTransferLocalContent)
			pr.With(csrf).Post("/transfer/export", a.handleTransferExport)
			pr.With(csrf).Post("/transfer/import", a.handleTransferImport)
		}

		pr.Get("/settings", a.handleSettingsForm)
		pr.With(csrf).Post("/settings", a.handleSettingsSave)
	})
}

// base adalah data yang dipakai layout pada setiap halaman.
type base struct {
	Title     string
	Nav       string
	CSRF      string
	User      string
	Role      string
	Version   string
	Flash     string
	FlashKind string
	Error     string
	Now       string
}

// base menyiapkan data layout untuk satu request dan mengambil flash yang
// tersisa untuk pengguna yang sedang login.
func (a *Admin) base(w http.ResponseWriter, r *http.Request, title, nav string) base {
	b := base{
		Title:   title,
		Nav:     nav,
		CSRF:    a.csrfToken(w, r),
		Version: a.version,
		Now:     a.nowUTC().In(wibLocation).Format("02 Jan 2006 15:04 WIB"),
	}
	if session, ok := a.sessions.Get(r); ok {
		b.User = session.Username
		b.Role = session.Role
		if f, ok := a.takeFlash(session.Username); ok {
			b.Flash = f.Text
			b.FlashKind = f.Kind
		}
	}
	return b
}

// csrfToken memakai token dari cookie bila ada, dan membuat cookie baru lewat
// SessionManager bila belum ada (syarat §10.1).
func (a *Admin) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if r != nil {
		if cookie, err := r.Cookie(httpserver.CSRFCookieName); err == nil && cookie.Value != "" {
			return cookie.Value
		}
	}
	return a.sessions.EnsureCSRFCookie(w, r)
}

// setFlash menyimpan pesan untuk ditampilkan pada halaman berikutnya.
func (a *Admin) setFlash(username, kind, text string) {
	if username == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.flashes == nil {
		a.flashes = make(map[string]flash)
	}
	a.flashes[username] = flash{Kind: kind, Text: text}
}

// takeFlash mengambil (dan menghapus) pesan untuk username.
func (a *Admin) takeFlash(username string) (flash, bool) {
	if username == "" {
		return flash{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.flashes[username]
	if ok {
		delete(a.flashes, username)
	}
	return f, ok
}

// renderPage merender halaman utuh (layout + content) ke w.
func (a *Admin) renderPage(w http.ResponseWriter, r *http.Request, page string, data any, status int) {
	tpl, ok := a.templates[page]
	if !ok {
		a.log.Error("admin: template tidak dikenal", "template", page)
		http.Error(w, "template tidak dikenal", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		a.log.Error("admin: render halaman gagal", "template", page, "path", requestPath(r), "err", err)
		http.Error(w, "Gagal merender halaman", http.StatusInternalServerError)
		return
	}

	writeHTML(w, status, buf.Bytes())
}

// renderPartial merender satu fragmen HTML tanpa layout.
func (a *Admin) renderPartial(w http.ResponseWriter, name string, data any) {
	tpl, ok := a.templates[name]
	if !ok {
		a.log.Error("admin: template parsial tidak dikenal", "template", name)
		http.Error(w, "template tidak dikenal", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("admin: render parsial gagal", "template", name, "err", err)
		http.Error(w, "Gagal merender halaman", http.StatusInternalServerError)
		return
	}

	writeHTML(w, http.StatusOK, buf.Bytes())
}

// writeHTML mengirim body HTML dengan header yang seragam.
func writeHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorData adalah data halaman error.
type errorData struct {
	base
	Status  int
	Message string
}

// renderError menampilkan halaman error HTML dengan status HTTP tertentu.
func (a *Admin) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	a.renderPage(w, r, "error", errorData{
		base:    a.base(w, r, "Kesalahan", ""),
		Status:  status,
		Message: message,
	}, status)
}

// internalError mencatat kesalahan asli ke log lalu menampilkan halaman 500
// tanpa membocorkan detail internal ke pengguna (standar-response R5).
func (a *Admin) internalError(w http.ResponseWriter, r *http.Request, err error, message string) {
	a.log.Error("admin: kesalahan internal",
		"method", r.Method,
		"path", requestPath(r),
		"err", err,
	)
	a.renderError(w, r, http.StatusInternalServerError, message)
}

// requestPath mengembalikan path request tanpa nil-panic.
func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

// sessionUsername mengembalikan nama pengguna dari sesi, atau "" bila tidak ada.
func (a *Admin) sessionUsername(r *http.Request) string {
	if session, ok := a.sessions.Get(r); ok {
		return session.Username
	}
	return ""
}

// flashTo menyimpan pesan untuk pengguna yang sedang login.
func (a *Admin) flashTo(r *http.Request, kind, text string) {
	a.setFlash(a.sessionUsername(r), kind, text)
}

// nowUTC mengembalikan waktu sekarang dalam UTC.
func (a *Admin) nowUTC() time.Time {
	if a.now == nil {
		return time.Now().UTC()
	}
	return a.now().UTC()
}

// stamp mengembalikan waktu sekarang dalam format kolom TEXT database.
func (a *Admin) stamp() string {
	return a.nowUTC().Format(timeLayout)
}

// parseTime mem-parse nilai waktu kolom TEXT database.
func parseTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{timeLayout, time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// pathID membaca path parameter chi sebagai ID positif.
func pathID(r *http.Request, key string) (int64, bool) {
	raw := strings.TrimSpace(chi.URLParam(r, key))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// wantsJSON melaporkan apakah klien mengharapkan body JSON (§10.3).
func wantsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

// normalizeQuery menerapkan normalisasi pencarian §4.1: trim, spasi berulang
// menjadi satu, dan pemotongan ke 100 karakter. Tidak ada batas minimum karena
// panel admin dipakai untuk debugging (§10.9).
func normalizeQuery(s string) string {
	normalized := strings.Join(strings.Fields(s), " ")
	if runes := []rune(normalized); len(runes) > maxQueryLength {
		normalized = string(runes[:maxQueryLength])
	}
	return normalized
}

// likeEscaper meng-escape wildcard LIKE agar input pengguna tidak menjadi pola.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains menyusun pola "mengandung" dengan ESCAPE '\'.
func likeContains(text string) string {
	return "%" + likeEscaper.Replace(text) + "%"
}

// pageInfo merangkum satu halaman daftar: nomor halaman yang sudah dijepit ke
// rentang valid, ukuran halaman, total baris, dan rentang baris yang tampil.
type pageInfo struct {
	Page       int
	PerPage    int
	Total      int64
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int
	From       int
	To         int
}

// paginationMeta adalah bentuk pagination pada respons JSON internal admin.
type paginationMeta struct {
	Page     int   `json:"page"`
	PerPage  int   `json:"per_page"`
	Total    int64 `json:"total"`
	LastPage int   `json:"last_page"`
}

// offset mengembalikan offset SQL untuk halaman ini.
func (p pageInfo) offset() int {
	if p.Page < 1 {
		return 0
	}
	return (p.Page - 1) * p.PerPage
}

// newPageInfo menghitung metadata halaman dan menjepit page ke 1..TotalPages.
// Pemanggil harus memakai Page hasilnya untuk LIMIT/OFFSET, bukan page mentah,
// supaya permintaan ?page=999 tidak menampilkan halaman kosong.
func newPageInfo(page, perPage int, total int64) pageInfo {
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	if page < 1 {
		page = 1
	}

	info := pageInfo{Page: page, PerPage: perPage, Total: total}
	if total <= 0 {
		return info
	}

	info.TotalPages = int((total + int64(perPage) - 1) / int64(perPage))
	if info.Page > info.TotalPages {
		info.Page = info.TotalPages
	}
	info.HasPrev = info.Page > 1
	info.HasNext = info.Page < info.TotalPages
	info.PrevPage = info.Page - 1
	if info.PrevPage < 1 {
		info.PrevPage = 1
	}
	info.NextPage = info.Page + 1
	info.From = (info.Page-1)*perPage + 1
	info.To = info.Page * perPage
	if info.To > int(total) {
		info.To = int(total)
	}
	return info
}

// parsePage membaca page dan per_page dengan batas yang aman.
func parsePage(values map[string][]string) (page, perPage int) {
	return parsePageNamed(values, "page", "per_page", defaultPerPage)
}

// parsePageNamed membaca pasangan kunci page/per_page tertentu. Dipakai halaman
// yang punya lebih dari satu tabel dengan pagination independen (mis. storage:
// page untuk konten dan gen_page untuk generation).
func parsePageNamed(values map[string][]string, pageKey, perPageKey string, defaultPerPageValue int) (page, perPage int) {
	page, perPage = 1, defaultPerPageValue
	if perPage < 1 {
		perPage = defaultPerPage
	}
	raw := strings.TrimSpace(queryGet(values, pageKey))
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		page = n
	}
	raw = strings.TrimSpace(queryGet(values, perPageKey))
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		perPage = n
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// queryGet membaca nilai pertama dari url.Values.
func queryGet(values map[string][]string, key string) string {
	if values == nil {
		return ""
	}
	if v, ok := values[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// progressPercent menghitung persentase renditions_ready/total_renditions.
func progressPercent(ready, total int64) int {
	if total <= 0 {
		return 0
	}
	percent := int(ready * 100 / total)
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

// humanBytes memformat jumlah byte agar mudah dibaca.
func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(n)
	index := -1
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

// wibLocation adalah zona waktu tampilan panel. Memakai FixedZone agar tidak
// bergantung pada database zona waktu di host.
var wibLocation = time.FixedZone("WIB", 7*60*60)

// humanTime menampilkan nilai waktu kolom TEXT secara ringkas dalam WIB.
func humanTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—"
	}
	if t, ok := parseTime(value); ok {
		return t.In(wibLocation).Format("02 Jan 2006 15:04 WIB")
	}
	return value
}

// templateFuncs adalah fungsi bantu yang tersedia di seluruh template.
var templateFuncs = template.FuncMap{
	"bytes": humanBytes,
	"when":  humanTime,
	"pct":   progressPercent,
	"add":   func(a, b int) int { return a + b },
	"sub":   func(a, b int) int { return a - b },
}

//go:embed templates/*.html
var templateFS embed.FS

// pageTemplates adalah halaman yang memakai layout.
var pageTemplates = []string{
	"error",
	"login",
	"dashboard",
	"sync",
	"sync_detail",
	"devices",
	"storage",
	"settings",
}

// sharedTemplates ikut diparse pada setiap halaman agar layout bisa memanggil
// fragmen bersama.
var sharedTemplates = []string{
	"templates/layout.html",
	"templates/sync_status.html",
	"templates/generation_items.html",
}

// parseTemplates mem-parse setiap halaman bersama layout ke template terpisah,
// sehingga blok "content" antar-halaman tidak saling menimpa.
func parseTemplates() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(pageTemplates)+1)
	for _, page := range pageTemplates {
		patterns := append(append([]string{}, sharedTemplates...), "templates/"+page+".html")
		tpl, err := template.New(page).Funcs(templateFuncs).ParseFS(templateFS, patterns...)
		if err != nil {
			return nil, fmt.Errorf("admin: parse template %s: %w", page, err)
		}
		pages[page] = tpl
	}

	// Fragmen yang juga diminta sendiri lewat polling (tanpa layout).
	partial, err := template.New("partials").Funcs(templateFuncs).
		ParseFS(templateFS, "templates/sync_status.html", "templates/generation_items.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse template parsial: %w", err)
	}
	pages["sync_status"] = partial
	pages["generation_items"] = partial

	return pages, nil
}
