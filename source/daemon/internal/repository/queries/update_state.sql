-- A single row, id = 1, enforced by a CHECK. It exists to survive the process
-- restart that completes an update, which is why it is not in memory.

-- name: GetUpdateState :one
SELECT status, from_version, to_version, boot_attempts, started_at, updated_at
FROM update_state
WHERE id = 1;

-- name: PutUpdateState :exec
UPDATE update_state
SET status        = ?,
    from_version  = ?,
    to_version    = ?,
    boot_attempts = ?,
    started_at    = ?,
    updated_at    = ?
WHERE id = 1;

-- One statement rather than a read-modify-write: this runs on every boot of a
-- pending update, which is exactly when the process is most likely to die
-- partway through.
-- name: IncrementBootAttempts :one
UPDATE update_state
SET boot_attempts = boot_attempts + 1,
    updated_at    = ?
WHERE id = 1
RETURNING status, from_version, to_version, boot_attempts, started_at, updated_at;

-- to_published_at is when the release named by to_version was published: the
-- recency floor the updater falls back to once that release's own document
-- stops being served, and NULL when there is no floor.
--
-- It is read and written on its own and never named by the three statements
-- above, because those run on the boot path, which happens BEFORE the
-- migrations: every column they name has to exist on the previous schema.
-- name: GetUpdateWatermark :one
SELECT to_published_at
FROM update_state
WHERE id = 1;

-- name: SetUpdateWatermark :exec
UPDATE update_state
SET to_published_at = ?
WHERE id = 1;
