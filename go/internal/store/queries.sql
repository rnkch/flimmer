-- name: ListMedia :many
SELECT * FROM media ORDER BY position, id;

-- name: GetMedia :one
SELECT * FROM media WHERE id = ?;

-- name: CountMedia :one
SELECT COUNT(*) FROM media;

-- name: InsertMedia :one
INSERT INTO media (filename, title, kind, position, image_duration_s, width, height, duration_s, size_bytes, source, url, format)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: SetMediaFormat :exec
UPDATE media SET format = ?, height = ? WHERE id = ?;

-- name: SetMediaPosition :exec
UPDATE media SET position = ? WHERE id = ?;

-- name: SetMediaTitle :exec
UPDATE media SET title = ? WHERE id = ?;

-- name: SetMediaImageDuration :exec
UPDATE media SET image_duration_s = ? WHERE id = ?;

-- name: SetMediaEnabled :exec
UPDATE media SET enabled = ? WHERE id = ?;

-- name: DeleteMedia :exec
DELETE FROM media WHERE id = ?;

-- name: SumMediaSize :one
SELECT CAST(COALESCE(SUM(size_bytes), 0) AS INTEGER) FROM media;

-- name: InsertJob :one
INSERT INTO jobs (source, src_path, orig_name, title, want_position, image_duration_s, tg_file_id, tg_chat_id, url, format, replaces_media_id, no_transcode)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: NextQueuedJob :one
SELECT * FROM jobs WHERE state = 'queued' ORDER BY id LIMIT 1;

-- name: ListActiveJobs :many
SELECT * FROM jobs WHERE state IN ('queued', 'running') ORDER BY id;

-- name: ListRecentJobs :many
SELECT * FROM jobs ORDER BY id DESC LIMIT ?;

-- name: SetJobRunning :execrows
UPDATE jobs SET state = 'running', phase = '', progress = 0, error = '', updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ? AND state = 'queued';

-- name: SetJobProgress :exec
UPDATE jobs SET phase = ?, progress = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?;

-- name: SetJobSrcPath :exec
UPDATE jobs SET src_path = ?, orig_name = ? WHERE id = ?;

-- name: SetJobProgressMessage :exec
UPDATE jobs SET tg_chat_id = ?, tg_progress_msg_id = ? WHERE id = ?;

-- name: SetJobDone :exec
UPDATE jobs SET state = 'done', phase = '', progress = 100, media_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?;

-- name: SetJobFailed :exec
UPDATE jobs SET state = 'failed', error = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?;

-- name: CancelJob :execrows
UPDATE jobs SET state = 'cancelled', updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE id = ? AND state IN ('queued', 'running');

-- name: TrimJobs :exec
-- Keeps the newest 100 finished jobs; waiting and running ones are never touched.
DELETE FROM jobs WHERE state IN ('done', 'failed', 'cancelled') AND id <= (
    SELECT id FROM jobs WHERE state IN ('done', 'failed', 'cancelled') ORDER BY id DESC LIMIT 1 OFFSET 100);

-- name: RequeueRunningJobs :exec
UPDATE jobs SET state = 'queued', progress = 0 WHERE state = 'running';

-- name: GetSetting :one
SELECT value FROM settings WHERE key = ?;

-- name: PutSetting :exec
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = ?;

-- name: InsertEvent :exec
INSERT INTO events (level, message) VALUES (?, ?);

-- name: ListEvents :many
SELECT * FROM events ORDER BY id DESC LIMIT ?;

-- name: TrimEvents :exec
-- Keeps the newest 200 log entries.
DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET 200);

-- name: InsertSession :exec
INSERT INTO sessions (token_hash, expires_at) VALUES (?, ?);

-- name: SessionValid :one
SELECT COUNT(*) FROM sessions WHERE token_hash = ? AND expires_at > strftime('%Y-%m-%dT%H:%M:%SZ', 'now');

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteAllSessions :exec
DELETE FROM sessions;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= strftime('%Y-%m-%dT%H:%M:%SZ', 'now');
