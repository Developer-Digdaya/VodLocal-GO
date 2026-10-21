-- ============================================================
-- 0001_init.sql
-- SQLite 3.46+  |  jalankan dengan foreign_keys=ON
-- ============================================================

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- ------------------------------------------------------------
-- SUMBER KONTEN
-- ------------------------------------------------------------
CREATE TABLE providers (
    id             INTEGER PRIMARY KEY,
    code           TEXT NOT NULL UNIQUE,          -- 'dens_tv' | 'dramahits'
    name           TEXT NOT NULL,
    base_url       TEXT,
    is_enabled     INTEGER NOT NULL DEFAULT 1 CHECK (is_enabled IN (0,1)),
    last_synced_at TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- ------------------------------------------------------------
-- KATALOG
-- ------------------------------------------------------------
CREATE TABLE titles (
    id               INTEGER PRIMARY KEY,
    provider_id      INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    external_id      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('movie','series')),
    title            TEXT NOT NULL,
    slug             TEXT NOT NULL,
    synopsis         TEXT,
    release_year     INTEGER,
    content_rating   TEXT,                        -- 'SU','13+','17+','R'
    language         TEXT,                        -- 'id','ko','en'
    country          TEXT,
    poster_path      TEXT,                        -- relatif ke /srv/media/artwork
    backdrop_path    TEXT,
    duration_seconds INTEGER,                     -- hanya movie
    is_published     INTEGER NOT NULL DEFAULT 1 CHECK (is_published IN (0,1)),
    raw_payload      TEXT,                        -- JSON mentah (debug, boleh dikosongkan)
    fetched_at       TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (provider_id, external_id)
);

CREATE INDEX idx_titles_kind       ON titles(kind, is_published);
CREATE INDEX idx_titles_slug       ON titles(slug);
CREATE INDEX idx_titles_updated    ON titles(updated_at DESC);

CREATE TABLE genres (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    slug TEXT NOT NULL UNIQUE
);

CREATE TABLE title_genres (
    title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    genre_id INTEGER NOT NULL REFERENCES genres(id) ON DELETE CASCADE,
    PRIMARY KEY (title_id, genre_id)
);

CREATE INDEX idx_title_genres_genre ON title_genres(genre_id);

CREATE TABLE episodes (
    id               INTEGER PRIMARY KEY,
    title_id         INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    external_id      TEXT NOT NULL,
    season_number    INTEGER NOT NULL DEFAULT 1,
    episode_number   INTEGER NOT NULL,
    name             TEXT,
    synopsis         TEXT,
    duration_seconds INTEGER,
    air_date         TEXT,
    still_path       TEXT,                        -- relatif ke /srv/media/artwork
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (title_id, season_number, episode_number),
    UNIQUE (title_id, external_id)
);

CREATE INDEX idx_episodes_title ON episodes(title_id, season_number, episode_number);

-- ------------------------------------------------------------
-- UNIT PUTAR
-- ------------------------------------------------------------
CREATE TABLE playables (
    id               INTEGER PRIMARY KEY,
    title_id         INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    episode_id       INTEGER REFERENCES episodes(id) ON DELETE CASCADE,  -- NULL = movie
    duration_seconds INTEGER,
    is_encrypted     INTEGER NOT NULL DEFAULT 0 CHECK (is_encrypted IN (0,1)),
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- movie: satu playable per title
CREATE UNIQUE INDEX ux_playables_movie
    ON playables(title_id) WHERE episode_id IS NULL;
-- episode: satu playable per episode
CREATE UNIQUE INDEX ux_playables_episode
    ON playables(episode_id) WHERE episode_id IS NOT NULL;

-- ------------------------------------------------------------
-- GENERASI MEDIA
-- ------------------------------------------------------------
CREATE TABLE generations (
    id           INTEGER PRIMARY KEY,
    label        TEXT NOT NULL UNIQUE,            -- 'gen-000042'
    root_path    TEXT NOT NULL UNIQUE,            -- /srv/media/generations/gen-000042
    status       TEXT NOT NULL DEFAULT 'building'
                 CHECK (status IN ('building','active','retired','failed')),
    file_count   INTEGER NOT NULL DEFAULT 0,
    total_bytes  INTEGER NOT NULL DEFAULT 0,
    note         TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    activated_at TEXT,
    retired_at   TEXT,
    is_active    INTEGER GENERATED ALWAYS AS (
                     CASE WHEN status = 'active' THEN 1 END
                 ) VIRTUAL
);

-- hanya satu generation boleh aktif
CREATE UNIQUE INDEX ux_generations_one_active ON generations(is_active);
CREATE INDEX idx_generations_status ON generations(status, created_at DESC);

-- ------------------------------------------------------------
-- MEDIA FISIK PER GENERASI
-- ------------------------------------------------------------
CREATE TABLE renditions (
    id               INTEGER PRIMARY KEY,
    playable_id      INTEGER NOT NULL REFERENCES playables(id) ON DELETE CASCADE,
    generation_id    INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    quality          TEXT NOT NULL,               -- '360p','480p','720p'
    bitrate_kbps     INTEGER,
    width            INTEGER,
    height           INTEGER,
    container        TEXT NOT NULL DEFAULT 'ts' CHECK (container IN ('ts','fmp4')),
    source_url       TEXT,                        -- URL asli provider (audit)
    playlist_path    TEXT,                        -- relatif ke generations.root_path
    segment_count    INTEGER NOT NULL DEFAULT 0,
    segments_done    INTEGER NOT NULL DEFAULT 0,
    total_bytes      INTEGER NOT NULL DEFAULT 0,
    bytes_done       INTEGER NOT NULL DEFAULT 0,
    duration_seconds INTEGER,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','downloading','ready','failed','skipped')),
    checksum         TEXT,
    verified_at      TEXT,
    last_error       TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (playable_id, generation_id, quality)
);

CREATE INDEX idx_renditions_gen_status ON renditions(generation_id, status);
CREATE INDEX idx_renditions_playable   ON renditions(playable_id);

CREATE TABLE subtitles (
    id            INTEGER PRIMARY KEY,
    playable_id   INTEGER NOT NULL REFERENCES playables(id) ON DELETE CASCADE,
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    lang          TEXT NOT NULL,                  -- 'id','en'
    label         TEXT,
    format        TEXT NOT NULL DEFAULT 'vtt' CHECK (format IN ('vtt','srt')),
    file_path     TEXT NOT NULL,                  -- relatif ke generations.root_path
    bytes         INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'ready'
                  CHECK (status IN ('pending','ready','failed')),
    UNIQUE (playable_id, generation_id, lang)
);

CREATE INDEX idx_subtitles_playable ON subtitles(playable_id, generation_id);

-- ------------------------------------------------------------
-- PROSES SYNC
-- ------------------------------------------------------------
CREATE TABLE sync_runs (
    id                 INTEGER PRIMARY KEY,
    generation_id      INTEGER REFERENCES generations(id) ON DELETE SET NULL,
    trigger_source     TEXT NOT NULL CHECK (trigger_source IN ('manual','scheduled','boot')),
    scope              TEXT NOT NULL DEFAULT 'full'
                       CHECK (scope IN ('full','metadata_only','selection')),
    status             TEXT NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued','running','verifying','switching',
                                         'cleanup','completed','failed','cancelled')),
    phase              TEXT,                      -- 'discovery','metadata','segments',...
    triggered_by       TEXT,                      -- admin username / 'scheduler'
    total_providers    INTEGER NOT NULL DEFAULT 0,
    total_titles       INTEGER NOT NULL DEFAULT 0,
    total_playables    INTEGER NOT NULL DEFAULT 0,
    total_renditions   INTEGER NOT NULL DEFAULT 0,
    renditions_ready   INTEGER NOT NULL DEFAULT 0,
    renditions_failed  INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded   INTEGER NOT NULL DEFAULT 0,
    started_at         TEXT,
    finished_at        TEXT,
    error_message      TEXT,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    is_active          INTEGER GENERATED ALWAYS AS (
                           CASE WHEN status IN ('queued','running','verifying',
                                                'switching','cleanup') THEN 1 END
                       ) VIRTUAL
);

-- hanya satu sync boleh berjalan
CREATE UNIQUE INDEX ux_sync_runs_one_active ON sync_runs(is_active);
CREATE INDEX idx_sync_runs_created ON sync_runs(created_at DESC);

CREATE TABLE sync_tasks (
    id            INTEGER PRIMARY KEY,
    sync_run_id   INTEGER NOT NULL REFERENCES sync_runs(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL
                  CHECK (kind IN ('discover','metadata','playlist','segments','subtitles','verify','cleanup')),
    provider_id   INTEGER REFERENCES providers(id) ON DELETE SET NULL,
    playable_id   INTEGER REFERENCES playables(id) ON DELETE SET NULL,
    rendition_id  INTEGER REFERENCES renditions(id) ON DELETE SET NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','running','done','failed','skipped')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    total_units   INTEGER NOT NULL DEFAULT 0,     -- jumlah segmen / judul
    done_units    INTEGER NOT NULL DEFAULT 0,
    bytes_done    INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    started_at    TEXT,
    finished_at   TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX idx_sync_tasks_run    ON sync_tasks(sync_run_id, status);
CREATE INDEX idx_sync_tasks_rendition ON sync_tasks(rendition_id);

-- ------------------------------------------------------------
-- PERANGKAT & PLAYBACK
-- ------------------------------------------------------------
CREATE TABLE devices (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    seat_number  TEXT,
    mac_address  TEXT UNIQUE,
    ip_address   TEXT,
    token_hash   TEXT NOT NULL UNIQUE,            -- SHA-256 token, bukan token mentah
    is_active    INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    last_seen_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX idx_devices_active ON devices(is_active, last_seen_at DESC);

CREATE TABLE playback_progress (
    device_id        INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    playable_id      INTEGER NOT NULL REFERENCES playables(id) ON DELETE CASCADE,
    position_seconds INTEGER NOT NULL DEFAULT 0,
    duration_seconds INTEGER,
    completed        INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0,1)),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    PRIMARY KEY (device_id, playable_id)
);

CREATE INDEX idx_progress_updated ON playback_progress(updated_at DESC);

-- ------------------------------------------------------------
-- APLIKASI
-- ------------------------------------------------------------
CREATE TABLE auth_users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,                  -- argon2id / bcrypt
    role          TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin','operator')),
    is_active     INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    last_login_at TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,                    -- 'sync.trigger','device.disable',...
    target_type TEXT,
    target_id   TEXT,
    detail      TEXT,                             -- JSON
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX idx_audit_created ON audit_log(created_at DESC);
