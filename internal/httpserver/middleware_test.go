package httpserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vodlocal/internal/httpserver"
	"vodlocal/internal/response"
)

// stubDevices adalah DeviceAuthenticator tiruan berbasis map token → device.
type stubDevices struct {
	devices   map[string]httpserver.Device
	err       error
	lastToken string
}

func (s *stubDevices) Authenticate(ctx context.Context, token string) (httpserver.Device, error) {
	s.lastToken = token
	if s.err != nil {
		return httpserver.Device{}, s.err
	}
	device, ok := s.devices[token]
	if !ok {
		return httpserver.Device{}, httpserver.ErrUnauthorized
	}
	return device, nil
}

func TestRequireDeviceRejectsMissingHeader(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler tidak boleh dipanggil")
	})
	handler := httpserver.RequireDevice(&stubDevices{})(next)

	tests := []struct {
		name   string
		header string
	}{
		{"tanpa header", ""},
		{"skema salah", "Basic abc"},
		{"tanpa token", "Bearer"},
		{"token kosong", "Bearer   "},
		{"token berspasi", "Bearer a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/me", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			body := decodeEnvelope(t, rec)
			if body.Code != response.CodeUnauthorized {
				t.Errorf("code = %d, want %d", body.Code, response.CodeUnauthorized)
			}
			if body.Msg != response.Label(response.CodeUnauthorized) {
				t.Errorf("msg = %q, want %q", body.Msg, response.Label(response.CodeUnauthorized))
			}
			if body.Errors == nil {
				t.Error("errors tidak boleh null")
			}
		})
	}
}

func TestRequireDevicePassesDevice(t *testing.T) {
	devices := &stubDevices{devices: map[string]httpserver.Device{
		"mc_valid": {ID: 12, Note: "Tablet Kursi 12", SeatNumber: "12A"},
	}}
	var got httpserver.Device
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device, ok := httpserver.DeviceFrom(r.Context())
		if !ok {
			t.Fatal("DeviceFrom tidak menemukan device di context")
		}
		got = device
		w.WriteHeader(http.StatusNoContent)
	})
	handler := httpserver.RequireDevice(devices)(next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/me", nil)
	req.Header.Set("Authorization", "Bearer mc_valid")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	want := httpserver.Device{ID: 12, Note: "Tablet Kursi 12", SeatNumber: "12A"}
	if got != want {
		t.Errorf("device = %+v, want %+v", got, want)
	}
	if devices.lastToken != "mc_valid" {
		t.Errorf("token = %q, want %q", devices.lastToken, "mc_valid")
	}
}

func TestRequireDeviceStatusMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHTTP int
		wantCode int
	}{
		{"disabled", httpserver.ErrDeviceDisabled, http.StatusForbidden, response.CodeDeviceDisabled},
		{"wrapped disabled", errors.Join(errors.New("db"), httpserver.ErrDeviceDisabled), http.StatusForbidden, response.CodeDeviceDisabled},
		{"unauthorized", httpserver.ErrUnauthorized, http.StatusUnauthorized, response.CodeUnauthorized},
		{"internal", errors.New("db down"), http.StatusInternalServerError, response.CodeInternalError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := httpserver.RequireDevice(&stubDevices{err: tc.err})(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { t.Fatal("handler tidak boleh dipanggil") },
			))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/me", nil)
			req.Header.Set("Authorization", "Bearer mc_x")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantHTTP {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantHTTP)
			}
			if body := decodeEnvelope(t, rec); body.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", body.Code, tc.wantCode)
			}
		})
	}
}

func TestDeviceFromEmptyContext(t *testing.T) {
	if _, ok := httpserver.DeviceFrom(context.Background()); ok {
		t.Fatal("DeviceFrom harus false untuk context kosong")
	}
	//nolint:staticcheck // context nil harus aman, bukan panic.
	if _, ok := httpserver.DeviceFrom(nil); ok {
		t.Fatal("DeviceFrom harus false untuk context nil")
	}
}

func TestSessionCreateGetRoundtrip(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)

	rec := httptest.NewRecorder()
	if err := manager.Create(rec, "admin", "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cookie := cookieNamed(t, rec.Result().Cookies(), httpserver.SessionCookieName)

	if !cookie.HttpOnly {
		t.Error("cookie sesi harus HttpOnly")
	}
	if got, want := cookie.SameSite, http.SameSiteLaxMode; got != want {
		t.Errorf("SameSite = %v, want %v", got, want)
	}
	if got, want := cookie.Path, "/"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookie)
	session, ok := manager.Get(req)
	if !ok {
		t.Fatal("Get harus true untuk cookie yang valid")
	}
	if session.Username != "admin" || session.Role != "admin" {
		t.Errorf("session = %+v, want admin/admin", session)
	}
}

func TestSessionGetRejectsBrokenCookie(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	other := httpserver.NewSessionManager([]byte("secret-lain"), time.Hour)

	rec := httptest.NewRecorder()
	if err := manager.Create(rec, "admin", "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	valid := cookieNamed(t, rec.Result().Cookies(), httpserver.SessionCookieName).Value
	payload, _, _ := strings.Cut(valid, ".")

	recOther := httptest.NewRecorder()
	if err := other.Create(recOther, "penyusup", "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	foreign := cookieNamed(t, recOther.Result().Cookies(), httpserver.SessionCookieName).Value

	tests := []struct {
		name  string
		value string
	}{
		{"kosong", ""},
		{"sampah", "bukan-cookie-sesi"},
		{"tanpa tanda tangan", payload},
		{"tanda tangan rusak", payload + ".AAAA"},
		{"payload diubah", "YWRtaW58YWRtaW58NDA3MDkwODgwMA.AAAA"},
		{"secret lain", foreign},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin", nil)
			if tc.value != "" {
				req.AddCookie(&http.Cookie{Name: httpserver.SessionCookieName, Value: tc.value})
			}
			if session, ok := manager.Get(req); ok {
				t.Fatalf("Get harus false, dapat %+v", session)
			}
		})
	}

	if _, ok := manager.Get(nil); ok {
		t.Fatal("Get(nil) harus false")
	}
}

func TestSessionExpired(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), -time.Minute)
	rec := httptest.NewRecorder()
	if err := manager.Create(rec, "admin", "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookieNamed(t, rec.Result().Cookies(), httpserver.SessionCookieName))
	if _, ok := manager.Get(req); ok {
		t.Fatal("session kedaluwarsa harus ditolak")
	}
}

func TestSessionCreateRejectsSeparator(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	rec := httptest.NewRecorder()
	if err := manager.Create(rec, "admin|super", "admin"); !errors.Is(err, httpserver.ErrInvalidSessionField) {
		t.Fatalf("err = %v, want ErrInvalidSessionField", err)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("cookie tidak boleh dikirim saat Create gagal: %v", cookies)
	}
}

func TestSessionDestroy(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	rec := httptest.NewRecorder()
	manager.Destroy(rec)

	cookie := cookieNamed(t, rec.Result().Cookies(), httpserver.SessionCookieName)
	if cookie.Value != "" {
		t.Errorf("Value = %q, want kosong", cookie.Value)
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negatif", cookie.MaxAge)
	}
	if !cookie.Expires.Before(time.Now()) {
		t.Errorf("Expires = %v, want di masa lalu", cookie.Expires)
	}
}

func TestRequireAdminJSON(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	handler := httpserver.RequireAdmin(manager)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { t.Fatal("handler tidak boleh dipanggil") },
	))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/titles", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != response.CodeSessionExpired {
		t.Errorf("code = %d, want %d", body.Code, response.CodeSessionExpired)
	}
}

func TestRequireAdminRedirectsBrowser(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	handler := httpserver.RequireAdmin(manager)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { t.Fatal("handler tidak boleh dipanggil") },
	))

	req := httptest.NewRequest(http.MethodGet, "/admin/titles", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "/login"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

func TestRequireAdminAllowsSession(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	rec := httptest.NewRecorder()
	if err := manager.Create(rec, "operator", "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cookie := cookieNamed(t, rec.Result().Cookies(), httpserver.SessionCookieName)

	called := false
	handler := httpserver.RequireAdmin(manager)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/admin/titles", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(cookie)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)

	if !called {
		t.Fatal("handler harus dipanggil untuk sesi valid")
	}
	if out.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", out.Code)
	}
}

func TestCSRF(t *testing.T) {
	manager := httpserver.NewSessionManager([]byte("rahasia-uji"), time.Hour)
	called := false
	handler := httpserver.CSRF(manager)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	// GET aman: lolos dan menerbitkan cookie mc_csrf.
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/admin/titles/new", nil))
	if !called {
		t.Fatal("GET harus lolos middleware CSRF")
	}
	if getRec.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d, want 204", getRec.Code)
	}
	csrfCookie := cookieNamed(t, getRec.Result().Cookies(), httpserver.CSRFCookieName)
	if len(csrfCookie.Value) < 16 {
		t.Fatalf("token CSRF terlalu pendek: %q", csrfCookie.Value)
	}

	t.Run("POST tanpa token", func(t *testing.T) {
		called = false
		req := formRequest(http.MethodPost, "/admin/titles", url.Values{"title": {"Contoh"}})
		req.AddCookie(csrfCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if called {
			t.Fatal("handler tidak boleh dipanggil")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if body := decodeEnvelope(t, rec); body.Code != response.CodeCSRFInvalid {
			t.Errorf("code = %d, want %d", body.Code, response.CodeCSRFInvalid)
		}
	})

	t.Run("POST token salah", func(t *testing.T) {
		called = false
		req := formRequest(http.MethodPost, "/admin/titles", url.Values{"_csrf": {"salah"}})
		req.AddCookie(csrfCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if called {
			t.Fatal("handler tidak boleh dipanggil")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("POST token cocok", func(t *testing.T) {
		called = false
		req := formRequest(http.MethodPost, "/admin/titles", url.Values{"_csrf": {csrfCookie.Value}})
		req.AddCookie(csrfCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if !called {
			t.Fatal("handler harus dipanggil dengan token yang cocok")
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})

	t.Run("token dicetak ulang saat cookie belum ada", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
		if got := rec.Result().Cookies(); len(got) == 0 {
			t.Fatal("cookie mc_csrf harus diterbitkan")
		}
	})
}

// formRequest membuat request dengan body form ter-encode.
func formRequest(method, target string, values url.Values) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
