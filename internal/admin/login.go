package admin

import (
	"net/http"
	"strings"
)

// loginData adalah data halaman login.
type loginData struct {
	base
	Username string
}

// handleLoginForm melayani GET /login.
func (a *Admin) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessions.Get(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderPage(w, r, "login", loginData{
		base: a.base(w, r, "Masuk", "login"),
	}, http.StatusOK)
}

// handleLoginSubmit melayani POST /login.
//
// Kredensial salah dijawab 401 dengan halaman form yang sama dan pesan generik,
// sehingga klien tidak bisa membedakan "pengguna tidak ada" dari "kata sandi
// salah" (10.1, kode 2006).
func (a *Admin) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	user, ok := a.authenticate(r.Context(), username, password)
	if !ok {
		a.log.Warn("admin: login gagal", "username", username, "remote", r.RemoteAddr)
		data := loginData{
			base:     a.base(w, r, "Masuk", "login"),
			Username: username,
		}
		data.Error = "Nama pengguna atau kata sandi salah."
		a.renderPage(w, r, "login", data, http.StatusUnauthorized)
		return
	}

	if err := a.sessions.Create(w, user.Username, user.Role); err != nil {
		a.log.Error("admin: gagal membuat sesi", "username", user.Username, "err", err)
		a.renderPage(w, r, "login", loginData{
			base:     a.base(w, r, "Masuk", "login"),
			Username: username,
		}, http.StatusInternalServerError)
		return
	}

	a.touchLastLogin(r.Context(), user.ID)
	a.log.Info("admin: login berhasil", "username", user.Username, "role", user.Role)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout melayani POST /logout.
func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	username := a.sessionUsername(r)
	a.sessions.Destroy(w)
	if username != "" {
		a.mu.Lock()
		delete(a.flashes, username)
		a.mu.Unlock()
		a.log.Info("admin: logout", "username", username)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
