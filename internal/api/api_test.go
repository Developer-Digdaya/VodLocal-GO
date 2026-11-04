package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"vodlocal/internal/api"
	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

// TestDeviceRegister memastikan registrasi mengembalikan 201 + token sekali
// pakai, menyimpan token sebagai hash, menyimpan identitas perangkat apa
// adanya, dan menolak identitas duplikat dengan 409/2003 beserta data kursinya.
func TestDeviceRegister(t *testing.T) {
	f := newFixture(t, nil)

	env, rec := f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"note":        "Tablet Kursi 12",
		"seat_number": "12",
		"mac_address": "aa-bb-cc-dd-ee-12",
		"app_version": "1.0.3",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, mau 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.Code != response.CodeSuccess {
		t.Fatalf("code = %d, mau 0", env.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, mau application/json", ct)
	}

	var data struct {
		Device struct {
			ID         int64  `json:"id"`
			Note       string `json:"note"`
			SeatNumber string `json:"seat_number"`
			MACAddress string `json:"mac_address"`
		} `json:"device"`
		Token string `json:"token"`
		Reset bool   `json:"reset"`
	}
	f.decode(env, &data)

	if data.Device.ID < 1 {
		t.Fatalf("device.id = %d, mau > 0", data.Device.ID)
	}
	if data.Device.Note != "Tablet Kursi 12" || data.Device.SeatNumber != "12" {
		t.Fatalf("device = %+v, mau name/seat_number sesuai request", data.Device)
	}
	if data.Reset {
		t.Fatalf("reset = true pada registrasi baru, mau false")
	}
	if !strings.HasPrefix(data.Token, "mc_") {
		t.Fatalf("token = %q, mau berawalan mc_", data.Token)
	}
	// 32 byte acak base64url tanpa padding = 43 karakter.
	if len(data.Token) != len("mc_")+43 {
		t.Fatalf("panjang token = %d, mau 46", len(data.Token))
	}

	var stored string
	if err := f.db.QueryRow(`SELECT token_hash FROM devices WHERE id = ?`, data.Device.ID).Scan(&stored); err != nil {
		t.Fatalf("baca token_hash: %v", err)
	}
	if stored == data.Token {
		t.Fatalf("token mentah tersimpan di database")
	}
	if want := hashToken(data.Token); stored != want {
		t.Fatalf("token_hash = %q, mau SHA-256 hex %q", stored, want)
	}

	// Identitas disimpan apa adanya: tidak di-uppercase, pemisah tidak dibuang.
	var mac string
	if err := f.db.QueryRow(`SELECT mac_address FROM devices WHERE id = ?`, data.Device.ID).Scan(&mac); err != nil {
		t.Fatalf("baca mac_address: %v", err)
	}
	if mac != "aa-bb-cc-dd-ee-12" {
		t.Fatalf("mac_address = %q, mau apa adanya: aa-bb-cc-dd-ee-12", mac)
	}
	if data.Device.MACAddress != "aa-bb-cc-dd-ee-12" {
		t.Fatalf("device.mac_address = %q, mau aa-bb-cc-dd-ee-12", data.Device.MACAddress)
	}

	// Identitas yang sama (beda kapitalisasi) harus ditolak beserta data kursi.
	env, rec = f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"mac_address": "AA-BB-CC-DD-EE-12",
		"seat_number": "13",
		"note":        "Tablet Lain",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status duplikat = %d, mau 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.Code != response.CodeDeviceAlreadyRegistered {
		t.Fatalf("code duplikat = %d, mau %d", env.Code, response.CodeDeviceAlreadyRegistered)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors = %+v, mau kosong", env.Errors)
	}
	var conflict struct {
		DeviceID   int64  `json:"device_id"`
		SeatNumber string `json:"seat_number"`
		Note       string `json:"note"`
	}
	f.decode(env, &conflict)
	if conflict.DeviceID != data.Device.ID {
		t.Fatalf("data.device_id = %d, mau %d", conflict.DeviceID, data.Device.ID)
	}
	if conflict.SeatNumber != "12" || conflict.Note != "Tablet Kursi 12" {
		t.Fatalf("data = %+v, mau kursi 12 milik Tablet Kursi 12", conflict)
	}

	// Validasi field: mac_address wajib diisi.
	env, rec = f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"seat_number": "14",
		"mac_address": "   ",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status MAC kosong = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.Code != response.CodeValidationError {
		t.Fatalf("code MAC kosong = %d, mau %d", env.Code, response.CodeValidationError)
	}
	var macMissing bool
	for _, detail := range env.Errors {
		if detail.Field == "body.mac_address" {
			macMissing = true
		}
	}
	if !macMissing {
		t.Fatalf("errors = %+v, mau detail body.mac_address", env.Errors)
	}

	// Validasi field: seat_number wajib diisi.
	env, rec = f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"mac_address": "android-tanpa-kursi",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status kursi kosong = %d, mau 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var seatMissing bool
	for _, detail := range env.Errors {
		if detail.Field == "body.seat_number" {
			seatMissing = true
		}
	}
	if !seatMissing {
		t.Fatalf("errors = %+v, mau detail body.seat_number", env.Errors)
	}
}

// TestDeviceAuthentication memeriksa /devices/me dengan token valid, tanpa
// header, token asing, dan device nonaktif (tugas uji #2).
func TestDeviceAuthentication(t *testing.T) {
	f := newFixture(t, nil)
	token, id := f.registerDevice("Tablet Kursi 1", "AA:BB:CC:DD:EE:01")

	env, rec := f.do(http.MethodGet, "/devices/me", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		ID         int64   `json:"id"`
		Note       string  `json:"note"`
		SeatNumber string  `json:"seat_number"`
		IsActive   bool    `json:"is_active"`
		LastSeenAt *string `json:"last_seen_at"`
		CreatedAt  *string `json:"created_at"`
	}
	f.decode(env, &data)
	if data.ID != id || data.Note != "Tablet Kursi 1" || !data.IsActive {
		t.Fatalf("data = %+v, mau device %d aktif", data, id)
	}
	if data.LastSeenAt == nil || *data.LastSeenAt != "2026-10-20T11:30:00Z" {
		t.Fatalf("last_seen_at = %v, mau 2026-10-20T11:30:00Z", data.LastSeenAt)
	}
	if data.CreatedAt == nil {
		t.Fatalf("created_at null, mau terisi")
	}

	// Middleware menulis last_seen_at ke database, bukan hanya ke response.
	var stored *string
	if err := f.db.QueryRow(`SELECT last_seen_at FROM devices WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("baca last_seen_at: %v", err)
	}
	if stored == nil || *stored != "2026-10-20T11:30:00Z" {
		t.Fatalf("last_seen_at di DB = %v, mau 2026-10-20T11:30:00Z", stored)
	}

	// Tanpa header Authorization.
	env, rec = f.do(http.MethodGet, "/devices/me", "", nil)
	if rec.Code != http.StatusUnauthorized || env.Code != response.CodeUnauthorized {
		t.Fatalf("tanpa token: status/code = %d/%d, mau 401/2000 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	// Token tidak dikenal.
	env, rec = f.do(http.MethodGet, "/devices/me", "mc_tidak-ada", nil)
	if rec.Code != http.StatusUnauthorized || env.Code != response.CodeUnauthorized {
		t.Fatalf("token asing: status/code = %d/%d, mau 401/2000", rec.Code, env.Code)
	}

	// Device dinonaktifkan admin.
	f.exec(`UPDATE devices SET is_active = 0 WHERE id = ?`, id)
	env, rec = f.do(http.MethodGet, "/devices/me", token, nil)
	if rec.Code != http.StatusForbidden || env.Code != response.CodeDeviceDisabled {
		t.Fatalf("device nonaktif: status/code = %d/%d, mau 403/2004 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	// Heartbeat device nonaktif juga ditolak.
	env, rec = f.do(http.MethodPost, "/devices/me/heartbeat", token, nil)
	if rec.Code != http.StatusForbidden || env.Code != response.CodeDeviceDisabled {
		t.Fatalf("heartbeat nonaktif: status/code = %d/%d, mau 403/2004", rec.Code, env.Code)
	}
}

// TestHeartbeat memperiksa heartbeat device aktif membalas 200 + data null.
func TestHeartbeat(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 2", "AA:BB:CC:DD:EE:02")

	env, rec := f.do(http.MethodPost, "/devices/me/heartbeat", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0", rec.Code, env.Code)
	}
	if string(env.Data) != "null" {
		t.Fatalf("data = %s, mau null", string(env.Data))
	}
}

// titlesData adalah bentuk data GET /titles yang dipakai beberapa test.
type titlesData struct {
	Items []struct {
		ID           int64   `json:"id"`
		Provider     string  `json:"provider"`
		Kind         string  `json:"kind"`
		Title        string  `json:"title"`
		PosterURL    *string `json:"poster_url"`
		EpisodeCount int64   `json:"episode_count"`
		Playable     bool    `json:"playable"`
	} `json:"items"`
	Meta listMeta `json:"meta"`
}

// ids mengembalikan ID item sesuai urutan response.
func (d titlesData) ids() []int64 {
	out := make([]int64, 0, len(d.Items))
	for _, item := range d.Items {
		out = append(out, item.ID)
	}
	return out
}

// TestTitlesList memeriksa envelope, pagination, penyaringan konten siap, serta
// filter kind (tugas uji #3).
func TestTitlesList(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 3", "AA:BB:CC:DD:EE:03")

	env, rec := f.do(http.MethodGet, "/titles", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data titlesData
	f.decode(env, &data)

	// Judul 3 hanya punya rendisi failed → tidak boleh muncul.
	if got, want := data.ids(), []int64{2, 1}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("items = %v, mau %v (default sort -updated_at)", got, want)
	}
	if data.Meta != (listMeta{Page: 1, PerPage: 50, Total: 2, TotalPages: 1}) {
		t.Fatalf("meta = %+v, mau page=1 per_page=50 total=2 total_pages=1", data.Meta)
	}
	if data.Items[0].Title != "Film Lama" || data.Items[0].Kind != "movie" {
		t.Fatalf("item pertama = %+v, mau film lama", data.Items[0])
	}
	if data.Items[0].PosterURL == nil || *data.Items[0].PosterURL != "/media/gen-000001/dens_tv/2/poster.jpg" {
		t.Fatalf("poster_url = %v, mau /media/gen-000001/dens_tv/2/poster.jpg", data.Items[0].PosterURL)
	}
	if !data.Items[0].Playable {
		t.Fatalf("playable = false, mau true untuk judul siap")
	}
	if data.Items[1].EpisodeCount != 2 {
		t.Fatalf("episode_count = %d, mau 2", data.Items[1].EpisodeCount)
	}

	// Filter kind.
	env, rec = f.do(http.MethodGet, "/titles?kind=movie", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("kind=movie: status = %d", rec.Code)
	}
	data = titlesData{}
	f.decode(env, &data)
	if len(data.Items) != 1 || data.Items[0].ID != 2 {
		t.Fatalf("kind=movie: items = %v, mau [2]", data.ids())
	}

	// Pagination halaman kedua dengan per_page=1.
	env, rec = f.do(http.MethodGet, "/titles?per_page=1&page=2", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("page=2: status = %d", rec.Code)
	}
	data = titlesData{}
	f.decode(env, &data)
	if data.Meta != (listMeta{Page: 2, PerPage: 1, Total: 2, TotalPages: 2}) {
		t.Fatalf("meta = %+v, mau page=2 per_page=1 total=2 total_pages=2", data.Meta)
	}
	if len(data.Items) != 1 || data.Items[0].ID != 1 {
		t.Fatalf("items = %v, mau [1]", data.ids())
	}

	// Sort whitelist.
	env, rec = f.do(http.MethodGet, "/titles?sort=title", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sort=title: status = %d", rec.Code)
	}
	data = titlesData{}
	f.decode(env, &data)
	if got := data.ids(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("sort=title: items = %v, mau [1 2]", got)
	}

	// per_page di luar batas → 422/1003.
	env, rec = f.do(http.MethodGet, "/titles?per_page=201", token, nil)
	if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeValidationError {
		t.Fatalf("per_page=201: status/code = %d/%d, mau 422/1003", rec.Code, env.Code)
	}
	if len(env.Errors) == 0 || env.Errors[0].Field != "per_page" {
		t.Fatalf("errors = %+v, mau detail per_page", env.Errors)
	}

	// sort di luar whitelist → 422/1003 (nilai user tidak pernah masuk SQL).
	for _, sort := range []string{"populer", "id%20DESC", "t.title--"} {
		env, rec = f.do(http.MethodGet, "/titles?sort="+sort, token, nil)
		if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeValidationError {
			t.Fatalf("sort=%s: status/code = %d/%d, mau 422/1003", sort, rec.Code, env.Code)
		}
	}

	// Tanpa token tetap 401.
	env, rec = f.do(http.MethodGet, "/titles", "", nil)
	if rec.Code != http.StatusUnauthorized || env.Code != response.CodeUnauthorized {
		t.Fatalf("tanpa token: status/code = %d/%d, mau 401/2000", rec.Code, env.Code)
	}
}

// TestTitlesKindShort memastikan judul Dramahits bisa disaring lewat kind=short
// dan nilai itu tetap muncul pada detail judul.
func TestTitlesKindShort(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 14", "AA:BB:CC:DD:EE:14")

	f.seedMediaTitle(mediaTitleSeed{
		ID:         20,
		PlayableID: 9120,
		Kind:       "short",
		Title:      "Drama Pendek",
		Slug:       "drama-pendek",
	})

	env, rec := f.do(http.MethodGet, "/titles?kind=short", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("kind=short: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var list titlesData
	f.decode(env, &list)
	if got := list.ids(); len(got) != 1 || got[0] != 20 {
		t.Fatalf("kind=short: items = %v, mau [20]", got)
	}
	if list.Items[0].Kind != "short" {
		t.Fatalf("kind = %q, mau short", list.Items[0].Kind)
	}

	env, rec = f.do(http.MethodGet, "/titles/20", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("detail: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var detail struct {
		Kind string `json:"kind"`
	}
	f.decode(env, &detail)
	if detail.Kind != "short" {
		t.Fatalf("detail kind = %q, mau short", detail.Kind)
	}
}

// TestTitleDetail memeriksa detail judul (seasons, first_playable_id) dan 404
// untuk ID yang tidak ada (tugas uji #4).
func TestTitleDetail(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 4", "AA:BB:CC:DD:EE:04")

	env, rec := f.do(http.MethodGet, "/titles/1", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		ID           int64  `json:"id"`
		Title        string `json:"title"`
		EpisodeCount int64  `json:"episode_count"`
		Seasons      []struct {
			SeasonNumber int64 `json:"season_number"`
			EpisodeCount int64 `json:"episode_count"`
		} `json:"seasons"`
		FirstPlayableID *int64 `json:"first_playable_id"`
	}
	f.decode(env, &data)

	if data.ID != 1 || data.Title != "Cinta Pertama" || data.EpisodeCount != 2 {
		t.Fatalf("data = %+v, mau judul 1 dengan 2 episode", data)
	}
	if len(data.Seasons) != 1 || data.Seasons[0].SeasonNumber != 1 || data.Seasons[0].EpisodeCount != 2 {
		t.Fatalf("seasons = %+v, mau [{1 2}]", data.Seasons)
	}
	if data.FirstPlayableID == nil || *data.FirstPlayableID != 9001 {
		t.Fatalf("first_playable_id = %v, mau 9001", data.FirstPlayableID)
	}

	// ID tidak ada → 404/3000.
	env, rec = f.do(http.MethodGet, "/titles/99999", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeTitleNotFound {
		t.Fatalf("status/code = %d/%d, mau 404/3000 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if string(env.Data) != "null" {
		t.Fatalf("data = %s, mau null pada error", string(env.Data))
	}

	// ID bukan angka → 404/3000 juga.
	env, rec = f.do(http.MethodGet, "/titles/abc", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeTitleNotFound {
		t.Fatalf("id bukan angka: status/code = %d/%d, mau 404/3000", rec.Code, env.Code)
	}

	// Judul yang belum punya rendisi ready → 404/3006.
	env, rec = f.do(http.MethodGet, "/titles/3", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeContentNotPublished {
		t.Fatalf("judul belum siap: status/code = %d/%d, mau 404/3006", rec.Code, env.Code)
	}
}

// TestTitleEpisodes memastikan penanda ready mengikuti rendisi generation aktif
// (tugas uji #5).
func TestTitleEpisodes(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 5", "AA:BB:CC:DD:EE:05")

	env, rec := f.do(http.MethodGet, "/titles/1/episodes", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		Items []struct {
			ID            int64   `json:"id"`
			SeasonNumber  int64   `json:"season_number"`
			EpisodeNumber int64   `json:"episode_number"`
			Name          *string `json:"name"`
			PlayableID    *int64  `json:"playable_id"`
			Ready         bool    `json:"ready"`
			StillURL      *string `json:"still_url"`
		} `json:"items"`
		Meta listMeta `json:"meta"`
	}
	f.decode(env, &data)

	if len(data.Items) != 2 {
		t.Fatalf("items = %d, mau 2", len(data.Items))
	}
	if data.Meta != (listMeta{Page: 1, PerPage: 50, Total: 2, TotalPages: 1}) {
		t.Fatalf("meta = %+v", data.Meta)
	}

	first := data.Items[0]
	if first.ID != 301 || !first.Ready {
		t.Fatalf("episode pertama = %+v, mau 301 ready", first)
	}
	if first.PlayableID == nil || *first.PlayableID != 9001 {
		t.Fatalf("playable_id = %v, mau 9001", first.PlayableID)
	}
	if first.Name == nil || *first.Name != "Awal" {
		t.Fatalf("name = %v, mau Awal", first.Name)
	}
	if first.StillURL == nil || *first.StillURL != "/media/gen-000001/dens_tv/1/still-1.jpg" {
		t.Fatalf("still_url = %v", first.StillURL)
	}

	second := data.Items[1]
	if second.ID != 302 || second.Ready {
		t.Fatalf("episode kedua = %+v, mau 302 belum ready", second)
	}
	if second.PlayableID == nil || *second.PlayableID != 9002 {
		t.Fatalf("playable_id = %v, mau 9002", second.PlayableID)
	}

	// Filter season yang tidak ada → list kosong, bukan 404.
	env, rec = f.do(http.MethodGet, "/titles/1/episodes?season=2", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("season=2: status/code = %d/%d, mau 200/0", rec.Code, env.Code)
	}
	data.Items = nil
	f.decode(env, &data)
	if len(data.Items) != 0 || data.Meta.Total != 0 {
		t.Fatalf("season=2: items = %+v, mau kosong", data.Items)
	}

	// Judul tidak ada → 404/3000.
	env, rec = f.do(http.MethodGet, "/titles/99999/episodes", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeTitleNotFound {
		t.Fatalf("judul hilang: status/code = %d/%d, mau 404/3000", rec.Code, env.Code)
	}
}

// searchData adalah bentuk data GET /search.
type searchData struct {
	Titles []struct {
		ID           int64   `json:"id"`
		Kind         string  `json:"kind"`
		Title        string  `json:"title"`
		PosterURL    *string `json:"poster_url"`
		EpisodeCount int64   `json:"episode_count"`
		Match        string  `json:"match"`
	} `json:"titles"`
	Episodes []struct {
		ID            int64   `json:"id"`
		TitleID       int64   `json:"title_id"`
		Title         string  `json:"title"`
		SeasonNumber  int64   `json:"season_number"`
		EpisodeNumber int64   `json:"episode_number"`
		Name          *string `json:"name"`
		PlayableID    *int64  `json:"playable_id"`
		Ready         bool    `json:"ready"`
		StillURL      *string `json:"still_url"`
		Match         string  `json:"match"`
	} `json:"episodes"`
	Meta struct {
		Limit         int64 `json:"limit"`
		TitlesTotal   int64 `json:"titles_total"`
		EpisodesTotal int64 `json:"episodes_total"`
	} `json:"meta"`
}

// TestSearch memeriksa validasi q, hasil kosong, dan penggabungan hasil judul +
// episode (tugas uji #6).
func TestSearch(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 6", "AA:BB:CC:DD:EE:06")

	// q terlalu pendek → 422/3005.
	env, rec := f.do(http.MethodGet, "/search?q=a", token, nil)
	if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeSearchQueryInvalid {
		t.Fatalf("q=a: status/code = %d/%d, mau 422/3005 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	if len(env.Errors) == 0 || env.Errors[0].Field != "q" {
		t.Fatalf("errors = %+v, mau detail q", env.Errors)
	}

	// q kosong juga 422/3005.
	env, rec = f.do(http.MethodGet, "/search", token, nil)
	if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeSearchQueryInvalid {
		t.Fatalf("tanpa q: status/code = %d/%d, mau 422/3005", rec.Code, env.Code)
	}

	// Tidak ada yang cocok → 200 dengan array kosong, bukan 404.
	env, rec = f.do(http.MethodGet, "/search?q=tidakadayangcocok", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("tanpa hasil: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	var data searchData
	f.decode(env, &data)
	if len(data.Titles) != 0 || len(data.Episodes) != 0 {
		t.Fatalf("tanpa hasil: titles=%+v episodes=%+v, mau kosong", data.Titles, data.Episodes)
	}
	if data.Titles == nil || data.Episodes == nil {
		t.Fatalf("titles/episodes bernilai null, mau array kosong: %s", string(env.Data))
	}
	if data.Meta.Limit != 20 {
		t.Fatalf("meta.limit = %d, mau 20", data.Meta.Limit)
	}

	// Cocok di judul dan di nama episode.
	env, rec = f.do(http.MethodGet, "/search?q=cinta", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("q=cinta: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
	data = searchData{}
	f.decode(env, &data)
	if len(data.Titles) != 1 || data.Titles[0].ID != 1 || data.Titles[0].Match != "title" {
		t.Fatalf("titles = %+v, mau satu judul 1 dengan match=title", data.Titles)
	}
	// Poster hasil search juga memakai path di dalam generation aktif.
	if data.Titles[0].PosterURL == nil || *data.Titles[0].PosterURL != "/media/gen-000001/dens_tv/1/poster.jpg" {
		t.Fatalf("poster_url = %v, mau /media/gen-000001/dens_tv/1/poster.jpg", data.Titles[0].PosterURL)
	}
	if len(data.Episodes) != 1 || data.Episodes[0].ID != 302 || data.Episodes[0].Match != "episode" {
		t.Fatalf("episodes = %+v, mau satu episode 302 dengan match=episode", data.Episodes)
	}
	// Still episode pada hasil search mengikuti aturan generation yang sama.
	if data.Episodes[0].StillURL == nil || *data.Episodes[0].StillURL != "/media/gen-000001/dens_tv/1/still-2.jpg" {
		t.Fatalf("still_url = %v, mau /media/gen-000001/dens_tv/1/still-2.jpg", data.Episodes[0].StillURL)
	}
	if data.Meta.TitlesTotal != 1 || data.Meta.EpisodesTotal != 1 {
		t.Fatalf("meta = %+v, mau titles_total=1 episodes_total=1", data.Meta)
	}
	if data.Episodes[0].Name == nil || *data.Episodes[0].Name != "Cinta Kedua" {
		t.Fatalf("nama episode = %v, mau Cinta Kedua", data.Episodes[0].Name)
	}
	if data.Episodes[0].Ready {
		t.Fatalf("episode 302 ready = true, mau false (rendisi pending)")
	}

	// type=title mengosongkan episodes dan sebaliknya.
	env, rec = f.do(http.MethodGet, "/search?q=cinta&type=title", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("type=title: status = %d", rec.Code)
	}
	data = searchData{}
	f.decode(env, &data)
	if len(data.Titles) != 1 || len(data.Episodes) != 0 || data.Meta.EpisodesTotal != 0 {
		t.Fatalf("type=title: titles=%d episodes=%d, mau 1/0", len(data.Titles), len(data.Episodes))
	}

	env, rec = f.do(http.MethodGet, "/search?q=cinta&type=episode", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("type=episode: status = %d", rec.Code)
	}
	data = searchData{}
	f.decode(env, &data)
	if len(data.Titles) != 0 || len(data.Episodes) != 1 {
		t.Fatalf("type=episode: titles=%d episodes=%d, mau 0/1", len(data.Titles), len(data.Episodes))
	}

	// limit di luar batas → 422/1003.
	env, rec = f.do(http.MethodGet, "/search?q=cinta&limit=99", token, nil)
	if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeValidationError {
		t.Fatalf("limit=99: status/code = %d/%d, mau 422/1003", rec.Code, env.Code)
	}

	// Wildcard user tidak boleh menjadi pola: "%" harus dicari sebagai teks.
	env, rec = f.do(http.MethodGet, "/search?q=%25%25", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("q=%%%%: status = %d", rec.Code)
	}
	data = searchData{}
	f.decode(env, &data)
	if len(data.Titles) != 0 || len(data.Episodes) != 0 {
		t.Fatalf("wildcard bocor: titles=%d episodes=%d, mau 0/0", len(data.Titles), len(data.Episodes))
	}
}

// TestPlayableDetail memastikan hanya varian ready yang ditawarkan dan playable
// tanpa varian siap dijawab 409/3003 (tugas uji #7).
func TestPlayableDetail(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 7", "AA:BB:CC:DD:EE:07")

	env, rec := f.do(http.MethodGet, "/playables/9001", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		ID      int64  `json:"id"`
		TitleID int64  `json:"title_id"`
		Title   string `json:"title"`
		Episode *struct {
			SeasonNumber  int64   `json:"season_number"`
			EpisodeNumber int64   `json:"episode_number"`
			Name          *string `json:"name"`
		} `json:"episode"`
		DurationSeconds   *int64 `json:"duration_seconds"`
		Generation        string `json:"generation"`
		MasterPlaylistURL string `json:"master_playlist_url"`
		Variants          []struct {
			Quality     string `json:"quality"`
			Width       *int64 `json:"width"`
			Height      *int64 `json:"height"`
			BitrateKbps *int64 `json:"bitrate_kbps"`
			Ready       bool   `json:"ready"`
		} `json:"variants"`
		Subtitles []struct {
			Lang   string  `json:"lang"`
			Label  *string `json:"label"`
			Format string  `json:"format"`
			URL    string  `json:"url"`
		} `json:"subtitles"`
	}
	f.decode(env, &data)

	if data.ID != 9001 || data.TitleID != 1 || data.Title != "Cinta Pertama" {
		t.Fatalf("data = %+v, mau playable 9001 judul 1", data)
	}
	if data.Episode == nil || data.Episode.SeasonNumber != 1 || data.Episode.EpisodeNumber != 1 {
		t.Fatalf("episode = %+v, mau s01e01", data.Episode)
	}
	if data.Generation != "gen-000001" {
		t.Fatalf("generation = %q, mau gen-000001", data.Generation)
	}
	if data.MasterPlaylistURL != "/api/v1/playables/9001/master.m3u8" {
		t.Fatalf("master_playlist_url = %q", data.MasterPlaylistURL)
	}
	if len(data.Variants) != 2 {
		t.Fatalf("variants = %+v, mau 2 varian ready (720p gagal tidak ikut)", data.Variants)
	}
	if data.Variants[0].Quality != "480p" || data.Variants[1].Quality != "360p" {
		t.Fatalf("urutan varian = %+v, mau height menurun (480p lalu 360p)", data.Variants)
	}
	if data.Variants[0].Height == nil || *data.Variants[0].Height != 480 || !data.Variants[0].Ready {
		t.Fatalf("varian pertama = %+v, mau 480p ready", data.Variants[0])
	}
	if len(data.Subtitles) != 1 || data.Subtitles[0].Lang != "id" {
		t.Fatalf("subtitles = %+v, mau satu subtitle id", data.Subtitles)
	}
	if want := "/media/gen-000001/dens_tv/series/1/s01e01/subtitle-id.vtt"; data.Subtitles[0].URL != want {
		t.Fatalf("url subtitle = %q, mau %q", data.Subtitles[0].URL, want)
	}

	// 9002 belum punya rendisi ready → 409/3003.
	env, rec = f.do(http.MethodGet, "/playables/9002", token, nil)
	if rec.Code != http.StatusConflict || env.Code != response.CodePlayableNotReady {
		t.Fatalf("belum siap: status/code = %d/%d, mau 409/3003 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	// Playable tidak ada → 404/3002.
	env, rec = f.do(http.MethodGet, "/playables/123456", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodePlayableNotFound {
		t.Fatalf("tidak ada: status/code = %d/%d, mau 404/3002", rec.Code, env.Code)
	}
}

// TestMasterPlaylist memeriksa master playlist dinamis (tugas uji #8).
func TestMasterPlaylist(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 8", "AA:BB:CC:DD:EE:08")

	rec := f.request(http.MethodGet, "/playables/9001/master.m3u8", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("Content-Type = %q, mau application/vnd.apple.mpegurl", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, mau no-cache", cc)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "#EXTM3U") {
		t.Fatalf("playlist tidak diawali #EXTM3U:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		t.Fatalf("playlist tanpa #EXT-X-STREAM-INF:\n%s", body)
	}
	const want480 = "/media/gen-000001/dens_tv/series/1/s01e01/480p/index.m3u8"
	const want360 = "/media/gen-000001/dens_tv/series/1/s01e01/360p/index.m3u8"
	if !strings.Contains(body, want480) || !strings.Contains(body, want360) {
		t.Fatalf("URI varian tidak lengkap:\n%s", body)
	}
	if strings.Contains(body, "720p") {
		t.Fatalf("varian failed ikut ditawarkan:\n%s", body)
	}
	if strings.Index(body, want480) > strings.Index(body, want360) {
		t.Fatalf("urutan varian salah (480p harus sebelum 360p):\n%s", body)
	}
	if strings.Count(body, "#EXT-X-STREAM-INF") != 2 {
		t.Fatalf("jumlah varian = %d, mau 2:\n%s", strings.Count(body, "#EXT-X-STREAM-INF"), body)
	}

	// Tanpa varian ready → 409/3003 dalam envelope.
	env, rec := f.do(http.MethodGet, "/playables/9002/master.m3u8", token, nil)
	if rec.Code != http.StatusConflict || env.Code != response.CodePlayableNotReady {
		t.Fatalf("belum siap: status/code = %d/%d, mau 409/3003 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}
}

// TestProgressUpsert memastikan POST /progress idempoten dan posisi terbaru
// menang, serta GET /progress mengembalikan riwayat (tugas uji #9).
func TestProgressUpsert(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 9", "AA:BB:CC:DD:EE:09")

	save := func(position int64) {
		t.Helper()
		env, rec := f.do(http.MethodPost, "/progress", token, map[string]any{
			"playable_id":      9001,
			"position_seconds": position,
			"duration_seconds": 2700,
			"completed":        false,
		})
		if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
			t.Fatalf("POST /progress posisi %d: status/code = %d/%d, mau 200/0 (body=%s)",
				position, rec.Code, env.Code, rec.Body.String())
		}
		if string(env.Data) != "null" {
			t.Fatalf("POST /progress: data = %s, mau null", string(env.Data))
		}
	}

	save(120)
	f.now = f.now.Add(30 * time.Second)
	save(742)

	// Hanya ada satu baris, dan posisinya yang terakhir.
	var rows int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM playback_progress`).Scan(&rows); err != nil {
		t.Fatalf("hitung progress: %v", err)
	}
	if rows != 1 {
		t.Fatalf("baris progress = %d, mau 1 (upsert)", rows)
	}

	var position int64
	if err := f.db.QueryRow(`SELECT position_seconds FROM playback_progress`).Scan(&position); err != nil {
		t.Fatalf("baca posisi: %v", err)
	}
	if position != 742 {
		t.Fatalf("posisi tersimpan = %d, mau 742", position)
	}

	// GET /progress/{playable_id}.
	env, rec := f.do(http.MethodGet, "/progress/9001", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /progress/9001: status/code = %d/%d, mau 200/0", rec.Code, env.Code)
	}
	var item struct {
		PlayableID      int64  `json:"playable_id"`
		TitleID         int64  `json:"title_id"`
		Title           string `json:"title"`
		PositionSeconds int64  `json:"position_seconds"`
		DurationSeconds *int64 `json:"duration_seconds"`
		ProgressPercent int64  `json:"progress_percent"`
		Completed       bool   `json:"completed"`
		UpdatedAt       any    `json:"updated_at"`
		Episode         *struct {
			SeasonNumber  int64 `json:"season_number"`
			EpisodeNumber int64 `json:"episode_number"`
		} `json:"episode"`
	}
	f.decode(env, &item)
	if item.PlayableID != 9001 || item.Title != "Cinta Pertama" || item.PositionSeconds != 742 {
		t.Fatalf("item = %+v, mau playable 9001 posisi 742", item)
	}
	if item.ProgressPercent != 27 {
		t.Fatalf("progress_percent = %d, mau 27", item.ProgressPercent)
	}
	if item.Episode == nil || item.Episode.EpisodeNumber != 1 {
		t.Fatalf("episode = %+v, mau s01e01", item.Episode)
	}
	if item.UpdatedAt == nil {
		t.Fatalf("updated_at null, mau terisi")
	}

	// Belum ada progress untuk playable lain → 404/8000.
	env, rec = f.do(http.MethodGet, "/progress/9003", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeProgressNotFound {
		t.Fatalf("progress lain: status/code = %d/%d, mau 404/8000", rec.Code, env.Code)
	}

	// GET /progress mengembalikan satu item + meta.
	f.now = f.now.Add(30 * time.Second)
	save(800)
	env, rec = f.do(http.MethodGet, "/progress", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("GET /progress: status/code = %d/%d, mau 200/0", rec.Code, env.Code)
	}
	var list struct {
		Items []struct {
			PlayableID      int64 `json:"playable_id"`
			PositionSeconds int64 `json:"position_seconds"`
		} `json:"items"`
		Meta struct {
			Limit int64 `json:"limit"`
			Total int64 `json:"total"`
		} `json:"meta"`
	}
	f.decode(env, &list)
	if len(list.Items) != 1 || list.Items[0].PositionSeconds != 800 {
		t.Fatalf("items = %+v, mau satu item posisi 800", list.Items)
	}
	if list.Meta.Limit != 20 || list.Meta.Total != 1 {
		t.Fatalf("meta = %+v, mau limit=20 total=1", list.Meta)
	}

	// Validasi: posisi negatif → 422/8001.
	env, rec = f.do(http.MethodPost, "/progress", token, map[string]any{
		"playable_id":      9001,
		"position_seconds": -5,
	})
	if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeProgressInvalid {
		t.Fatalf("posisi negatif: status/code = %d/%d, mau 422/8001 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	// Playable tidak ada → 404/3002.
	env, rec = f.do(http.MethodPost, "/progress", token, map[string]any{
		"playable_id":      123456,
		"position_seconds": 10,
	})
	if rec.Code != http.StatusNotFound || env.Code != response.CodePlayableNotFound {
		t.Fatalf("playable hilang: status/code = %d/%d, mau 404/3002", rec.Code, env.Code)
	}
}

// TestRetiredGenerationStillServes memastikan konten yang sudah diunduh tetap
// tersedia walau batch tersebut bukan batch aktif.
func TestRetiredGenerationStillServes(t *testing.T) {
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 10", "AA:BB:CC:DD:EE:10")

	f.exec(`UPDATE generations SET status = 'retired' WHERE id = 1`)

	for _, target := range []string{
		"/titles",
		"/titles/1",
		"/titles/1/episodes",
		"/search?q=cinta",
		"/playables/9001",
	} {
		env, rec := f.do(http.MethodGet, target, token, nil)
		if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
			t.Fatalf("%s: status/code = %d/%d, mau 200/0 (body=%s)", target, rec.Code, env.Code, rec.Body.String())
		}
	}

	rec := f.request(http.MethodGet, "/playables/9001/master.m3u8", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("master playlist: status = %d, mau 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), "#EXTM3U") {
		t.Fatalf("master playlist bukan HLS: %s", rec.Body.String())
	}
}

// TestAdminOverview memastikan route admin hanya ada bila middleware admin
// dipasang (tugas uji #11).
func TestAdminOverview(t *testing.T) {
	// Tanpa AdminOnly: route tidak terpasang → 404/1002.
	f := newFixture(t, nil)
	token, _ := f.registerDevice("Tablet Kursi 11", "AA:BB:CC:DD:EE:11")

	env, rec := f.do(http.MethodGet, "/admin/overview", token, nil)
	if rec.Code != http.StatusNotFound || env.Code != response.CodeNotFound {
		t.Fatalf("tanpa middleware: status/code = %d/%d, mau 404/1002 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	// Dengan AdminOnly (pass-through pada test ini).
	f = newFixture(t, func(next http.Handler) http.Handler { return next })
	token, _ = f.registerDevice("Tablet Kursi 12", "AA:BB:CC:DD:EE:12")

	env, rec = f.do(http.MethodGet, "/admin/overview", token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("dengan middleware: status/code = %d/%d, mau 200/0 (body=%s)", rec.Code, env.Code, rec.Body.String())
	}

	var data struct {
		Generation *struct {
			ID         int64  `json:"id"`
			Label      string `json:"label"`
			Status     string `json:"status"`
			TotalBytes int64  `json:"total_bytes"`
		} `json:"generation"`
		Catalog struct {
			Titles          int64 `json:"titles"`
			Episodes        int64 `json:"episodes"`
			Playables       int64 `json:"playables"`
			RenditionsReady int64 `json:"renditions_ready"`
		} `json:"catalog"`
		Devices struct {
			Registered int64 `json:"registered"`
			Online     int64 `json:"online"`
		} `json:"devices"`
		Storage struct {
			UsedBytes int64 `json:"used_bytes"`
		} `json:"storage"`
		Sync struct {
			ID     *int64 `json:"id"`
			Status string `json:"status"`
		} `json:"sync"`
	}
	f.decode(env, &data)

	if data.Generation == nil || data.Generation.Label != "gen-000001" || data.Generation.Status != "active" {
		t.Fatalf("generation = %+v, mau gen-000001 aktif", data.Generation)
	}
	if data.Generation.TotalBytes != 4096 {
		t.Fatalf("total_bytes = %d, mau 4096", data.Generation.TotalBytes)
	}
	if data.Catalog.Titles != 3 || data.Catalog.Episodes != 2 || data.Catalog.Playables != 4 {
		t.Fatalf("catalog = %+v, mau titles=3 episodes=2 playables=4", data.Catalog)
	}
	if data.Catalog.RenditionsReady != 3 {
		t.Fatalf("renditions_ready = %d, mau 3", data.Catalog.RenditionsReady)
	}
	if data.Devices.Registered != 1 || data.Devices.Online != 1 {
		t.Fatalf("devices = %+v, mau registered=1 online=1", data.Devices)
	}
	if data.Storage.UsedBytes != 4096 {
		t.Fatalf("storage.used_bytes = %d, mau 4096", data.Storage.UsedBytes)
	}
	if data.Sync.ID != nil || data.Sync.Status != "idle" {
		t.Fatalf("sync = %+v, mau idle", data.Sync)
	}
}

// TestAuthenticatorDirect menguji Authenticator tanpa lapisan HTTP: token valid
// mengembalikan device dan memperbarui last_seen_at, token asing ditolak, dan
// device nonaktif dilaporkan ErrDeviceDisabled.
func TestAuthenticatorDirect(t *testing.T) {
	f := newFixture(t, nil)
	token, id := f.registerDevice("Tablet Kursi 14", "AA:BB:CC:DD:EE:14")

	auth := api.NewAuthenticator(f.db, func() time.Time { return f.now })

	device, err := auth.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if device.ID != id || device.Note != "Tablet Kursi 14" || device.SeatNumber != "7" {
		t.Fatalf("device = %+v, mau device %d", device, id)
	}

	var lastSeen *string
	if err := f.db.QueryRow(`SELECT last_seen_at FROM devices WHERE id = ?`, id).Scan(&lastSeen); err != nil {
		t.Fatalf("baca last_seen_at: %v", err)
	}
	if lastSeen == nil || *lastSeen != "2026-10-20T11:30:00Z" {
		t.Fatalf("last_seen_at = %v, mau 2026-10-20T11:30:00Z", lastSeen)
	}

	if _, err := auth.Authenticate(t.Context(), "mc_asing"); !errors.Is(err, httpserver.ErrUnauthorized) {
		t.Fatalf("token asing: err = %v, mau ErrUnauthorized", err)
	}

	f.exec(`UPDATE devices SET is_active = 0 WHERE id = ?`, id)
	if _, err := auth.Authenticate(t.Context(), token); !errors.Is(err, httpserver.ErrDeviceDisabled) {
		t.Fatalf("device nonaktif: err = %v, mau ErrDeviceDisabled", err)
	}
}
