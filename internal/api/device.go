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
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

const (
	// tokenPrefix menandai token device agar mudah dikenali di log/dukungan.
	tokenPrefix = "mc_"
	// tokenRandomBytes adalah jumlah byte acak di dalam token (§6.1).
	tokenRandomBytes = 32

	// Batas panjang field registrasi (§6.1).
	deviceNameMax = 100
	seatNumberMax = 20
	appVersionMax = 20
)

// macPattern menerima MAC yang sudah dinormalkan ke bentuk AA:BB:CC:DD:EE:FF.
var macPattern = regexp.MustCompile(`^[0-9A-F]{2}(?::[0-9A-F]{2}){5}$`)

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
		`SELECT id, name, COALESCE(seat_number, ''), is_active FROM devices WHERE token_hash = ?`,
		hashDeviceToken(token),
	).Scan(&device.ID, &device.Name, &device.SeatNumber, &active)
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
type registerRequest struct {
	Name       string `json:"name"`
	SeatNumber string `json:"seat_number"`
	MACAddress string `json:"mac_address"`
	AppVersion string `json:"app_version"`
}

// deviceSummary adalah bentuk device pada response registrasi (§6.1).
type deviceSummary struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	SeatNumber any    `json:"seat_number"`
}

// registerResponse adalah data sukses registrasi.
type registerResponse struct {
	Device deviceSummary `json:"device"`
	Token  string        `json:"token"`
}

// deviceDetail adalah bentuk GET /devices/me (§6.2).
type deviceDetail struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	SeatNumber any    `json:"seat_number"`
	IsActive   bool   `json:"is_active"`
	LastSeenAt any    `json:"last_seen_at"`
	CreatedAt  any    `json:"created_at"`
}

// handleDeviceRegister melayani POST /devices/register (publik).
func (s *server) handleDeviceRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	seatNumber := strings.TrimSpace(req.SeatNumber)
	appVersion := strings.TrimSpace(req.AppVersion)
	mac := normalizeMAC(req.MACAddress)

	fields := map[string]string{}
	switch {
	case name == "":
		fields["body.name"] = "wajib diisi"
	case utf8.RuneCountInString(name) > deviceNameMax:
		fields["body.name"] = "maksimal 100 karakter"
	}
	if utf8.RuneCountInString(seatNumber) > seatNumberMax {
		fields["body.seat_number"] = "maksimal 20 karakter"
	}
	if utf8.RuneCountInString(appVersion) > appVersionMax {
		fields["body.app_version"] = "maksimal 20 karakter"
	}
	if strings.TrimSpace(req.MACAddress) != "" && !macPattern.MatchString(mac) {
		fields["body.mac_address"] = "format MAC tidak valid"
	}
	if s.validationFailed(w, fields) {
		return
	}

	ctx := r.Context()
	if mac != "" {
		var exists int
		err := s.write.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE mac_address = ?`, mac).Scan(&exists)
		switch {
		case err == nil:
			response.Error(w, response.CodeDeviceAlreadyRegistered, nil)
			return
		case !errors.Is(err, sql.ErrNoRows):
			s.internalError(w, r, err)
			return
		}
	}

	token, err := newDeviceToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	now := s.timestamp()
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO devices (name, seat_number, mac_address, token_hash, is_active, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		name, nullIfBlank(seatNumber), nullIfBlank(mac), hashDeviceToken(token), now, now,
	)
	if err != nil {
		if isUniqueViolation(err, "mac_address") {
			response.Error(w, response.CodeDeviceAlreadyRegistered, nil)
			return
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
		Device: deviceSummary{ID: id, Name: name, SeatNumber: nullText(seatNumber)},
		Token:  token,
	})
}

// handleDeviceMe melayani GET /devices/me.
func (s *server) handleDeviceMe(w http.ResponseWriter, r *http.Request) {
	device, ok := httpserver.DeviceFrom(r.Context())
	if !ok {
		response.Error(w, response.CodeUnauthorized, nil)
		return
	}

	var (
		name       string
		seatNumber sql.NullString
		active     int
		lastSeen   sql.NullString
		createdAt  sql.NullString
	)
	err := s.read.QueryRowContext(r.Context(),
		`SELECT name, seat_number, is_active, last_seen_at, created_at FROM devices WHERE id = ?`,
		device.ID,
	).Scan(&name, &seatNumber, &active, &lastSeen, &createdAt)
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
		Name:       name,
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

// normalizeMAC menormalkan MAC ke huruf besar dengan pemisah ":" dan menerima
// pemisah "-" maupun "." yang umum dikirim klien. Input kosong menghasilkan
// string kosong.
func normalizeMAC(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.NewReplacer(":", "", "-", "", ".", "", " ", "").Replace(raw)
	if len(raw) != 12 {
		return strings.ToUpper(raw)
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, strings.ToUpper(raw[i:i+2]))
	}
	return strings.Join(parts, ":")
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
