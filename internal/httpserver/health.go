package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Readiness melaporkan apakah server siap melayani playback.
//
// Nilai balik kedua dipakai sebagai keterangan: nama generation saat siap
// (mis. "gen-000042") atau alasan saat belum siap (mis. "no_active_generation",
// "generation_missing_on_disk", "db_unavailable").
type Readiness interface {
	Ready(ctx context.Context) (bool, string)
}

// healthBody adalah bentuk GET /health: sederhana dan tanpa envelope
// (docs/api-spec.md §11).
type healthBody struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// readyBody adalah bentuk GET /ready. Salah satu dari generation atau reason
// selalu kosong sehingga tidak ikut terkirim.
type readyBody struct {
	Status     string `json:"status"`
	Generation string `json:"generation,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// HealthHandler melayani GET /health. Endpoint ini selalu 200 selama proses
// hidup; uptime dihitung dari waktu handler dibuat memakai now, sehingga test
// bisa menyuntikkan jam tiruan.
func HealthHandler(version string, now func() time.Time) http.HandlerFunc {
	if now == nil {
		now = time.Now
	}
	start := now()
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := now().Sub(start)
		if uptime < 0 {
			uptime = 0
		}
		writeJSON(w, http.StatusOK, healthBody{
			Status:        "ok",
			Version:       version,
			UptimeSeconds: int64(uptime / time.Second),
		})
	}
}

// ReadyHandler melayani GET /ready: 200 bila siap, 503 bila belum, dengan
// keterangan dari Readiness.
func ReadyHandler(r Readiness) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r == nil {
			writeJSON(w, http.StatusServiceUnavailable, readyBody{
				Status: "not_ready",
				Reason: "db_unavailable",
			})
			return
		}
		ready, info := r.Ready(req.Context())
		if ready {
			writeJSON(w, http.StatusOK, readyBody{Status: "ready", Generation: info})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, readyBody{Status: "not_ready", Reason: info})
	}
}

// writeJSON mengirim v sebagai JSON dengan status yang diberikan. Dipakai
// endpoint operasional yang dikecualikan dari envelope.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
