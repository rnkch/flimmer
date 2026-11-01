package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateFromFirstRelease opens a database with the first release's jobs
// table and checks that rows survive and the URL columns exist afterwards.
func TestMigrateFromFirstRelease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
CREATE TABLE media (id INTEGER PRIMARY KEY, filename TEXT NOT NULL UNIQUE, title TEXT NOT NULL,
    kind TEXT NOT NULL, position INTEGER NOT NULL, image_duration_s INTEGER NOT NULL DEFAULT 5,
    width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, duration_s REAL NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT 'import',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')));
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL CHECK (source IN ('telegram', 'web')),
    src_path TEXT NOT NULL DEFAULT '', orig_name TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
    want_position INTEGER NOT NULL DEFAULT 0, image_duration_s INTEGER NOT NULL DEFAULT 0,
    tg_file_id TEXT NOT NULL DEFAULT '', tg_chat_id INTEGER NOT NULL DEFAULT 0, tg_progress_msg_id INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'queued', progress INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
    media_id INTEGER REFERENCES media (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')));
INSERT INTO media (filename, title, kind, position) VALUES ('a.jpg', 'A', 'image', 1);
INSERT INTO jobs (source, orig_name, state, media_id) VALUES ('telegram', 'alt.mp4', 'done', 1);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	j, err := st.GetJob(ctx, 1)
	if err != nil || j.OrigName != "alt.mp4" || j.State != "done" || j.MediaID.Int64 != 1 {
		t.Fatalf("old job after migration: %+v %v", j, err)
	}
	var v int
	st.DB.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d", v)
	}
	if _, err := st.DB.Exec(`INSERT INTO jobs (source, url, format) VALUES ('url', 'https://x', '18')`); err != nil {
		t.Fatalf("url job after migration: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO media (filename, title, kind, position, url) VALUES ('stream-x', 'S', 'stream', 2, 'https://x')`); err != nil {
		t.Fatalf("stream entry after migration: %v", err)
	}
	if m, err := st.GetMedia(ctx, 1); err != nil || m.Filename != "a.jpg" {
		t.Fatalf("old media after migration: %+v %v", m, err)
	}

	// Opening again is a no-op.
	st.Close()
	if st, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func TestFreshDatabaseIsCurrent(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	st.DB.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d", v)
	}
}
