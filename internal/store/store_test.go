package store

import (
	"context"
	"database/sql"
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

// 1. Open + Migrate berhasil dan menghasilkan 15 tabel aplikasi
// (0001 membuat 15 tabel, 0002 menambah generation_items, 0003 menghapus
// sync_tasks, 0004 menghapus genres/title_genres).
func TestOpenAndMigrateCreatesSchema(t *testing.T) {
	s := newMigratedStore(t)

	got := scalarInt(t, s,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if got != 15 {
		t.Errorf("jumlah tabel = %d, ingin 15", got)
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

// 9. Migrasi 0006: devices.name boleh NULL dan identitas unik
// case-insensitive.
func TestDevicesIdentitySchema(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	// name tidak lagi NOT NULL.
	var notNull int
	if err := s.Write().QueryRowContext(ctx,
		`SELECT "notnull" FROM pragma_table_info('devices') WHERE name = 'name'`,
	).Scan(&notNull); err != nil {
		t.Fatalf("baca pragma_table_info(devices).name: %v", err)
	}
	if notNull != 0 {
		t.Errorf("devices.name NOT NULL = %d, ingin 0 (nama opsional)", notNull)
	}

	// name kosong boleh, dan tersimpan sebagai NULL.
	if _, err := s.Write().ExecContext(ctx,
		`INSERT INTO devices (name, seat_number, mac_address, token_hash) VALUES (NULL, '1', 'android-a1b2-c3d4', 'hash-1')`,
	); err != nil {
		t.Fatalf("insert device tanpa nama error: %v", err)
	}

	// Identitas unik tanpa memandang kapitalisasi.
	_, err := s.Write().ExecContext(ctx,
		`INSERT INTO devices (name, seat_number, mac_address, token_hash) VALUES ('Lain', '2', 'ANDROID-A1B2-C3D4', 'hash-2')`,
	)
	if err == nil {
		t.Fatal("insert identitas beda kapitalisasi tidak error, ingin UNIQUE constraint failed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		t.Errorf("error = %v, ingin memuat \"UNIQUE constraint failed\"", err)
	}

	// Index untuk panel admin ikut dibuat ulang.
	var indexes int
	if err := s.Write().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_devices_active'`,
	).Scan(&indexes); err != nil {
		t.Fatalf("hitung index idx_devices_active: %v", err)
	}
	if indexes != 1 {
		t.Errorf("index idx_devices_active = %d, ingin 1", indexes)
	}
}

// 10. Rebuild devices pada migrasi 0006 tidak menghapus playback_progress.
func TestMigrateDevicesRebuildKeepsProgress(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error: %v", err)
	}

	// Terapkan migrasi sebelum 0006 supaya ada data dengan skema devices lama.
	conn, err := s.Write().Conn(ctx)
	if err != nil {
		t.Fatalf("ambil koneksi migrasi: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("pragma foreign_keys=OFF: %v", err)
	}
	for _, m := range migrations {
		if m.version >= 6 {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			t.Fatalf("applyMigration(%s) error: %v", m.name, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatalf("pragma foreign_keys=ON: %v", err)
	}
	// Pool tulis hanya punya satu koneksi: kembalikan dulu agar Migrate dan
	// seed berikutnya tidak menunggu selamanya.
	if err := conn.Close(); err != nil {
		t.Fatalf("tutup koneksi migrasi: %v", err)
	}

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q error: %v", query, err)
		}
	}

	exec(`INSERT INTO providers (id, code, name) VALUES (1, 'dens_tv', 'Dens TV')`)
	exec(`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
	      VALUES (1, 1, 'ext-1', 'movie', 'Film Lama', 'film-lama')`)
	exec(`INSERT INTO playables (id, title_id, duration_seconds) VALUES (1, 1, 5400)`)
	// name NOT NULL pada skema lama: nama kosong disimpan sebagai ''.
	exec(`INSERT INTO devices (id, name, seat_number, mac_address, token_hash, is_active)
	      VALUES (7, 'Tablet Lama', '3', 'aa-bb-cc-dd-ee-07', 'hash-7', 1)`)
	exec(`INSERT INTO devices (id, name, seat_number, mac_address, token_hash, is_active)
	      VALUES (8, '', '4', 'android-a1b2-c3d4', 'hash-8', 1)`)
	exec(`INSERT INTO playback_progress (device_id, playable_id, position_seconds)
	      VALUES (7, 1, 120)`)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}

	var devices int
	if err := s.Write().QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&devices); err != nil {
		t.Fatalf("hitung devices: %v", err)
	}
	if devices != 2 {
		t.Errorf("baris devices = %d, ingin 2", devices)
	}

	var (
		name       sql.NullString
		macAddress string
	)
	if err := s.Write().QueryRowContext(ctx,
		`SELECT name, mac_address FROM devices WHERE id = 7`).Scan(&name, &macAddress); err != nil {
		t.Fatalf("baca device 7: %v", err)
	}
	if !name.Valid || name.String != "Tablet Lama" {
		t.Errorf("name device 7 = %v, ingin \"Tablet Lama\"", name)
	}
	if macAddress != "aa-bb-cc-dd-ee-07" {
		t.Errorf("mac_address device 7 = %q, ingin tidak berubah", macAddress)
	}

	// Nama kosong dari skema lama menjadi NULL.
	if err := s.Write().QueryRowContext(ctx,
		`SELECT name FROM devices WHERE id = 8`).Scan(&name); err != nil {
		t.Fatalf("baca device 8: %v", err)
	}
	if name.Valid {
		t.Errorf("name device 8 = %q, ingin NULL", name.String)
	}

	// Progres tonton tidak boleh ikut terhapus oleh rebuild tabel.
	var progress int
	if err := s.Write().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playback_progress WHERE device_id = 7`).Scan(&progress); err != nil {
		t.Fatalf("hitung playback_progress: %v", err)
	}
	if progress != 1 {
		t.Errorf("playback_progress device 7 = %d, ingin 1", progress)
	}

	// foreign_keys kembali menyala pada koneksi pool tulis.
	var fk int
	if err := s.Write().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d setelah Migrate, ingin 1", fk)
	}

	// Tidak ada pelanggaran foreign key yang tersisa.
	rows, err := s.Write().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("pragma foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Error("foreign_key_check masih melaporkan pelanggaran setelah migrasi")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterasi foreign_key_check: %v", err)
	}
}
