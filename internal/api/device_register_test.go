package api_test

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"vodlocal/internal/response"
)

// registerData adalah data respons POST /devices/register yang dipakai test
// registrasi perangkat.
type registerData struct {
	Device struct {
		ID         int64   `json:"id"`
		Note       *string `json:"note"`
		SeatNumber *string `json:"seat_number"`
		MACAddress string  `json:"mac_address"`
	} `json:"device"`
	Token string `json:"token"`
	Reset bool   `json:"reset"`
}

// conflictData adalah data respons 409/2003 (perangkat pemakai identitas).
type conflictData struct {
	DeviceID   int64   `json:"device_id"`
	SeatNumber *string `json:"seat_number"`
	Note       *string `json:"note"`
}

// registerOK mendaftarkan perangkat dan memastikan respons 201 + code 0.
func (f *fixture) registerOK(body map[string]any) registerData {
	f.t.Helper()

	env, rec := f.do(http.MethodPost, "/devices/register", "", body)
	if rec.Code != http.StatusCreated || env.Code != response.CodeSuccess {
		f.t.Fatalf("register: status/code = %d/%d, mau 201/0 (body=%s)",
			rec.Code, env.Code, rec.Body.String())
	}

	var data registerData
	f.decode(env, &data)
	if data.Token == "" {
		f.t.Fatalf("register: token kosong")
	}
	if data.Device.ID < 1 {
		f.t.Fatalf("register: device.id = %d, mau > 0", data.Device.ID)
	}

	return data
}

// TestDeviceRegisterFreeFormIdentity memastikan identitas perangkat berupa
// device ID bebas (bukan MAC) diterima apa adanya tanpa normalisasi.
func TestDeviceRegisterFreeFormIdentity(t *testing.T) {
	f := newFixture(t, nil)

	const identity = "android-a1b2-c3d4"
	data := f.registerOK(map[string]any{
		"mac_address": identity,
		"seat_number": "4",
		"note":        "Tablet Android",
		"app_version": "2.1.0",
	})

	if data.Device.MACAddress != identity {
		t.Fatalf("device.mac_address = %q, mau %q apa adanya", data.Device.MACAddress, identity)
	}
	if data.Reset {
		t.Fatal("reset = true pada registrasi baru, mau false")
	}

	var stored string
	if err := f.db.QueryRow(`SELECT mac_address FROM devices WHERE id = ?`, data.Device.ID).Scan(&stored); err != nil {
		t.Fatalf("baca mac_address: %v", err)
	}
	if stored != identity {
		t.Fatalf("mac_address tersimpan = %q, mau %q (tanpa normalisasi)", stored, identity)
	}

	// Batas panjang identitas: 128 karakter masih diterima.
	long128 := strings.Repeat("a", 128)
	data = f.registerOK(map[string]any{
		"mac_address": long128,
		"seat_number": "5",
	})
	if data.Device.MACAddress != long128 {
		t.Fatalf("mac_address = %q, mau identitas 128 karakter", data.Device.MACAddress)
	}
}

// TestDeviceRegisterDuplicateWithoutReset memastikan identitas duplikat ditolak
// 409/2003 beserta data kursi yang sudah memakai identitas itu.
func TestDeviceRegisterDuplicateWithoutReset(t *testing.T) {
	f := newFixture(t, nil)

	first := f.registerOK(map[string]any{
		"mac_address": "android-bentrok-01",
		"seat_number": "3",
		"note":        "Tablet Kursi 3",
	})

	// Kapitalisasi berbeda harus tetap dianggap identitas yang sama.
	env, rec := f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"mac_address": "ANDROID-Bentrok-01",
		"seat_number": "8",
		"note":        "Tablet Lain",
	})
	if rec.Code != http.StatusConflict || env.Code != response.CodeDeviceAlreadyRegistered {
		t.Fatalf("duplikat: status/code = %d/%d, mau 409/%d (body=%s)",
			rec.Code, env.Code, response.CodeDeviceAlreadyRegistered, rec.Body.String())
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors = %+v, mau kosong", env.Errors)
	}

	var conflict conflictData
	f.decode(env, &conflict)
	if conflict.DeviceID != first.Device.ID {
		t.Fatalf("data.device_id = %d, mau %d", conflict.DeviceID, first.Device.ID)
	}
	if conflict.SeatNumber == nil || *conflict.SeatNumber != "3" {
		t.Fatalf("data.seat_number = %v, mau \"3\"", conflict.SeatNumber)
	}
	if conflict.Note == nil || *conflict.Note != "Tablet Kursi 3" {
		t.Fatalf("data.name = %v, mau \"Tablet Kursi 3\"", conflict.Note)
	}

	// Penolakan tidak boleh menambah baris perangkat.
	var rows int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&rows); err != nil {
		t.Fatalf("hitung devices: %v", err)
	}
	if rows != 1 {
		t.Fatalf("baris devices = %d, mau 1", rows)
	}
}

// TestDeviceRegisterDuplicateWithReset memastikan is_reset=true menimpa baris
// yang sama (id tetap, token baru) dan playback_progress tidak hilang.
func TestDeviceRegisterDuplicateWithReset(t *testing.T) {
	f := newFixture(t, nil)

	const identity = "android-reset-01"
	first := f.registerOK(map[string]any{
		"mac_address": identity,
		"seat_number": "3",
		"note":        "Tablet Lama",
	})

	// Progres tonton yang wajib bertahan setelah reset.
	env, rec := f.do(http.MethodPost, "/progress", first.Token, map[string]any{
		"playable_id":      9001,
		"position_seconds": 120,
		"duration_seconds": 2700,
	})
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("POST /progress: status/code = %d/%d, mau 200/0 (body=%s)",
			rec.Code, env.Code, rec.Body.String())
	}

	// Perangkat dinonaktifkan admin; reset harus mengaktifkannya kembali.
	f.exec(`UPDATE devices SET is_active = 0 WHERE id = ?`, first.Device.ID)

	env, rec = f.do(http.MethodPost, "/devices/register", "", map[string]any{
		"mac_address": identity,
		"seat_number": "9",
		"note":        "Tablet Baru",
		"is_reset":    true,
	})
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("reset: status/code = %d/%d, mau 200/0 (body=%s)",
			rec.Code, env.Code, rec.Body.String())
	}

	var second registerData
	f.decode(env, &second)
	if !second.Reset {
		t.Fatal("reset = false, mau true")
	}
	if second.Device.ID != first.Device.ID {
		t.Fatalf("device.id = %d setelah reset, mau tetap %d", second.Device.ID, first.Device.ID)
	}
	if second.Token == first.Token {
		t.Fatal("token tidak berubah setelah reset")
	}
	if second.Device.SeatNumber == nil || *second.Device.SeatNumber != "9" {
		t.Fatalf("device.seat_number = %v, mau \"9\"", second.Device.SeatNumber)
	}
	if second.Device.Note == nil || *second.Device.Note != "Tablet Baru" {
		t.Fatalf("device.name = %v, mau \"Tablet Baru\"", second.Device.Note)
	}

	// Tetap satu baris perangkat, dan is_active kembali 1.
	var rows, active int
	if err := f.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(is_active), 0) FROM devices`).Scan(&rows, &active); err != nil {
		t.Fatalf("baca devices: %v", err)
	}
	if rows != 1 {
		t.Fatalf("baris devices = %d, mau 1 (baris yang sama ditimpa)", rows)
	}
	if active != 1 {
		t.Fatalf("is_active = %d, mau 1 setelah reset", active)
	}

	// Progres tonton milik device itu tidak boleh hilang.
	var progress int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM playback_progress WHERE device_id = ?`, first.Device.ID).Scan(&progress); err != nil {
		t.Fatalf("hitung progress: %v", err)
	}
	if progress != 1 {
		t.Fatalf("baris progress = %d, mau 1 (tidak hilang setelah reset)", progress)
	}

	// Token lama tidak berlaku lagi, token baru berlaku.
	env, rec = f.do(http.MethodGet, "/devices/me", first.Token, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != response.CodeUnauthorized {
		t.Fatalf("token lama: status/code = %d/%d, mau 401/2000 (body=%s)",
			rec.Code, env.Code, rec.Body.String())
	}
	env, rec = f.do(http.MethodGet, "/devices/me", second.Token, nil)
	if rec.Code != http.StatusOK || env.Code != response.CodeSuccess {
		t.Fatalf("token baru: status/code = %d/%d, mau 200/0 (body=%s)",
			rec.Code, env.Code, rec.Body.String())
	}
}

// TestDeviceRegisterOptionalName memastikan name boleh kosong: respons berisi
// name null dan kolom devices.name tersimpan NULL.
func TestDeviceRegisterOptionalName(t *testing.T) {
	f := newFixture(t, nil)

	for _, body := range []map[string]any{
		{"mac_address": "android-tanpa-nama", "seat_number": "6"},
		{"mac_address": "android-nama-spasi", "seat_number": "6", "note": "   "},
	} {
		data := f.registerOK(body)
		if data.Device.Note != nil {
			t.Fatalf("device.name = %v, mau null", *data.Device.Note)
		}

		var stored sql.NullString
		if err := f.db.QueryRow(`SELECT note FROM devices WHERE id = ?`, data.Device.ID).Scan(&stored); err != nil {
			t.Fatalf("baca name: %v", err)
		}
		if stored.Valid {
			t.Fatalf("name tersimpan = %q, mau NULL", stored.String)
		}
	}
}

// TestDeviceRegisterSeatNumberRequired memastikan seat_number wajib diisi.
func TestDeviceRegisterSeatNumberRequired(t *testing.T) {
	f := newFixture(t, nil)

	for _, seat := range []string{"", "   "} {
		env, rec := f.do(http.MethodPost, "/devices/register", "", map[string]any{
			"mac_address": "android-tanpa-kursi",
			"seat_number": seat,
		})
		if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeValidationError {
			t.Fatalf("seat=%q: status/code = %d/%d, mau 422/1003 (body=%s)",
				seat, rec.Code, env.Code, rec.Body.String())
		}

		var found bool
		for _, detail := range env.Errors {
			if detail.Field == "body.seat_number" && detail.Message == "wajib diisi" {
				found = true
			}
		}
		if !found {
			t.Fatalf("seat=%q: errors = %+v, mau body.seat_number \"wajib diisi\"", seat, env.Errors)
		}
	}

	// Tidak ada baris yang tercipta dari request tidak valid.
	var rows int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&rows); err != nil {
		t.Fatalf("hitung devices: %v", err)
	}
	if rows != 0 {
		t.Fatalf("baris devices = %d, mau 0", rows)
	}
}

// TestDeviceRegisterValidationLimits memeriksa pesan validasi batas panjang
// setiap field registrasi.
func TestDeviceRegisterValidationLimits(t *testing.T) {
	f := newFixture(t, nil)

	cases := []struct {
		name  string
		body  map[string]any
		field string
		pesan string
	}{
		{
			name:  "mac_address terlalu panjang",
			body:  map[string]any{"mac_address": strings.Repeat("a", 129), "seat_number": "1"},
			field: "body.mac_address",
			pesan: "maksimal 128 karakter",
		},
		{
			name:  "seat_number terlalu panjang",
			body:  map[string]any{"mac_address": "android-panjang", "seat_number": strings.Repeat("b", 21)},
			field: "body.seat_number",
			pesan: "maksimal 20 karakter",
		},
		{
			name:  "name terlalu panjang",
			body:  map[string]any{"mac_address": "android-panjang", "seat_number": "1", "note": strings.Repeat("c", 101)},
			field: "body.note",
			pesan: "maksimal 100 karakter",
		},
		{
			name:  "app_version terlalu panjang",
			body:  map[string]any{"mac_address": "android-panjang", "seat_number": "1", "app_version": strings.Repeat("d", 21)},
			field: "body.app_version",
			pesan: "maksimal 20 karakter",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, rec := f.do(http.MethodPost, "/devices/register", "", tc.body)
			if rec.Code != http.StatusUnprocessableEntity || env.Code != response.CodeValidationError {
				t.Fatalf("status/code = %d/%d, mau 422/1003 (body=%s)",
					rec.Code, env.Code, rec.Body.String())
			}

			var found bool
			for _, detail := range env.Errors {
				if detail.Field == tc.field && detail.Message == tc.pesan {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %+v, mau %s \"%s\"", env.Errors, tc.field, tc.pesan)
			}
		})
	}
}
