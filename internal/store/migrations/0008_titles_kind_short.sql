-- 0008_titles_kind_short.sql
--
-- Dramahits adalah katalog short, jadi judulnya memakai kind 'short' dan bukan
-- lagi 'series'. SQLite tidak bisa mengubah CHECK lewat ALTER TABLE, sehingga
-- tabel titles dibangun ulang. Migrate mematikan foreign_keys selama transaksi,
-- jadi DROP TABLE titles tidak memicu ON DELETE CASCADE pada episodes,
-- playables, dan generation_items.

CREATE TABLE titles_new (
    id               INTEGER PRIMARY KEY,
    provider_id      INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    external_id      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('movie','series','short')),
    title            TEXT NOT NULL,
    slug             TEXT NOT NULL,
    synopsis         TEXT,
    release_year     INTEGER,
    content_rating   TEXT,
    language         TEXT,
    country          TEXT,
    poster_path      TEXT,
    backdrop_path    TEXT,
    duration_seconds INTEGER,
    is_published     INTEGER NOT NULL DEFAULT 1 CHECK (is_published IN (0,1)),
    raw_payload      TEXT,
    fetched_at       TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (provider_id, external_id)
);

INSERT INTO titles_new (id, provider_id, external_id, kind, title, slug, synopsis,
    release_year, content_rating, language, country, poster_path, backdrop_path,
    duration_seconds, is_published, raw_payload, fetched_at, created_at, updated_at)
SELECT id, provider_id, external_id, kind, title, slug, synopsis,
    release_year, content_rating, language, country, poster_path, backdrop_path,
    duration_seconds, is_published, raw_payload, fetched_at, created_at, updated_at
FROM titles;

DROP TABLE titles;

ALTER TABLE titles_new RENAME TO titles;

CREATE INDEX idx_titles_kind    ON titles(kind, is_published);
CREATE INDEX idx_titles_slug    ON titles(slug);
CREATE INDEX idx_titles_updated ON titles(updated_at DESC);

UPDATE titles SET kind = 'short'
WHERE provider_id IN (SELECT id FROM providers WHERE code = 'dramahits');
