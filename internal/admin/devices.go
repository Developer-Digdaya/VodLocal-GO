package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Parameter token perangkat (docs/api-spec.md §2/§6.1).
const (
	deviceTokenPrefix = "mc_"
	deviceTokenBytes  = 32
)

// Nilai filter status perangkat (§10.4).
const (
	deviceStatusAll      = "all"
	deviceStatusOnline   = "online"
	deviceStatusOffline  = "offline"
	deviceStatusDisabled = "disabled"
)

// deviceRow adalah satu baris devices untuk template.
type deviceRow struct {
	ID         int64
	Name       string
	SeatNumber string
	MACAddress string
	IPAddress  string
	IsActive   bool
	Status     string
	LastSeenAt string
	CreatedAt  string
}

// devicesData adalah data halaman /devices.
type devicesData struct {
	base
	Q          string
	Status     string
	Sort       string
	Page       int
	PerPage    int
	Total      int64
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int
	Devices    []deviceRow
}

// handleDevices melayani GET /devices (§10.4).
func (a *Admin) handleDevices(w http.ResponseWriter, r *http.Request) {
	data, ok := a.devicesData(w, r, "")
	if !ok {
		return
	}
	a.renderPage(w, r, "devices", data, http.StatusOK)
}

// devicesData membaca daftar perangkat sesuai query dan menyiapkan data halaman.
func (a *Admin) devicesData(w http.ResponseWriter, r *http.Request, errMessage string) (devicesData, bool) {
	ctx := r.Context()
	values := r.URL.Query()

	search := normalizeQuery(queryGet(values, "q"))
	status := strings.ToLower(strings.TrimSpace(queryGet(values, "status")))
	switch status {
	case deviceStatusOnline, deviceStatusOffline, deviceStatusDisabled:
	default:
		status = deviceStatusAll
	}
	sortKey, orderBy := deviceOrderClause(strings.TrimSpace(queryGet(values, "sort")))
	page, perPage := parsePage(values)

	cutoff := a.nowUTC().Add(-onlineWindow).Format(timeLayout)
	where, args := deviceFilter(search, status, cutoff)

	data := devicesData{
		base:    a.base(w, r, "Perangkat", "devices"),
		Q:       search,
		Status:  status,
		Sort:    sortKey,
		Page:    page,
		PerPage: perPage,
	}
	data.Error = errMessage

	if err := a.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`+where, args...).Scan(&data.Total); err != nil {
		a.internalError(w, r, err, "Gagal menghitung perangkat.")
		return data, false
	}

	queryArgs := append(append([]any{}, args...), perPage, (page-1)*perPage)
	rows, err := a.read.QueryContext(ctx, `
SELECT id, name, COALESCE(seat_number, ''), COALESCE(mac_address, ''), COALESCE(ip_address, ''),
       is_active, COALESCE(last_seen_at, ''), created_at
FROM devices`+where+`
ORDER BY `+orderBy+`
LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
		return data, false
	}
	defer func() { _ = rows.Close() }()

	devices := make([]deviceRow, 0, perPage)
	for rows.Next() {
		var (
			device   deviceRow
			isActive int
		)
		if err := rows.Scan(&device.ID, &device.Name, &device.SeatNumber, &device.MACAddress,
			&device.IPAddress, &isActive, &device.LastSeenAt, &device.CreatedAt); err != nil {
			a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
			return data, false
		}
		device.IsActive = isActive == 1
		device.Status = deviceStatusName(device, cutoff)
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
		return data, false
	}
	data.Devices = devices

	if data.Total > 0 {
		data.TotalPages = int((data.Total + int64(perPage) - 1) / int64(perPage))
	}
	data.HasPrev = page > 1
	data.PrevPage = page - 1
	if data.PrevPage < 1 {
		data.PrevPage = 1
	}
	data.HasNext = page < data.TotalPages
	data.NextPage = page + 1

	return data, true
}

// deviceStatusName menghitung status tampilan sebuah perangkat (§10.4).
func deviceStatusName(device deviceRow, cutoff string) string {
	if !device.IsActive {
		return deviceStatusDisabled
	}
	if device.LastSeenAt != "" && device.LastSeenAt >= cutoff {
		return deviceStatusOnline
	}
	return deviceStatusOffline
}

// deviceOrderClause memetakan sort ke klausa ORDER BY yang diizinkan.
func deviceOrderClause(sort string) (key, orderBy string) {
	switch sort {
	case "seat_number":
		return "seat_number", `COALESCE(seat_number, '') ASC, name ASC`
	case "name":
		return "name", "name ASC, id ASC"
	case "-created_at":
		return "-created_at", "created_at DESC, id DESC"
	default:
		return "-last_seen_at", `COALESCE(last_seen_at, '') DESC, id DESC`
	}
}

// deviceFilter menyusun klausa WHERE pencarian dan filter status.
func deviceFilter(search, status, cutoff string) (string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 5)

	if search != "" {
		pattern := likeContains(search)
		where = append(where, `(name LIKE ?`+likeEscapeClause+
			` OR COALESCE(seat_number, '') LIKE ?`+likeEscapeClause+
			` OR COALESCE(mac_address, '') LIKE ?`+likeEscapeClause+
			` OR COALESCE(ip_address, '') LIKE ?`+likeEscapeClause+`)`)
		args = append(args, pattern, pattern, pattern, pattern)
	}

	switch status {
	case deviceStatusOnline:
		where = append(where, "is_active = 1 AND last_seen_at IS NOT NULL AND last_seen_at >= ?")
		args = append(args, cutoff)
	case deviceStatusOffline:
		where = append(where, "is_active = 1 AND (last_seen_at IS NULL OR last_seen_at < ?)")
		args = append(args, cutoff)
	case deviceStatusDisabled:
		where = append(where, "is_active = 0")
	}

	return " WHERE " + strings.Join(where, " AND "), args
}

// handleDeviceDisable melayani POST /devices/{id}/disable.
func (a *Admin) handleDeviceDisable(w http.ResponseWriter, r *http.Request) {
	a.setDeviceActive(w, r, false)
}

// handleDeviceEnable melayani POST /devices/{id}/enable.
func (a *Admin) handleDeviceEnable(w http.ResponseWriter, r *http.Request) {
	a.setDeviceActive(w, r, true)
}

// setDeviceActive mengubah devices.is_active lalu mengalihkan ke /devices.
func (a *Admin) setDeviceActive(w http.ResponseWriter, r *http.Request, active bool) {
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Perangkat tidak ditemukan.")
		return
	}

	device, err := a.deviceByID(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca perangkat.")
		return
	}
	if device == nil {
		a.renderError(w, r, http.StatusNotFound, "Perangkat tidak ditemukan.")
		return
	}

	value := 0
	action := "dinonaktifkan"
	if active {
		value = 1
		action = "diaktifkan"
	}

	if _, err := a.write.ExecContext(r.Context(),
		`UPDATE devices SET is_active = ? WHERE id = ?`, value, id,
	); err != nil {
		a.internalError(w, r, err, "Gagal memperbarui perangkat.")
		return
	}

	a.log.Info("admin: status perangkat diubah",
		"device_id", id, "is_active", value, "username", a.sessionUsername(r))
	a.flashTo(r, "success", fmt.Sprintf("Perangkat %q %s.", device.Name, action))
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// handleDeviceRotate melayani POST /devices/{id}/rotate-token.
//
// Token baru dibuat di sini, disimpan sebagai SHA-256 hex, dan hanya
// ditampilkan sekali lewat flash message; token mentah tidak pernah di-log.
func (a *Admin) handleDeviceRotate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Perangkat tidak ditemukan.")
		return
	}

	device, err := a.deviceByID(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca perangkat.")
		return
	}
	if device == nil {
		a.renderError(w, r, http.StatusNotFound, "Perangkat tidak ditemukan.")
		return
	}

	token, hash, err := newDeviceToken()
	if err != nil {
		a.internalError(w, r, err, "Gagal membuat token perangkat baru.")
		return
	}

	if _, err := a.write.ExecContext(r.Context(),
		`UPDATE devices SET token_hash = ? WHERE id = ?`, hash, id,
	); err != nil {
		a.internalError(w, r, err, "Gagal menyimpan token perangkat baru.")
		return
	}

	a.log.Info("admin: token perangkat dirotasi",
		"device_id", id, "username", a.sessionUsername(r))
	a.flashTo(r, "success", fmt.Sprintf(
		"Token baru untuk perangkat %q: %s — simpan sekarang, token hanya ditampilkan sekali.",
		device.Name, token))
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// deviceByID membaca satu perangkat, atau nil bila tidak ada.
func (a *Admin) deviceByID(ctx context.Context, id int64) (*deviceRow, error) {
	var (
		device   deviceRow
		isActive int
	)
	err := a.read.QueryRowContext(ctx, `
SELECT id, name, COALESCE(seat_number, ''), COALESCE(mac_address, ''), COALESCE(ip_address, ''),
       is_active, COALESCE(last_seen_at, ''), created_at
FROM devices WHERE id = ?`, id,
	).Scan(&device.ID, &device.Name, &device.SeatNumber, &device.MACAddress,
		&device.IPAddress, &isActive, &device.LastSeenAt, &device.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	device.IsActive = isActive == 1
	device.Status = deviceStatusName(device, a.nowUTC().Add(-onlineWindow).Format(timeLayout))
	return &device, nil
}

// newDeviceToken membuat token perangkat "mc_"+base64url(32 byte acak) beserta
// SHA-256 hex yang disimpan di database.
func newDeviceToken() (token, hash string, err error) {
	raw := make([]byte, deviceTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("admin: buat token perangkat: %w", err)
	}
	token = deviceTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

// hashToken menghitung SHA-256 hex dari token perangkat.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
