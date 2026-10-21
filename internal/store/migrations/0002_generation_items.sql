-- ============================================================
-- 0002_generation_items.sql
-- Kategori statis per provider + progres per series dalam
-- sebuah generation.
-- ============================================================

ALTER TABLE providers ADD COLUMN category TEXT NOT NULL DEFAULT '';

UPDATE providers SET category = 'short' WHERE code = 'dramahits';
UPDATE providers SET category = 'vod'   WHERE code = 'densgo';

CREATE TABLE generation_items (
    id                 INTEGER PRIMARY KEY,
    generation_id      INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id        INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    category           TEXT NOT NULL DEFAULT '',
    series_external_id TEXT NOT NULL,
    series_title       TEXT NOT NULL,
    poster_url         TEXT,
    status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending','running','done','failed','skipped')),
    current_step       TEXT,
    current_episode    TEXT,
    total_episodes     INTEGER NOT NULL DEFAULT 0,
    episodes_done      INTEGER NOT NULL DEFAULT 0,
    episodes_failed    INTEGER NOT NULL DEFAULT 0,
    total_renditions   INTEGER NOT NULL DEFAULT 0,
    renditions_ready   INTEGER NOT NULL DEFAULT 0,
    renditions_failed  INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded   INTEGER NOT NULL DEFAULT 0,
    progress_percent   INTEGER NOT NULL DEFAULT 0
                       CHECK (progress_percent BETWEEN 0 AND 100),
    error_message      TEXT,
    started_at         TEXT,
    finished_at        TEXT,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (generation_id, provider_id, series_external_id)
);

CREATE INDEX idx_generation_items_gen      ON generation_items(generation_id, status);
CREATE INDEX idx_generation_items_provider ON generation_items(generation_id, provider_id);
