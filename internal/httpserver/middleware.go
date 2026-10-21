package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"vodlocal/internal/response"
)

// Device adalah identitas perangkat (tablet kursi) yang sudah terautentikasi.
// Nilainya disimpan di context oleh RequireDevice dan dibaca kembali dengan
// DeviceFrom.
type Device struct {
	ID         int64
	Name       string
	SeatNumber string
}

// DeviceAuthenticator memverifikasi bearer token device dan mengembalikan
// perangkat pemiliknya.
//
// Implementasi mengembalikan ErrUnauthorized bila token tidak dikenal,
// ErrDeviceDisabled bila perangkat dinonaktifkan, dan error lain untuk
// kegagalan internal (mis. database tidak bisa dibaca).
type DeviceAuthenticator interface {
	Authenticate(ctx context.Context, token string) (Device, error)
}

var (
	// ErrUnauthorized menandai token device yang tidak dikenal atau sudah
	// dirotasi.
	ErrUnauthorized = errors.New("httpserver: unauthorized")
	// ErrDeviceDisabled menandai perangkat yang tokennya valid tetapi
	// dinonaktifkan (devices.is_active = 0).
	ErrDeviceDisabled = errors.New("httpserver: device disabled")
)

// ctxKey adalah tipe kunci privat untuk nilai Device di context request.
// Tipe kosong ini memastikan tidak ada paket lain yang bisa menimpa nilainya.
type ctxKey struct{}

// DeviceFrom mengambil Device yang dipasang RequireDevice pada context.
func DeviceFrom(ctx context.Context) (Device, bool) {
	if ctx == nil {
		return Device{}, false
	}
	device, ok := ctx.Value(ctxKey{}).(Device)
	if !ok {
		return Device{}, false
	}
	return device, true
}

// RequireDevice mengautentikasi bearer token device sebelum meneruskan request
// ke handler berikutnya.
//
// Header yang hilang atau tidak berformat "Bearer <token>" dijawab
// 401/CodeUnauthorized, token milik perangkat nonaktif dijawab
// 403/CodeDeviceDisabled, dan kegagalan lain dijawab 500/CodeInternalError.
// Semua kegagalan memakai envelope standar (docs/api-spec.md §2).
func RequireDevice(a DeviceAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				response.Error(w, response.CodeUnauthorized, nil)
				return
			}
			if a == nil {
				response.Error(w, response.CodeInternalError, nil)
				return
			}
			device, err := a.Authenticate(r.Context(), token)
			if err != nil {
				switch {
				case errors.Is(err, ErrDeviceDisabled):
					response.Error(w, response.CodeDeviceDisabled, nil)
				case errors.Is(err, ErrUnauthorized):
					response.Error(w, response.CodeUnauthorized, nil)
				default:
					response.Error(w, response.CodeInternalError, nil)
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, device)))
		})
	}
}

// bearerToken mengekstrak token dari nilai header Authorization.
//
// Skema dibandingkan tanpa peduli huruf besar/kecil (RFC 7235) dan token tidak
// boleh kosong atau mengandung spasi, sehingga "Bearer" tanpa token dan
// "Bearer a b" sama-sama ditolak.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
