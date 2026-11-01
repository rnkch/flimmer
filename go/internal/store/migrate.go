package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the version of schema.sql. Databases created by an older
// version are brought up to date by the migrations below.
const schemaVersion = 5

// migrations[v] upgrades a database from version v to v+1. Version 1 is the
// first release (jobs without URL support).
var migrations = map[int]string{
	1: `
CREATE TABLE jobs_new (
    id                 INTEGER PRIMARY KEY,
    source             TEXT    NOT NULL CHECK (source IN ('telegram', 'web', 'url')),
    src_path           TEXT    NOT NULL DEFAULT '',
    orig_name          TEXT    NOT NULL DEFAULT '',
    title              TEXT    NOT NULL DEFAULT '',
    want_position      INTEGER NOT NULL DEFAULT 0,
    image_duration_s   INTEGER NOT NULL DEFAULT 0,
    tg_file_id         TEXT    NOT NULL DEFAULT '',
    tg_chat_id         INTEGER NOT NULL DEFAULT 0,
    tg_progress_msg_id INTEGER NOT NULL DEFAULT 0,
    url                TEXT    NOT NULL DEFAULT '',
    format             TEXT    NOT NULL DEFAULT '',
    phase              TEXT    NOT NULL DEFAULT '',
    state              TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed')),
    progress           INTEGER NOT NULL DEFAULT 0,
    error              TEXT    NOT NULL DEFAULT '',
    media_id           INTEGER REFERENCES media (id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
INSERT INTO jobs_new (id, source, src_path, orig_name, title, want_position, image_duration_s,
    tg_file_id, tg_chat_id, tg_progress_msg_id, state, progress, error, media_id, created_at, updated_at)
SELECT id, source, src_path, orig_name, title, want_position, image_duration_s,
    tg_file_id, tg_chat_id, tg_progress_msg_id, state, progress, error, media_id, created_at, updated_at
FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
`,
	2: `
CREATE TABLE media_new (
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
INSERT INTO media_new (id, filename, title, kind, position, image_duration_s, width, height,
    duration_s, size_bytes, source, created_at)
SELECT id, filename, title, kind, position, image_duration_s, width, height,
    duration_s, size_bytes, source, created_at
FROM media;
DROP TABLE media;
ALTER TABLE media_new RENAME TO media;
ALTER TABLE jobs ADD COLUMN replaces_media_id INTEGER NOT NULL DEFAULT 0;
`,
	3: `
ALTER TABLE jobs ADD COLUMN no_transcode INTEGER NOT NULL DEFAULT 0;
`,
	// 4: the 'cancelled' state; a CHECK constraint can only change by rebuilding.
	4: `
CREATE TABLE jobs_new (
    id                 INTEGER PRIMARY KEY,
    source             TEXT    NOT NULL CHECK (source IN ('telegram', 'web', 'url')),
    src_path           TEXT    NOT NULL DEFAULT '',
    orig_name          TEXT    NOT NULL DEFAULT '',
    title              TEXT    NOT NULL DEFAULT '',
    want_position      INTEGER NOT NULL DEFAULT 0,
    image_duration_s   INTEGER NOT NULL DEFAULT 0,
    tg_file_id         TEXT    NOT NULL DEFAULT '',
    tg_chat_id         INTEGER NOT NULL DEFAULT 0,
    tg_progress_msg_id INTEGER NOT NULL DEFAULT 0,
    url                TEXT    NOT NULL DEFAULT '',
    format             TEXT    NOT NULL DEFAULT '',
    phase              TEXT    NOT NULL DEFAULT '',
    replaces_media_id  INTEGER NOT NULL DEFAULT 0,
    no_transcode       INTEGER NOT NULL DEFAULT 0,
    state              TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    progress           INTEGER NOT NULL DEFAULT 0,
    error              TEXT    NOT NULL DEFAULT '',
    media_id           INTEGER REFERENCES media (id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
INSERT INTO jobs_new (id, source, src_path, orig_name, title, want_position, image_duration_s,
    tg_file_id, tg_chat_id, tg_progress_msg_id, url, format, phase, replaces_media_id, no_transcode,
    state, progress, error, media_id, created_at, updated_at)
SELECT id, source, src_path, orig_name, title, want_position, image_duration_s,
    tg_file_id, tg_chat_id, tg_progress_msg_id, url, format, phase, replaces_media_id, no_transcode,
    state, progress, error, media_id, created_at, updated_at
FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
`,
}

// migrate runs before schema.sql: it upgrades existing tables so that the
// CREATE TABLE IF NOT EXISTS statements afterwards are no-ops for them.
func migrate(ctx context.Context, conn *sql.DB) error {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	var hasJobs int
	conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'jobs'").
		Scan(&hasJobs)
	if hasJobs == 0 {
		// Fresh database: schema.sql creates the current version.
		_, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	}
	if version == 0 {
		version = 1 // databases from before versioning
	}
	if version < schemaVersion {
		// Tables are rebuilt; without this, dropping media would null jobs.media_id.
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return err
		}
		defer conn.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	}
	for ; version < schemaVersion; version++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			fmt.Sprintf("PRAGMA user_version = %d", version+1),
		); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	var broken int
	conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&broken)
	if broken > 0 {
		return fmt.Errorf("migration left %d broken foreign keys", broken)
	}
	return nil
}
