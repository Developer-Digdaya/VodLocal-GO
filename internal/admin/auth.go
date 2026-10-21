package admin

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Parameter penyimpanan kata sandi admin.
const (
	// pbkdf2Algo adalah label algoritma pada format hash.
	pbkdf2Algo = "pbkdf2-sha256"
	// pbkdf2Iterations adalah jumlah iterasi PBKDF2-HMAC-SHA256.
	pbkdf2Iterations = 210000
	// passwordSaltBytes dan passwordKeyBytes mengikuti rekomendasi 32 byte.
	passwordSaltBytes = 32
	passwordKeyBytes  = 32
	// maxPasswordIterations mencegah hash berbahaya (crafted) membuat server
	// sibuk saat verifikasi.
	maxPasswordIterations = 1000000
	// roleAdmin adalah peran pengguna yang dibuat SeedAdmin.
	roleAdmin = "admin"
)

// Error yang bisa dikembalikan fungsi kata sandi.
var (
	// ErrEmptyPassword dilaporkan bila kata sandi kosong.
	ErrEmptyPassword = errors.New("admin: kata sandi tidak boleh kosong")
	// ErrEmptyUsername dilaporkan bila nama pengguna kosong.
	ErrEmptyUsername = errors.New("admin: nama pengguna tidak boleh kosong")
)

// HashPassword menghasilkan hash kata sandi berformat
// "pbkdf2-sha256$<iterasi>$<salt-base64>$<hash-base64>".
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}

	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("admin: buat salt kata sandi: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, passwordKeyBytes)
	if err != nil {
		return "", fmt.Errorf("admin: hitung pbkdf2: %w", err)
	}

	return fmt.Sprintf("%s$%d$%s$%s",
		pbkdf2Algo,
		pbkdf2Iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword melaporkan apakah password cocok dengan hash berformat
// "pbkdf2-sha256$<iterasi>$<salt-base64>$<hash-base64>".
//
// Perbandingan memakai subtle.ConstantTimeCompare sehingga waktu eksekusi tidak
// membocorkan berapa banyak byte awal yang sudah benar.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Algo {
		return false
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > maxPasswordIterations {
		return false
	}

	salt, err := decodeBase64(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}

	want, err := decodeBase64(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}

	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(got, want) == 1
}

// decodeBase64 menerima base64 standar maupun tanpa padding.
func decodeBase64(value string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

// dummyHash dipakai agar login dengan nama pengguna yang tidak ada tetap
// menjalankan PBKDF2, sehingga waktu respons tidak membocorkan keberadaan user.
var dummyHash = sync.OnceValue(func() string {
	hash, err := HashPassword("dummy-password-for-timing")
	if err != nil {
		return ""
	}
	return hash
})

// authUser adalah satu baris auth_users.
type authUser struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	IsActive     bool
}

// findUser membaca pengguna berdasarkan nama pengguna. sql.ErrNoRows
// dikembalikan apa adanya bila tidak ada.
func (a *Admin) findUser(ctx context.Context, username string) (authUser, error) {
	var (
		user     authUser
		isActive int
	)
	err := a.read.QueryRowContext(ctx,
		`SELECT id, username, password_hash, role, is_active FROM auth_users WHERE username = ?`,
		username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &isActive)
	if err != nil {
		return authUser{}, err
	}
	user.IsActive = isActive == 1
	return user, nil
}

// countUsers menghitung jumlah baris auth_users.
func (a *Admin) countUsers(ctx context.Context) (int64, error) {
	var total int64
	if err := a.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// createUser menyimpan pengguna baru dengan kata sandi yang di-hash.
func (a *Admin) createUser(ctx context.Context, username, password, role string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = a.write.ExecContext(ctx,
		`INSERT INTO auth_users (username, password_hash, role, is_active, created_at)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT(username) DO NOTHING`,
		username, hash, role, a.stamp(),
	)
	if err != nil {
		return fmt.Errorf("admin: simpan pengguna: %w", err)
	}
	return nil
}

// touchLastLogin memperbarui auth_users.last_login_at. Kegagalan hanya dicatat
// ke log: login yang sudah berhasil tidak boleh gagal karena kolom audit.
func (a *Admin) touchLastLogin(ctx context.Context, userID int64) {
	if _, err := a.write.ExecContext(ctx,
		`UPDATE auth_users SET last_login_at = ? WHERE id = ?`, a.stamp(), userID,
	); err != nil {
		a.log.Warn("admin: gagal memperbarui last_login_at", "user_id", userID, "err", err)
	}
}

// authenticate memeriksa kredensial dan mengembalikan pengguna aktif yang cocok.
//
// Nilai balik kedua false untuk semua kegagalan (tidak ada user, user nonaktif,
// kata sandi salah) sehingga pemanggil tidak bisa membedakan penyebabnya.
func (a *Admin) authenticate(ctx context.Context, username, password string) (authUser, bool) {
	if username == "" || password == "" {
		VerifyPassword(dummyHash(), password)
		return authUser{}, false
	}

	user, err := a.findUser(ctx, username)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Error("admin: gagal membaca pengguna", "username", username, "err", err)
		}
		VerifyPassword(dummyHash(), password)
		return authUser{}, false
	}

	if !user.IsActive || !VerifyPassword(user.PasswordHash, password) {
		return authUser{}, false
	}

	return user, true
}

// SeedAdmin membuat pengguna admin awal.
//
// Fungsi ini idempoten: bila tabel auth_users sudah berisi minimal satu baris,
// tidak ada yang diubah. Kata sandi kosong dilaporkan sebagai error yang jelas
// agar kesalahan konfigurasi tidak menghasilkan admin tanpa sandi.
func (a *Admin) SeedAdmin(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return ErrEmptyUsername
	}
	if password == "" {
		return ErrEmptyPassword
	}

	total, err := a.countUsers(ctx)
	if err != nil {
		return fmt.Errorf("admin: hitung pengguna: %w", err)
	}
	if total > 0 {
		return nil
	}

	if err := a.createUser(ctx, username, password, roleAdmin); err != nil {
		return err
	}
	a.log.Info("admin: pengguna admin awal dibuat", "username", username, "role", roleAdmin)
	return nil
}
