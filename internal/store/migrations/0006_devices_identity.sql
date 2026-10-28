-- ============================================================
-- 0006_devices_identity.sql
-- Rebuild tabel devices supaya:
--   * name boleh NULL (nama perangkat opsional), dan
--   * identitas perangkat unik secara case-insensitive (COLLATE NOCASE),
--     karena klien boleh mengirim MAC maupun device ID bebas.
--
-- Migrasi ini dijalankan dengan PRAGMA foreign_keys=OFF (lihat migrate.go):
-- DROP TABLE devices akan memicu ON DELETE CASCADE ke playback_progress bila
-- foreign key aktif, sehingga progres tonton perangkat bisa hilang.
-- ============================================================

CREATE TABLE devices_new (
    id           INTEGER PRIMARY KEY,
    name         TEXT,
    seat_number  TEXT,
    mac_address  TEXT COLLATE NOCASE UNIQUE,
    ip_address   TEXT,
    token_hash   TEXT NOT NULL UNIQUE,
    is_active    INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    last_seen_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

INSERT INTO devices_new
    SELECT id, NULLIF(name, ''), seat_number, mac_address, ip_address, token_hash,
           is_active, last_seen_at, created_at
    FROM devices;

DROP TABLE devices;

ALTER TABLE devices_new RENAME TO devices;

CREATE INDEX idx_devices_active ON devices(is_active, last_seen_at DESC);
