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
