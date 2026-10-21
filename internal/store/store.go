// Package store menyediakan akses SQLite (driver pure-Go modernc.org/sqlite)
// untuk aplikasi vodlocal.
//
// SQLite memakai model "satu writer, banyak reader": Store membuka dua pool
// *sql.DB terpisah di atas file database yang sama. Pool tulis dibatasi satu
// koneksi (menghilangkan SQLITE_BUSY tanpa retry loop), pool baca boleh
// membuka beberapa koneksi karena WAL mengizinkan reader paralel.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Driver SQLite pure Go, tanpa cgo.
	_ "modernc.org/sqlite"
)

const (
	// driverName adalah nama driver yang didaftarkan modernc.org/sqlite.
	driverName = "sqlite"

	// dsnParams adalah parameter DSN (pragma) yang dipakai kedua pool.
	// mode=ro sengaja tidak dipakai: pool baca harus bisa dibuka sebelum
	// database ada (migrasi dijalankan setelah Open).
	dsnParams = "_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=cache_size(-65536)"

	// readMaxOpenConns / writeMaxOpenConns mengikuti desain §10:
	// banyak reader, tepat satu writer.
	readMaxOpenConns  = 8
	writeMaxOpenConns = 1

	// pingTimeout membatasi lamanya Open menunggu database bisa dihubungi.
	pingTimeout = 10 * time.Second
)

// Store membungkus dua pool koneksi ke satu file SQLite.
type Store struct {
	path  string
	read  *sql.DB
	write *sql.DB
}

// DSN menyusun DSN SQLite untuk dbPath. Pragma diset lewat DSN sehingga
// berlaku untuk setiap koneksi di pool.
func DSN(dbPath string) string {
	p := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(p, "file:") {
		p = "file:" + p
	}
	return p + "?" + dsnParams
}

// Open membuka pool baca dan tulis untuk database pada dbPath.
// File database boleh belum ada; migrasi dijalankan setelahnya lewat Migrate.
func Open(dbPath string) (*Store, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, errors.New("store: dbPath kosong")
	}

	if dbPath != ":memory:" {
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: buat direktori database: %w", err)
			}
		}
	}

	dsn := DSN(dbPath)

	write, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: buka pool tulis: %w", err)
	}
	write.SetMaxOpenConns(writeMaxOpenConns)
	write.SetMaxIdleConns(writeMaxOpenConns)

	read, err := sql.Open(driverName, dsn)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("store: buka pool baca: %w", err)
	}
	read.SetMaxOpenConns(readMaxOpenConns)
	read.SetMaxIdleConns(readMaxOpenConns)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	if err := write.PingContext(ctx); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("store: ping pool tulis: %w", err)
	}
	if err := read.PingContext(ctx); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("store: ping pool baca: %w", err)
	}

	return &Store{path: dbPath, read: read, write: write}, nil
}

// Path mengembalikan path file database yang dibuka.
func (s *Store) Path() string { return s.path }

// Read mengembalikan pool untuk query baca.
func (s *Store) Read() *sql.DB { return s.read }

// Write mengembalikan pool untuk perintah tulis (satu koneksi).
func (s *Store) Write() *sql.DB { return s.write }

// Close menutup kedua pool. Aman dipanggil lebih dari sekali.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}

	var errs []error

	if s.read != nil {
		errs = append(errs, s.read.Close())
		s.read = nil
	}
	if s.write != nil {
		errs = append(errs, s.write.Close())
		s.write = nil
	}

	return errors.Join(errs...)
}
