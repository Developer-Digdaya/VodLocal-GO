package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Nilai filter status perangkat (10.4): halaman /devices hanya mengenal
// perangkat aktif dan tidak aktif.
const (
	deviceStatusAll      = "all"
	deviceStatusActive   = "active"
	deviceStatusInactive = "inactive"
)

// deviceRow adalah satu baris devices untuk template.
type deviceRow struct {
	ID         int64
	Note       string
	SeatNumber string
	MACAddress string
	IPAddress  string
	IsActive   bool
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
	From       int
	To         int
	Devices    []deviceRow
}

// handleDevices melayani GET /devices (10.4).
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
	case deviceStatusActive, deviceStatusInactive:
	default:
		status = deviceStatusAll
	}
	sortKey, orderBy := deviceOrderClause(strings.TrimSpace(queryGet(values, "sort")))
	page, perPage := parsePage(values)

	where, args := deviceFilter(search, status)

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

	info := newPageInfo(page, perPage, data.Total)
	data.Page = info.Page
	data.PerPage = info.PerPage
	data.TotalPages = info.TotalPages
	data.HasPrev = info.HasPrev
	data.HasNext = info.HasNext
	data.PrevPage = info.PrevPage
	data.NextPage = info.NextPage
	data.From = info.From
	data.To = info.To

	queryArgs := append(append([]any{}, args...), info.PerPage, info.offset())
	rows, err := a.read.QueryContext(ctx, `
SELECT id, COALESCE(note, ''), COALESCE(seat_number, ''), COALESCE(mac_address, ''), COALESCE(ip_address, ''),
       is_active, COALESCE(last_seen_at, ''), created_at
FROM devices`+where+`
ORDER BY `+orderBy+`
LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
		return data, false
	}
	defer func() { _ = rows.Close() }()

	devices := make([]deviceRow, 0, info.PerPage)
	for rows.Next() {
		var (
			device   deviceRow
			isActive int
		)
		if err := rows.Scan(&device.ID, &device.Note, &device.SeatNumber, &device.MACAddress,
			&device.IPAddress, &isActive, &device.LastSeenAt, &device.CreatedAt); err != nil {
			a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
			return data, false
		}
		device.IsActive = isActive == 1
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		a.internalError(w, r, err, "Gagal membaca daftar perangkat.")
		return data, false
	}
	data.Devices = devices

	return data, true
}

// deviceLabel adalah sebutan perangkat pada flash message. Nama boleh kosong
// (kolom note nullable), jadi dipakai nomor kursi lalu "ID <id>" sebagai
// cadangan agar pesan tidak pernah berbunyi `Perangkat "" diaktifkan.`
func deviceLabel(device *deviceRow) string {
	if note := strings.TrimSpace(device.Note); note != "" {
		return note
	}
	if seat := strings.TrimSpace(device.SeatNumber); seat != "" {
		return seat
	}
	return fmt.Sprintf("ID %d", device.ID)
}

// deviceOrderClause memetakan sort ke klausa ORDER BY yang diizinkan.
//
// note dan seat_number boleh NULL, jadi keduanya di-COALESCE agar urutan tidak
// bergantung pada posisi NULL di SQLite.
func deviceOrderClause(sort string) (key, orderBy string) {
	switch sort {
	case "seat_number":
		return "seat_number", `COALESCE(seat_number, '') ASC, COALESCE(note, '') ASC, id ASC`
	case "note":
		return "note", `COALESCE(note, '') ASC, id ASC`
	case "-created_at":
		return "-created_at", "created_at DESC, id DESC"
	default:
		return "-last_seen_at", `COALESCE(last_seen_at, '') DESC, id DESC`
	}
}

// deviceFilter menyusun klausa WHERE pencarian dan filter status.
func deviceFilter(search, status string) (string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)

	if search != "" {
		pattern := likeContains(search)
		where = append(where, `(note LIKE ?`+likeEscapeClause+
			` OR COALESCE(seat_number, '') LIKE ?`+likeEscapeClause+
			` OR COALESCE(mac_address, '') LIKE ?`+likeEscapeClause+
			` OR COALESCE(ip_address, '') LIKE ?`+likeEscapeClause+`)`)
		args = append(args, pattern, pattern, pattern, pattern)
	}

	switch status {
	case deviceStatusActive:
		where = append(where, "is_active = 1")
	case deviceStatusInactive:
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
	a.flashTo(r, "success", fmt.Sprintf("Perangkat %q %s.", deviceLabel(device), action))
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// handleDeviceDelete melayani POST /devices/{id}/delete.
//
// Menghapus baris devices secara permanen. playback_progress milik perangkat
// ikut terhapus lewat ON DELETE CASCADE.
func (a *Admin) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.PostFormValue("confirm")) != "DELETE" {
		a.renderError(w, r, http.StatusBadRequest, "Konfirmasi hapus perangkat tidak valid.")
		return
	}

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

	if _, err := a.write.ExecContext(r.Context(),
		`DELETE FROM devices WHERE id = ?`, id,
	); err != nil {
		a.internalError(w, r, err, "Gagal menghapus perangkat.")
		return
	}

	a.log.Info("admin: perangkat dihapus",
		"device_id", id, "note", device.Note, "username", a.sessionUsername(r))
	a.flashTo(r, "success", fmt.Sprintf("Perangkat %q dihapus.", deviceLabel(device)))
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// deviceByID membaca satu perangkat, atau nil bila tidak ada.
func (a *Admin) deviceByID(ctx context.Context, id int64) (*deviceRow, error) {
	var (
		device   deviceRow
		isActive int
	)
	err := a.read.QueryRowContext(ctx, `
SELECT id, COALESCE(note, ''), COALESCE(seat_number, ''), COALESCE(mac_address, ''), COALESCE(ip_address, ''),
       is_active, COALESCE(last_seen_at, ''), created_at
FROM devices WHERE id = ?`, id,
	).Scan(&device.ID, &device.Note, &device.SeatNumber, &device.MACAddress,
		&device.IPAddress, &isActive, &device.LastSeenAt, &device.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	device.IsActive = isActive == 1
	return &device, nil
}
