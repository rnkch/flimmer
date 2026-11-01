-- Playlist entries. Ordered by position (1..n, kept contiguous by the kiosk
-- service). Streams have no file: url is played directly by mpv (format is
-- the yt-dlp format spec it asks for); filename is then a unique placeholder.
CREATE TABLE IF NOT EXISTS media (
    id               INTEGER PRIMARY KEY,
    filename         TEXT    NOT NULL UNIQUE,
    title            TEXT    NOT NULL,
    kind             TEXT    NOT NULL CHECK (kind IN ('image', 'video', 'stream')),
    position         INTEGER NOT NULL,
    image_duration_s INTEGER NOT NULL DEFAULT 5,
    width            INTEGER NOT NULL DEFAULT 0,
    height           INTEGER NOT NULL DEFAULT 0,
    duration_s       REAL    NOT NULL DEFAULT 0,
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    source           TEXT    NOT NULL DEFAULT 'import',
    url              TEXT    NOT NULL DEFAULT '',
    format           TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Ingest queue: one row per uploaded file or URL. Survives restarts; a job
-- that was 'running' when the service stopped is put back to 'queued'.
CREATE TABLE IF NOT EXISTS jobs (
    id                 INTEGER PRIMARY KEY,
    source             TEXT    NOT NULL CHECK (source IN ('telegram', 'web', 'url')),
    src_path           TEXT    NOT NULL DEFAULT '',
    orig_name          TEXT    NOT NULL DEFAULT '',
    title              TEXT    NOT NULL DEFAULT '',
    want_position      INTEGER NOT NULL DEFAULT 0, -- 0 = append at end
    image_duration_s   INTEGER NOT NULL DEFAULT 0, -- 0 = default setting
    tg_file_id         TEXT    NOT NULL DEFAULT '',
    tg_chat_id         INTEGER NOT NULL DEFAULT 0,
    tg_progress_msg_id INTEGER NOT NULL DEFAULT 0,
    url                TEXT    NOT NULL DEFAULT '', -- source = 'url': page to download with yt-dlp
    format             TEXT    NOT NULL DEFAULT '', -- yt-dlp format spec, e.g. "298+140"
    phase              TEXT    NOT NULL DEFAULT '', -- while running: download | transcode
    replaces_media_id  INTEGER NOT NULL DEFAULT 0,  -- stream entry this download replaces when done
    no_transcode       INTEGER NOT NULL DEFAULT 0,  -- 1 = keep the video as it is (remux only)
    state              TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    progress           INTEGER NOT NULL DEFAULT 0,
    error              TEXT    NOT NULL DEFAULT '',
    media_id           INTEGER REFERENCES media (id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY,
    at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    level   TEXT NOT NULL,
    message TEXT NOT NULL
);

-- Web UI logins. Only a SHA-256 of the session token is stored.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    expires_at TEXT NOT NULL
);
