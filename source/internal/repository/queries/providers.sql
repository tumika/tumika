-- name: GetProvider :one
SELECT id, display_name, kind, enabled, config, created_at, updated_at
FROM providers
WHERE id = ?;

-- name: ListProviders :many
SELECT id, display_name, kind, enabled, config, created_at, updated_at
FROM providers
ORDER BY id;

-- Seeding is idempotent: the registry calls this at every boot. display_name,
-- kind and config come from the driver and are refreshed; enabled is the
-- operator's decision and is deliberately NOT overwritten.
-- name: UpsertProvider :exec
INSERT INTO providers (id, display_name, kind, enabled, config, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE
SET display_name = excluded.display_name,
    kind         = excluded.kind,
    config       = excluded.config,
    updated_at   = excluded.updated_at;

-- name: SetProviderEnabled :exec
UPDATE providers
SET enabled    = ?,
    updated_at = ?
WHERE id = ?;
