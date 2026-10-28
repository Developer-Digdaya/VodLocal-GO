// Package api memasang lapisan HTTP /api/v1 untuk aplikasi tablet bus:
// registrasi dan autentikasi device, katalog, pencarian, playback HLS, progress
// tonton, serta statistik dashboard admin.
//
// Seluruh handler JSON memakai envelope dari package response sehingga kode
// aplikasi (bukan status HTTP) adalah kebenaran yang dibaca klien
// (docs/standar-response.md). Satu-satunya pengecualian di paket ini adalah
// master playlist HLS (/playables/{id}/master.m3u8) yang harus tetap berupa
// playlist valid (docs/api-spec.md 11).
//
// Router di sini dipasang di bawah prefix /api/v1 dengan prefix yang sudah
// dipotong (lihat httpserver.Config.API), sehingga seluruh route ditulis tanpa
// prefix tersebut.
package api

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

// Config adalah seluruh dependensi router API.
type Config struct {
	// Read adalah pool baca (Store.Read) untuk seluruh query katalog.
	Read *sql.DB
	// Write adalah pool tulis (Store.Write) untuk registrasi device, heartbeat,
	// progress, dan pembaruan last_seen_at.
	Write *sql.DB
	// Logger dipakai mencatat kesalahan internal; nil berarti slog.Default().
	Logger *slog.Logger
	// AdminOnly adalah middleware sesi admin (httpserver.RequireAdmin). Bila
	// nil, route /admin/* tidak dipasang sama sekali.
	AdminOnly func(http.Handler) http.Handler
	// Now adalah sumber waktu; nil berarti time.Now.
	Now func() time.Time
}

// server memegang dependensi yang dipakai seluruh handler.
type server struct {
	read  *sql.DB
	write *sql.DB
	log   *slog.Logger
	now   func() time.Time
}

// NewRouter merakit router chi untuk /api/v1.
func NewRouter(cfg Config) http.Handler {
	s := &server{
		read:  cfg.Read,
		write: cfg.Write,
		log:   cfg.Logger,
		now:   cfg.Now,
	}
	if s.read == nil {
		s.read = cfg.Write
	}
	if s.write == nil {
		s.write = cfg.Read
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}

	// Authenticator memakai pool tulis karena setiap request yang valid
	// memperbarui devices.last_seen_at (api-spec 2).
	deviceAuth := NewAuthenticator(s.write, s.now)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(noStore)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.Error(w, response.CodeNotFound, nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.Error(w, response.CodeMethodNotAllowed, nil)
	})

	// Publik.
	r.Post("/devices/register", s.handleDeviceRegister)

	// Butuh token device.
	r.Group(func(pr chi.Router) {
		pr.Use(httpserver.RequireDevice(deviceAuth))

		pr.Get("/devices/me", s.handleDeviceMe)
		pr.Post("/devices/me/heartbeat", s.handleDeviceHeartbeat)

		pr.Get("/search", s.handleSearch)
		pr.Get("/titles", s.handleTitles)
		pr.Get("/titles/{id}", s.handleTitleDetail)
		pr.Get("/titles/{id}/episodes", s.handleTitleEpisodes)

		pr.Get("/playables/{id}", s.handlePlayable)
		pr.Get("/playables/{id}/master.m3u8", s.handleMasterPlaylist)

		pr.Get("/progress", s.handleProgressList)
		pr.Get("/progress/{playable_id}", s.handleProgressItem)
		pr.Post("/progress", s.handleProgressSave)
	})

	// Butuh sesi admin. Tanpa middleware, route sengaja tidak terpasang.
	if cfg.AdminOnly != nil {
		r.With(cfg.AdminOnly).Get("/admin/overview", s.handleAdminOverview)
	}

	return r
}

// pathParam membaca path parameter chi.
func pathParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

// noStore menambahkan Cache-Control: no-store pada seluruh response API
// (api-spec 13). Handler master playlist menimpanya dengan no-cache.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
