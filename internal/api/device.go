package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

const (
	// tokenPrefix menandai token device agar mudah dikenali di log/dukungan.
	tokenPrefix = "mc_"
	// tokenRandomBytes adalah jumlah byte acak di dalam token (6.1).
	tokenRandomBytes = 32

	// Batas panjang field registrasi (6.1).
	macAddressMax = 128
	deviceNoteMax = 100
	seatNumberMax = 20
	appVersionMax = 20
)

// Authenticator memverifikasi bearer token device. Nilainya memenuhi
// httpserver.DeviceAuthenticator.
type Authenticator struct {
	db  *sql.DB
	now func() time.Time
}

// NewAuthenticator membuat Authenticator di atas pool db. now nil berarti
// time.Now.
func NewAuthenticator(db *sql.DB, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{db: db, now: now}
}

// hashDeviceToken mengembalikan SHA-256 hex dari token. Nilai inilah yang
// disimpan di devices.token_hash; token mentah tidak pernah disimpan.
func hashDeviceToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate mencari device pemilik token, memperbarui last_seen_at, dan
// mengembalikan identitasnya.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (httpserver.Device, error) {
	if a == nil || a.db == nil {
		return httpserver.Device{}, httpserver.ErrUnauthorized
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return httpserver.Device{}, httpserver.ErrUnauthorized
	}

	var (
		device httpserver.Device
		active int
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(note, ''), COALESCE(seat_number, ''), is_active FROM devices WHERE token_hash = ?`,
		hashDeviceToken(token),
	).Scan(&device.ID, &device.Note, &device.SeatNumber, &active)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return httpserver.Device{}, httpserver.ErrUnauthorized
	case err != nil:
		return httpserver.Device{}, fmt.Errorf("api: membaca device: %w", err)
	}

	if active == 0 {
		return httpserver.Device{}, httpserver.ErrDeviceDisabled
	}

	// Kegagalan memperbarui last_seen_at tidak boleh menggagalkan request yang
	// sudah terautentikasi; cukup dicatat agar bisa ditelusuri.
	if _, err := a.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`,
		a.now().UTC().Format(timeLayout), device.ID,
	); err != nil {
		slog.Default().Warn("api: gagal memperbarui last_seen_at", "device_id", device.ID, "err", err)
	}

	return device, nil
}

// registerRequest adalah body POST /devices/register.
//
// mac_address bukan hanya MAC: klien boleh mengirim device ID apa pun (mis.
// "android-a1b2-c3d4"), jadi nilainya tidak divalidasi formatnya dan tidak
// dinormalkan.
type registerRequest struct {
	MACAddress string `json:"mac_address"`
	SeatNumber string `json:"seat_number"`
	Note       string `json:"note"`
	AppVersion string `json:"app_version"`
	IsReset    bool   `json:"is_reset"`
}

// deviceSummary adalah bentuk device pada response registrasi (6.1).
type deviceSummary struct {
	ID         int64  `json:"id"`
	Note       any    `json:"note"`
	SeatNumber any    `json:"seat_number"`
	MACAddress string `json:"mac_address"`
}

// registerResponse adalah data sukses registrasi. Reset bernilai true bila
// baris perangkat yang sudah ada ditimpa (is_reset).
type registerResponse struct {
	Device deviceSummary `json:"device"`
	Token  string        `json:"token"`
	Reset  bool          `json:"reset"`
}

// registeredDevice adalah data pada 409/2003: perangkat yang sudah memakai
// identitas yang diminta klien, supaya klien bisa menampilkan kursi mana yang
// bentrok.
type registeredDevice struct {
	DeviceID   int64 `json:"device_id"`
	SeatNumber any   `json:"seat_number"`
	Note       any   `json:"note"`
}

// storedDevice adalah identitas minimal baris devices yang sudah ada.
type storedDevice struct {
	ID         int64
	Note       string
	SeatNumber string
	MACAddress string
}

// deviceDetail adalah bentuk GET /devices/me (6.2).
type deviceDetail struct {
	ID         int64  `json:"id"`
	Note       string `json:"note"`
	SeatNumber any    `json:"seat_number"`
	IsActive   bool   `json:"is_active"`
	LastSeenAt any    `json:"last_seen_at"`
	CreatedAt  any    `json:"created_at"`
}

// handleDeviceRegister melayani POST /devices/register (publik).
//
// Identitas perangkat (mac_address) boleh berupa MAC maupun device ID bebas;
// nilainya hanya di-trim, tidak divalidasi format dan tidak dinormalkan.
// Pencarian duplikat dilakukan case-insensitive.
//
// Perilaku:
//   - identitas sudah terdaftar dan is_reset=false → 409/2003 beserta data
//     kursi yang memakai identitas itu;
//   - identitas sudah terdaftar dan is_reset=true → baris yang sama ditimpa
//     (id tetap supaya playback_progress tidak hilang) dan dibalas 200;
//   - identitas baru → INSERT dan dibalas 201.
func (s *server) handleDeviceRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	mac := strings.TrimSpace(req.MACAddress)
	seatNumber := strings.TrimSpace(req.SeatNumber)
	note := strings.TrimSpace(req.Note)
	appVersion := strings.TrimSpace(req.AppVersion)

	fields := map[string]string{}
	switch {
	case mac == "":
		fields["body.mac_address"] = "wajib diisi"
	case utf8.RuneCountInString(mac) > macAddressMax:
		fields["body.mac_address"] = "maksimal 128 karakter"
	}
	switch {
	case seatNumber == "":
		fields["body.seat_number"] = "wajib diisi"
	case utf8.RuneCountInString(seatNumber) > seatNumberMax:
		fields["body.seat_number"] = "maksimal 20 karakter"
	}
	if utf8.RuneCountInString(note) > deviceNoteMax {
		fields["body.note"] = "maksimal 100 karakter"
	}
	if utf8.RuneCountInString(appVersion) > appVersionMax {
		fields["body.app_version"] = "maksimal 20 karakter"
	}
	if s.validationFailed(w, fields) {
		return
	}

	ctx := r.Context()
	existing, found, err := s.findDeviceByIdentity(ctx, mac)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	if found && !req.IsReset {
		response.ErrorData(w, response.CodeDeviceAlreadyRegistered, registeredDevice{
			DeviceID:   existing.ID,
			SeatNumber: nullText(existing.SeatNumber),
			Note:       nullText(existing.Note),
		}, nil)
		return
	}

	token, err := newDeviceToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	now := s.timestamp()

	// Identitas sudah ada dan klien minta reset: timpa baris yang sama tanpa
	// mengubah id, sehingga playback_progress perangkat tetap tersimpan.
	if found {
		if _, err := s.write.ExecContext(ctx,
			`UPDATE devices
			    SET note = ?, seat_number = ?, token_hash = ?, is_active = 1, last_seen_at = ?
			  WHERE id = ?`,
			nullIfBlank(note), nullIfBlank(seatNumber), hashDeviceToken(token), now, existing.ID,
		); err != nil {
			s.internalError(w, r, err)
			return
		}

		response.Success(w, registerResponse{
			Device: deviceSummary{
				ID:         existing.ID,
				Note:       nullText(note),
				SeatNumber: nullText(seatNumber),
				MACAddress: existing.MACAddress,
			},
			Token: token,
			Reset: true,
		})
		return
	}

	res, err := s.write.ExecContext(ctx,
		`INSERT INTO devices (note, seat_number, mac_address, token_hash, is_active, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		nullIfBlank(note), nullIfBlank(seatNumber), mac, hashDeviceToken(token), now, now,
	)
	if err != nil {
		// Race: baris lain menyisipkan identitas yang sama di antara SELECT dan
		// INSERT. Baca ulang lalu balas 409 dengan data kursinya, bukan 500.
		if isUniqueViolation(err, "mac_address") {
			existing, found, lerr := s.findDeviceByIdentity(ctx, mac)
			if lerr != nil {
				s.internalError(w, r, lerr)
				return
			}
			if found {
				response.ErrorData(w, response.CodeDeviceAlreadyRegistered, registeredDevice{
					DeviceID:   existing.ID,
					SeatNumber: nullText(existing.SeatNumber),
					Note:       nullText(existing.Note),
				}, nil)
				return
			}
		}
		s.internalError(w, r, err)
		return
	}

	id, err := res.LastInsertId()
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Created(w, registerResponse{
		Device: deviceSummary{
			ID:         id,
			Note:       nullText(note),
			SeatNumber: nullText(seatNumber),
			MACAddress: mac,
		},
		Token: token,
		Reset: false,
	})
}

// findDeviceByIdentity mencari device dengan identitas yang sama secara
// case-insensitive. Nilai balik kedua false berarti belum ada.
func (s *server) findDeviceByIdentity(ctx context.Context, mac string) (storedDevice, bool, error) {
	var device storedDevice
	err := s.write.QueryRowContext(ctx,
		`SELECT id, COALESCE(note, ''), COALESCE(seat_number, ''), COALESCE(mac_address, '')
		   FROM devices
		  WHERE mac_address = ? COLLATE NOCASE`,
		mac,
	).Scan(&device.ID, &device.Note, &device.SeatNumber, &device.MACAddress)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedDevice{}, false, nil
	case err != nil:
		return storedDevice{}, false, fmt.Errorf("api: mencari device: %w", err)
	}

	return device, true, nil
}

// handleDeviceMe melayani GET /devices/me.
func (s *server) handleDeviceMe(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	var (
		note       string
		seatNumber sql.NullString
		active     int
		lastSeen   sql.NullString
		createdAt  sql.NullString
	)
	err := s.read.QueryRowContext(r.Context(),
		`SELECT COALESCE(note, ''), seat_number, is_active, last_seen_at, created_at FROM devices WHERE id = ?`,
		device.ID,
	).Scan(&note, &seatNumber, &active, &lastSeen, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		response.Error(w, response.CodeDeviceNotFound, nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}

	response.Success(w, deviceDetail{
		ID:         device.ID,
		Note:       note,
		SeatNumber: nullString(seatNumber),
		IsActive:   active == 1,
		LastSeenAt: isoTime(lastSeen.String),
		CreatedAt:  isoTime(createdAt.String),
	})
}

// handleDeviceHeartbeat melayani POST /devices/me/heartbeat.
func (s *server) handleDeviceHeartbeat(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	if _, err := s.write.ExecContext(r.Context(),
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`,
		s.timestamp(), device.ID,
	); err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, nil)
}

// newDeviceToken membuat token "mc_" + base64url(32 byte acak) tanpa padding.
func newDeviceToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api: membuat token device: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// isUniqueViolation melaporkan apakah err adalah pelanggaran UNIQUE pada kolom
// tertentu. Pesan driver dipakai karena SQLite tidak memberi kode terstruktur.
func isUniqueViolation(err error, column string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unique constraint failed") {
		return false
	}
	return column == "" || strings.Contains(msg, strings.ToLower(column))
}
