package admin

import (
	"net/http"

	"vodlocal"
)

// apidocsData adalah data halaman /api-docs.
type apidocsData struct {
	base
	// SpecBytes adalah ukuran openapi.json yang di-embed ke binary, dipakai
	// sebagai informasi kecil di kepala halaman.
	SpecBytes string
}

// handleAPIDocs melayani GET /api-docs: halaman penjelajah OpenAPI yang
// menampilkan daftar operasi dan panel "Coba" untuk menjalankan request ke
// origin panel itu sendiri.
//
// Halaman tidak butuh data dari database; seluruh isi spesifikasi diambil
// peramban dari /api-docs/spec.json supaya render HTML tetap ringan.
func (a *Admin) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	a.renderPage(w, r, "apidocs", apidocsData{
		base:      a.base(w, r, "Dokumentasi API", "apidocs"),
		SpecBytes: humanBytes(int64(len(vodlocal.OpenAPISpec))),
	}, http.StatusOK)
}

// handleAPIDocsSpec melayani GET /api-docs/spec.json: mengirim isi openapi.json
// apa adanya.
//
// Respons tidak di-cache (no-store) seperti seluruh respons panel, dan
// dinyatakan sebagai application/json tanpa charset agar peramban mem-parse
// langsung sebagai JSON.
func (a *Admin) handleAPIDocsSpec(w http.ResponseWriter, r *http.Request) {
	if len(vodlocal.OpenAPISpec) == 0 {
		http.Error(w, "spesifikasi OpenAPI tidak tersedia", http.StatusNotFound)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(vodlocal.OpenAPISpec)
}
