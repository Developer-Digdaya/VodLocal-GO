// Command adminreset mengganti kata sandi satu akun admin tanpa menghapus
// katalog, storage, device, atau riwayat sync.
//
// Pemakaian:
//
//	go run ./cmd/adminreset -db data/app.db -user admin -password 'admin12345'
//
// Nilai -db, -user, dan -password juga bisa diambil dari VODLOCAL_DB_PATH,
// VODLOCAL_ADMIN_USER, dan VODLOCAL_ADMIN_PASSWORD.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"vodlocal/internal/admin"
	"vodlocal/internal/store"
)

func main() {
	dbDefault := strings.TrimSpace(os.Getenv("VODLOCAL_DB_PATH"))
	if dbDefault == "" {
		dbDefault = "data/app.db"
	}
	userDefault := strings.TrimSpace(os.Getenv("VODLOCAL_ADMIN_USER"))
	if userDefault == "" {
		userDefault = "admin"
	}
	passDefault := os.Getenv("VODLOCAL_ADMIN_PASSWORD")

	dbPath := flag.String("db", dbDefault, "path file SQLite")
	username := flag.String("user", userDefault, "nama pengguna admin")
	password := flag.String("password", passDefault, "kata sandi baru")
	flag.Parse()

	usernameValue := strings.TrimSpace(*username)
	if usernameValue == "" {
		fmt.Fprintln(os.Stderr, "adminreset: -user tidak boleh kosong")
		os.Exit(2)
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "adminreset: -password tidak boleh kosong")
		os.Exit(2)
	}

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "adminreset: buka database: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "adminreset: migrasi database: %v\n", err)
		os.Exit(1)
	}

	hash, err := admin.HashPassword(*password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "adminreset: hash kata sandi: %v\n", err)
		os.Exit(1)
	}

	if _, err := st.Write().ExecContext(ctx, `
INSERT INTO auth_users (username, password_hash, role, is_active)
VALUES (?, ?, 'admin', 1)
ON CONFLICT(username) DO UPDATE SET
    password_hash = excluded.password_hash,
    role          = 'admin',
    is_active     = 1`, usernameValue, hash); err != nil {
		fmt.Fprintf(os.Stderr, "adminreset: simpan admin: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("adminreset: kata sandi %q diperbarui di %s\n", usernameValue, *dbPath)
}
