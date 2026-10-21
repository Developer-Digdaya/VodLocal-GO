package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// migrationsFS memuat seluruh file migrasi bernomor.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	migrationsDir     = "migrations"
	schemaMigrations  = "schema_migrations"
	sqliteMasterTable = "sqlite_master"
)

// migration adalah satu file migrasi yang sudah di-embed.
type migration struct {
	version int
	name    string
	body    string
}

// Migrate menerapkan migrasi yang belum tercatat di tabel schema_migrations.
// Migrasi dijalankan berurutan menurut nama file, masing-masing dalam satu
// transaksi. Memanggil Migrate berulang kali aman (idempotent).
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.write == nil {
		return errors.New("store: Migrate dipanggil pada Store yang sudah ditutup")
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}

	return nil
}

// applyMigration menjalankan semua statement satu migrasi dalam satu transaksi
// lalu mencatatnya di schema_migrations.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: mulai transaksi migrasi %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range splitStatements(m.body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migrasi %s statement #%d: %w", m.name, i+1, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO "+schemaMigrations+"(version,name) VALUES(?,?)",
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("store: catat migrasi %s: %w", m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migrasi %s: %w", m.name, err)
	}

	return nil
}

// appliedVersions membaca versi migrasi yang sudah tercatat. Bila tabel
// schema_migrations belum ada (database baru), hasilnya kosong.
func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	var n int
	err := s.write.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+sqliteMasterTable+" WHERE type='table' AND name=?",
		schemaMigrations,
	).Scan(&n)
	if err != nil {
		return nil, fmt.Errorf("store: cek tabel %s: %w", schemaMigrations, err)
	}

	applied := make(map[int]bool)
	if n == 0 {
		return applied, nil
	}

	rows, err := s.write.QueryContext(ctx, "SELECT version FROM "+schemaMigrations)
	if err != nil {
		return nil, fmt.Errorf("store: baca %s: %w", schemaMigrations, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan %s.version: %w", schemaMigrations, err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterasi %s: %w", schemaMigrations, err)
	}

	return applied, nil
}

// loadMigrations membaca dan mengurutkan seluruh file migrasi menurut nama.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("store: baca direktori migrasi: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		body, err := migrationsFS.ReadFile(migrationsDir + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: baca migrasi %s: %w", entry.Name(), err)
		}

		version, err := migrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}

		out = append(out, migration{version: version, name: entry.Name(), body: string(body)})
	}

	if len(out) == 0 {
		return nil, errors.New("store: tidak ada file migrasi ter-embed")
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].version != out[j].version {
			return out[i].version < out[j].version
		}
		return out[i].name < out[j].name
	})

	return out, nil
}

// migrationVersion mengambil nomor versi dari nama file, mis. "0001_init.sql" → 1.
func migrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	prefix, _, found := strings.Cut(base, "_")
	if !found {
		prefix = base
	}

	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("store: nama migrasi %q tidak berawalan nomor: %w", name, err)
	}

	return version, nil
}

// splitStatements memecah skrip SQL menjadi statement tunggal: baris komentar
// ("--") dan baris kosong dibuang, sisanya dipecah pada ";".
// DDL proyek ini tidak memuat ";" di dalam string literal.
func splitStatements(script string) []string {
	var b strings.Builder

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	var stmts []string
	for _, part := range strings.Split(b.String(), ";") {
		if stmt := strings.TrimSpace(part); stmt != "" {
			stmts = append(stmts, stmt)
		}
	}

	return stmts
}
