package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"vodlocal/internal/response"
)

const (
	// defaultPerPage dan maxPerPage mengikuti docs/api-spec.md §4.
	defaultPerPage = 50
	maxPerPage     = 200

	// maxBodyBytes adalah batas body JSON (§1: 64 KB).
	maxBodyBytes = 64 << 10

	// timeLayout adalah format waktu ISO-8601 UTC yang dipakai seluruh DB.
	timeLayout = "2006-01-02T15:04:05Z"

	// minSearchLength dan maxSearchLength mengikuti §4.1: di bawah minimum
	// ditolak, di atas maksimum dipotong tanpa error.
	minSearchLength = 2
	maxSearchLength = 100
)

// pageMeta adalah bentuk data.meta untuk endpoint list (§4, R12).
type pageMeta struct {
	Page       int64 `json:"page"`
	PerPage    int64 `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int64 `json:"total_pages"`
}

// pagination menyimpan halaman yang sudah divalidasi.
type pagination struct {
	Page    int64
	PerPage int64
}

// parsePagination membaca page dan per_page. Nilai yang tidak valid dicatat ke
// fields agar handler bisa menjawab 422/1003; page < 1 dijepit ke 1 karena
// dokumen tidak memberi batas bawah (hanya "mulai dari 1").
func parsePagination(q url.Values, fields map[string]string) pagination {
	p := pagination{Page: 1, PerPage: defaultPerPage}

	if raw := strings.TrimSpace(q.Get("page")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			fields["page"] = "harus berupa angka"
		case n < 1:
			p.Page = 1
		default:
			p.Page = n
		}
	}

	if raw := strings.TrimSpace(q.Get("per_page")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			fields["per_page"] = "harus berupa angka"
		case n < 1:
			fields["per_page"] = "minimal 1"
		case n > maxPerPage:
			fields["per_page"] = "maksimal 200"
		default:
			p.PerPage = n
		}
	}

	return p
}

// offset menghitung OFFSET untuk halaman saat ini.
func (p pagination) offset() int64 {
	return (p.Page - 1) * p.PerPage
}

// meta menyusun pageMeta untuk total baris yang diketahui.
func (p pagination) meta(total int64) pageMeta {
	pages := int64(0)
	if total > 0 {
		pages = (total + p.PerPage - 1) / p.PerPage
	}
	return pageMeta{Page: p.Page, PerPage: p.PerPage, Total: total, TotalPages: pages}
}

// pathID membaca path parameter bernama key sebagai ID positif.
func pathID(r *http.Request, key string) (int64, bool) {
	raw := strings.TrimSpace(pathParam(r, key))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// validationFailed mengirim 422/1003 bila ada field yang bermasalah.
func (s *server) validationFailed(w http.ResponseWriter, fields map[string]string) bool {
	if len(fields) == 0 {
		return false
	}
	response.ValidationErrors(w, fields)
	return true
}

// detailsFromFields mengubah map field→pesan menjadi []Detail deterministik
// (R6). Dipakai saat kode error bukan 1003, mis. 8001 PROGRESS_INVALID.
func detailsFromFields(fields map[string]string) []response.Detail {
	details := make([]response.Detail, 0, len(fields))
	for field, message := range fields {
		details = append(details, response.Detail{Field: field, Message: message})
	}
	sort.Slice(details, func(i, j int) bool { return details[i].Field < details[j].Field })
	return details
}

// internalError mencatat kesalahan asli ke log dan menjawab 500/1000 tanpa
// membocorkan detail internal ke klien (R5).
func (s *server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("api: kesalahan internal",
		"method", r.Method,
		"path", r.URL.Path,
		"err", err,
	)
	response.Error(w, response.CodeInternalError, nil)
}

// decodeJSON membaca body JSON dengan batas 64 KB. Nilai balik false berarti
// response error sudah dikirim.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	err := json.NewDecoder(body).Decode(dst)
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		response.Error(w, response.CodePayloadTooLarge, nil)
		return false
	}
	response.Error(w, response.CodeBadRequest, nil)
	return false
}

// normalizeSearchText menerapkan aturan §4.1: trim, spasi berulang menjadi
// satu, dan pemotongan ke 100 karakter.
func normalizeSearchText(s string) string {
	normalized := strings.Join(strings.Fields(s), " ")
	if runes := []rune(normalized); len(runes) > maxSearchLength {
		normalized = string(runes[:maxSearchLength])
	}
	return normalized
}

// likeEscaper meng-escape karakter wildcard LIKE supaya input user tidak
// pernah menjadi pola pencarian.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains menyusun pola "mengandung" dengan ESCAPE '\'.
func likeContains(text string) string {
	return "%" + likeEscaper.Replace(text) + "%"
}

// likePrefix menyusun pola "diawali" dengan ESCAPE '\'.
func likePrefix(text string) string {
	return likeEscaper.Replace(text) + "%"
}

// isoTime mengubah nilai waktu TEXT dari DB menjadi ISO-8601 UTC. Nilai kosong
// menjadi null dan nilai yang tidak bisa diparse dikembalikan apa adanya
// sehingga data lama tidak hilang.
func isoTime(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{
		timeLayout,
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(timeLayout)
		}
	}
	return s
}

// nullText menampilkan string kosong sebagai null (§1: field kosong = null).
func nullText(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// nullString menampilkan sql.NullString sebagai string atau null.
func nullString(s sql.NullString) any {
	if !s.Valid || strings.TrimSpace(s.String) == "" {
		return nil
	}
	return s.String
}

// nullInt64 menampilkan sql.NullInt64 sebagai angka atau null.
func nullInt64(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// nullIfBlank mengubah string kosong menjadi NULL saat dipakai sebagai
// argumen INSERT.
func nullIfBlank(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// mediaURL menyusun URL media di dalam sebuah generation (§8.1).
func mediaURL(generation, path string) string {
	return "/media/" + generation + "/" + strings.TrimPrefix(strings.ReplaceAll(path, `\`, "/"), "/")
}

// clock mengembalikan waktu sekarang menurut sumber waktu yang disuntikkan.
func (s *server) clock() time.Time {
	return s.now().UTC()
}

// timestamp mengembalikan waktu sekarang dalam format TEXT DB.
func (s *server) timestamp() string {
	return s.clock().Format(timeLayout)
}
