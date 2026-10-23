// Package httpserver merakit lapisan HTTP server VodLocal: router chi, middleware
// autentikasi device dan sesi admin, endpoint operasional, serta handler media.
//
// Bentuk response mengikuti docs/standar-response.md: semua response JSON
// memakai envelope dari package response. Endpoint yang dikecualikan dokumen
// tersebut (docs/api-spec.md §11) — /media/*, /health, /ready —
// mengembalikan bytes mentah atau JSON sederhana tanpa envelope.
package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"vodlocal/internal/response"
)

// Config adalah seluruh dependensi NewRouter. Route hanya dipasang bila
// dependensinya tersedia, kecuali /health dan /ready yang selalu ada.
type Config struct {
	// Version dilaporkan oleh GET /health.
	Version string
	// Logger dipakai access log ringkas; nil berarti slog.Default().
	Logger *slog.Logger
	// Media melayani /media/{generation}/*; nil mematikan route.
	Media MediaResolver
	// Ready dilaporkan oleh GET /ready; nil selalu menjawab 503.
	Ready Readiness
	// Devices dipakai lapisan API (RequireDevice) untuk bearer token device.
	Devices DeviceAuthenticator
	// Sessions dipakai lapisan admin (RequireAdmin, CSRF).
	Sessions *SessionManager
	// API dipasang di bawah /api/v1 dengan prefix yang dipotong; nil berarti
	// API tidak dipasang.
	API http.Handler
	// Register dipanggil setelah route bawaan dipasang, untuk menambahkan
	// route lapisan atas seperti panel admin. Nil berarti tidak ada.
	Register func(r chi.Router)
	// Now adalah sumber waktu untuk /health; nil berarti time.Now.
	Now func() time.Time
}

// NewRouter membangun handler HTTP utama.
//
// Middleware global: chi RequestID, RealIP, Recoverer, dan access log slog.
// Route media sengaja tidak memakai middleware.Timeout maupun
// middleware.Compress agar transfer besar dengan Range tetap murni bytes
// (docs/api-spec.md §13).
func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(withRequestIDHeader)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(accessLog(cfg.Logger))

	r.Get("/health", HealthHandler(cfg.Version, cfg.Now))
	r.Get("/ready", ReadyHandler(cfg.Ready))

	if cfg.Media != nil {
		r.Get("/media/{generation}/*", MediaHandler(cfg.Media).ServeHTTP)
	}

	if cfg.API != nil {
		r.Mount("/api/v1", http.StripPrefix("/api/v1", cfg.API))
	}

	if cfg.Register != nil {
		cfg.Register(r)
	}

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.Error(w, response.CodeNotFound, nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.Error(w, response.CodeMethodNotAllowed, nil)
	})
	return r
}

// withRequestIDHeader menyalin request ID yang dibuat middleware.RequestID ke
// header response X-Request-ID, sehingga klien bisa mengorelasikan permintaan
// dengan log (docs/standar-response.md §10).
func withRequestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(middleware.RequestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}

// accessLog mencatat satu baris ringkas per request: method, path, status,
// durasi, dan request_id. Level mengikuti kelas status (4xx Warn, 5xx Error).
// Header Authorization tidak pernah dicatat.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			args := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", durationMillis(time.Since(start)),
				"request_id", middleware.GetReqID(r.Context()),
			}
			switch {
			case status >= http.StatusInternalServerError:
				logger.Error("http request", args...)
			case status >= http.StatusBadRequest:
				logger.Warn("http request", args...)
			default:
				logger.Info("http request", args...)
			}
		})
	}
}

// durationMillis mengubah durasi ke milidetik pecahan dengan presisi mikrodetik
// agar request cepat tidak tercatat sebagai 0 ms.
func durationMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
