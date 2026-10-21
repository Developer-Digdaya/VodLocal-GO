package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vodlocal/internal/response"
)

const (
	// SessionCookieName adalah cookie sesi admin (docs/api-spec.md §2).
	SessionCookieName = "mc_session"
	// CSRFCookieName adalah cookie double-submit token CSRF untuk form admin.
	CSRFCookieName = "mc_csrf"
	// sessionSep memisahkan field payload. Username dan role tidak boleh
	// memuat karakter ini.
	sessionSep = "|"
	// csrfTokenBytes adalah panjang token CSRF dalam byte (32 byte acak).
	csrfTokenBytes = 32
	// sessionParts adalah jumlah field payload: username, role, unix_exp.
	sessionParts = 3
)

// ErrInvalidSessionField dikembalikan Create bila username atau role memuat
// pemisah "|" yang akan merusak payload cookie.
var ErrInvalidSessionField = errors.New(`httpserver: session field contains the reserved "|" separator`)

// Session adalah identitas admin yang sedang login.
type Session struct {
	Username string
	Role     string
}

// SessionManager membuat, membaca, dan menghapus cookie sesi bertanda tangan
// HMAC-SHA256.
//
// Nilai cookie berbentuk "base64(payload).base64(hmac-sha256(payload))" dengan
// payload "username|role|unix_exp". Karena payload ditandatangani, klien tidak
// bisa mengubah username, role, atau masa berlaku tanpa membuat tanda tangan
// yang salah. Secret yang lemah membatalkan jaminan itu, jadi secret harus
// diisi dari konfigurasi acak dan tidak dibagikan.
type SessionManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewSessionManager membuat SessionManager dengan secret penandatangan dan
// umur cookie ttl. Secret disalin sehingga pemanggil bebas mengubah slice-nya.
//
// ttl dipakai apa adanya: nilai <= 0 menghasilkan cookie yang langsung
// kedaluwarsa (berguna untuk test), bukan nilai default diam-diam.
func NewSessionManager(secret []byte, ttl time.Duration) *SessionManager {
	key := make([]byte, len(secret))
	copy(key, secret)
	return &SessionManager{secret: key, ttl: ttl, now: time.Now}
}

// Create menandatangani sesi untuk username/role dan mengirimnya sebagai
// cookie mc_session. Cookie bersifat HttpOnly, SameSite=Lax, dan Path=/.
func (m *SessionManager) Create(w http.ResponseWriter, username, role string) error {
	if strings.Contains(username, sessionSep) || strings.Contains(role, sessionSep) {
		return ErrInvalidSessionField
	}
	now := m.clock()
	expires := now.Add(m.ttl)
	payload := username + sessionSep + role + sessionSep + strconv.FormatInt(expires.Unix(), 10)
	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    m.sign(payload),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if m.ttl > 0 {
		cookie.Expires = expires
		cookie.MaxAge = int(m.ttl / time.Second)
	}
	http.SetCookie(w, cookie)
	return nil
}

// Get membaca cookie sesi dari r. Nilai balik kedua false bila cookie tidak
// ada, rusak, tanda tangannya tidak cocok, atau sudah kedaluwarsa.
func (m *SessionManager) Get(r *http.Request) (Session, bool) {
	if r == nil {
		return Session{}, false
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, false
	}
	payload, ok := m.verify(cookie.Value)
	if !ok {
		return Session{}, false
	}
	parts := strings.Split(payload, sessionSep)
	if len(parts) != sessionParts {
		return Session{}, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Session{}, false
	}
	if m.clock().Unix() >= exp {
		return Session{}, false
	}
	return Session{Username: parts[0], Role: parts[1]}, true
}

// Destroy mengirim ulang cookie sesi sebagai kedaluwarsa sehingga browser
// menghapusnya.
func (m *SessionManager) Destroy(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// EnsureCSRFCookie memastikan request memiliki token CSRF dan mengembalikan
// nilainya.
//
// Bila cookie mc_csrf sudah ada, nilainya dipakai apa adanya. Bila belum, token
// acak 32 byte dibuat dan dikirim sebagai cookie baru, sehingga halaman form
// bisa memakai nilai baliknya sebagai isi field _csrf.
func (m *SessionManager) EnsureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if r != nil {
		if cookie, err := r.Cookie(CSRFCookieName); err == nil && cookie.Value != "" {
			return cookie.Value
		}
	}
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	cookie := &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if m != nil && m.ttl > 0 {
		cookie.Expires = m.clock().Add(m.ttl)
		cookie.MaxAge = int(m.ttl / time.Second)
	}
	http.SetCookie(w, cookie)
	return token
}

// RequireAdmin menolak request tanpa sesi admin yang valid.
//
// Request yang mengharapkan JSON (Accept atau Content-Type memuat
// application/json) mendapat 401/CodeSessionExpired dalam envelope. Request
// lain, yaitu navigasi browser ke halaman admin, dialihkan 303 ke /login.
func RequireAdmin(s *SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s != nil {
				if _, ok := s.Get(r); ok {
					next.ServeHTTP(w, r)
					return
				}
			}
			if wantsJSON(r) {
				response.Error(w, response.CodeSessionExpired, nil)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		})
	}
}

// CSRF memeriksa token double-submit untuk setiap request yang mengubah state.
//
// Method aman (GET, HEAD, OPTIONS) hanya memastikan cookie mc_csrf tersedia.
// Method lain wajib mengirim field form _csrf yang nilainya sama dengan cookie
// tersebut; bila tidak, request dijawab 403/CodeCSRFInvalid.
func CSRF(s *SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			expected := ""
			if s != nil {
				expected = s.EnsureCSRFCookie(w, r)
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			got := r.FormValue("_csrf")
			if expected == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
				response.Error(w, response.CodeCSRFInvalid, nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clock mengembalikan waktu sekarang, memakai time.Now bila tidak diset.
func (m *SessionManager) clock() time.Time {
	if m == nil || m.now == nil {
		return time.Now()
	}
	return m.now()
}

// sign mengembalikan "base64url(payload).base64url(hmac-sha256(payload))".
func (m *SessionManager) sign(payload string) string {
	raw := []byte(payload)
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify memeriksa tanda tangan value dan mengembalikan payload aslinya.
// Perbandingan tanda tangan memakai ConstantTimeCompare agar tidak membocorkan
// informasi lewat waktu eksekusi.
func (m *SessionManager) verify(value string) (string, bool) {
	encodedPayload, encodedMAC, found := strings.Cut(value, ".")
	if !found {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return "", false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(gotMAC, mac.Sum(nil)) != 1 {
		return "", false
	}
	return string(payload), true
}

// wantsJSON melaporkan apakah request mengharapkan body JSON.
func wantsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	return containsJSON(r.Header.Get("Accept")) || containsJSON(r.Header.Get("Content-Type"))
}

// containsJSON melaporkan apakah nilai header memuat media type JSON.
func containsJSON(header string) bool {
	return strings.Contains(strings.ToLower(header), "application/json")
}
