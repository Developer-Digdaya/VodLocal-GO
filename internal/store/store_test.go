package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// newStore membuat Store baru di direktori sementara tanpa migrasi.
func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "vodlocal.db"))
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error: %v", err)
		}
	})

	return s
}

// newMigratedStore membuat Store baru dan menjalankan migrasi.
func newMigratedStore(t *testing.T) *Store {
	t.Helper()

	s := newStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}

	return s
}

func scalarInt(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()

	var n int
	if err := s.Write().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q error: %v", query, err)
	}

	return n
}

// 1. Open + Migrate berhasil dan menghasilkan 17 tabel aplikasi
// (0001 membuat 17 tabel, 0002 menambah generation_items, 0003 menghapus
// sync_tasks).
func TestOpenAndMigrateCreatesSchema(t *testing.T) {
	s := newMigratedStore(t)

	got := scalarInt(t, s,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if got != 17 {
		t.Errorf("jumlah tabel = %d, ingin 17", got)
	}
}

// 2. Migrate idempotent: dipanggil dua kali tetap sukses dan tidak menambah baris.
func TestMigrateTwiceIsIdempotent(t *testing.T) {
	s := newMigratedStore(t)

	before := scalarInt(t, s, `SELECT COUNT(*) FROM schema_migrations`)

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() kedua error: %v", err)
	}

	after := scalarInt(t, s, `SELECT COUNT(*) FROM schema_migrations`)
	if after != before {
		t.Errorf("baris schema_migrations = %d setelah migrasi kedua, sebelum %d", after, before)
	}
	if after == 0 {
		t.Error("schema_migrations kosong setelah migrasi")
	}
}

// 3. Pragma dari DSN: journal_mode = wal (pada pool write maupun read).
func TestJournalModeIsWAL(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	var writeMode string
	if err := s.Write().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&writeMode); err != nil {
		t.Fatalf("pragma journal_mode (write) error: %v", err)
	}
	if !strings.EqualFold(writeMode, "wal") {
		t.Errorf("journal_mode (write) = %q, ingin \"wal\"", writeMode)
	}

	var readMode string
	if err := s.Read().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&readMode); err != nil {
		t.Fatalf("pragma journal_mode (read) error: %v", err)
	}
	if !strings.EqualFold(readMode, "wal") {
		t.Errorf("journal_mode (read) = %q, ingin \"wal\"", readMode)
	}
}

// 4. foreign_keys aktif pada koneksi write.
func TestForeignKeysOnWriteConnection(t *testing.T) {
	s := newMigratedStore(t)

	var on int
	if err := s.Write().QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatalf("pragma foreign_keys error: %v", err)
	}
	if on != 1 {
		t.Errorf("foreign_keys = %d, ingin 1", on)
	}
}

// 5. Dua generation aktif ditolak unique index parsial pada generated column.
func TestOnlyOneActiveGeneration(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	insert := `INSERT INTO generations(label, root_path, status) VALUES(?,?,'active')`
	if _, err := s.Write().ExecContext(ctx, insert, "gen-000001", "/srv/media/generations/gen-000001"); err != nil {
		t.Fatalf("insert generation pertama error: %v", err)
	}

	_, err := s.Write().ExecContext(ctx, insert, "gen-000002", "/srv/media/generations/gen-000002")
	if err == nil {
		t.Fatal("insert generation aktif kedua tidak error, ingin UNIQUE constraint failed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		t.Errorf("error = %v, ingin memuat \"UNIQUE constraint failed\"", err)
	}

	// Status non-aktif tetap boleh berlapis.
	if _, err := s.Write().ExecContext(ctx,
		`INSERT INTO generations(label, root_path, status) VALUES(?,?,'retired')`,
		"gen-000003", "/srv/media/generations/gen-000003",
	); err != nil {
		t.Errorf("insert generation retired error: %v", err)
	}
}

// 6. Dua sync non-terminal ditolak unique index parsial pada generated column.
func TestOnlyOneActiveSyncRun(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	insert := `INSERT INTO sync_runs(trigger_source) VALUES('manual')`
	if _, err := s.Write().ExecContext(ctx, insert); err != nil {
		t.Fatalf("insert sync_run pertama error: %v", err)
	}

	_, err := s.Write().ExecContext(ctx,
		`INSERT INTO sync_runs(trigger_source, status) VALUES('scheduled','running')`)
	if err == nil {
		t.Fatal("insert sync_run non-terminal kedua tidak error, ingin UNIQUE constraint failed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		t.Errorf("error = %v, ingin memuat \"UNIQUE constraint failed\"", err)
	}
}

// 7. integrity_check = ok setelah skema dibuat.
func TestIntegrityCheck(t *testing.T) {
	s := newMigratedStore(t)

	var result string
	if err := s.Read().QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatalf("pragma integrity_check error: %v", err)
	}
	if result != "ok" {
		t.Errorf("integrity_check = %q, ingin \"ok\"", result)
	}
}

// 8. Close menutup kedua pool dan aman dipanggil dua kali.
func TestCloseTwice(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vodlocal.db"))
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close() pertama error: %v", err)
	}

	// Panggilan kedua tidak boleh panic; nil pool aman ditutup ulang.
	if err := s.Close(); err != nil {
		t.Errorf("Close() kedua error: %v", err)
	}

	if s.Read() != nil || s.Write() != nil {
		t.Errorf("pool masih tidak nil setelah Close: read=%v write=%v", s.Read(), s.Write())
	}
}

// splitStatements membuang komentar dan baris kosong, lalu memecah pada ";".
func TestSplitStatements(t *testing.T) {
	script := "-- komentar\nCREATE TABLE a (id INTEGER);\n\n  -- komentar indentasi\nCREATE INDEX i ON a(id);\n"

	got := splitStatements(script)
	want := []string{"CREATE TABLE a (id INTEGER)", "CREATE INDEX i ON a(id)"}

	if len(got) != len(want) {
		t.Fatalf("jumlah statement = %d (%q), ingin %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement #%d = %q, ingin %q", i+1, got[i], want[i])
		}
	}
}

// File migrasi ter-embed, terurut, dan nomornya terbaca.
func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("tidak ada migrasi ter-embed")
	}
	if migrations[0].version != 1 || migrations[0].name != "0001_init.sql" {
		t.Errorf("migrasi pertama = %+v, ingin versi 1 bernama 0001_init.sql", migrations[0])
	}

	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatalf("ReadDir embed error: %v", err)
	}
	if len(entries) != len(migrations) {
		t.Errorf("migrasi ter-embed = %d, terbaca = %d", len(entries), len(migrations))
	}
}
