-- "Live" means the credential occupies the one slot per (provider, kind) that
-- the partial unique index permits: in use, or sealed and awaiting its first
-- verification. domain.CredentialStatus.Live() must agree with this predicate.

-- name: GetLiveCredential :one
SELECT id, provider_id, kind, ciphertext, nonce, key_ref, cipher,
       hint, account_email, status, issued_at, expires_at, expiry_is_estimate,
       last_verified_at, last_verify_error, created_at, updated_at
FROM provider_credentials
WHERE provider_id = ?
  AND kind = ?
  AND status IN ('active', 'unverified');

-- name: ListLiveCredentials :many
SELECT id, provider_id, kind, ciphertext, nonce, key_ref, cipher,
       hint, account_email, status, issued_at, expires_at, expiry_is_estimate,
       last_verified_at, last_verify_error, created_at, updated_at
FROM provider_credentials
WHERE status IN ('active', 'unverified')
ORDER BY provider_id, kind;

-- name: InsertCredential :one
INSERT INTO provider_credentials (
    provider_id, kind, ciphertext, nonce, key_ref, cipher,
    hint, account_email, status, issued_at, expires_at, expiry_is_estimate,
    last_verified_at, last_verify_error, created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- Guarded on liveness, and reporting rows affected, because verification
-- deliberately runs outside a transaction: the row can be retired by a DELETE or
-- replaced by another submission while the provider is being called. Without the
-- guard, a late verdict writes 'active' back onto a revoked row and resurrects
-- it. Zero rows means "superseded", which is a normal outcome, not an error.
-- name: UpdateCredentialStatus :execrows
UPDATE provider_credentials
SET status            = ?,
    last_verify_error = ?,
    updated_at        = ?
WHERE id = sqlc.arg(id)
  AND status IN ('active', 'unverified');

-- Writes every field, and is guarded on liveness like the status update.
--
-- MERGING is the caller's job, not this query's: which fields a driver is
-- entitled to overwrite is a business rule, and it lives in ProviderService
-- where the stored metadata is already in hand. Doing it in SQL would also have
-- meant COALESCE/NULLIF, which sqlc's SQLite parser cannot name parameters
-- inside.
-- name: UpdateCredentialMeta :execrows
UPDATE provider_credentials
SET hint               = ?,
    account_email      = ?,
    issued_at          = ?,
    expires_at         = ?,
    expiry_is_estimate = ?,
    last_verified_at   = ?,
    updated_at         = ?
WHERE id = ?
  AND status IN ('active', 'unverified');

-- Frees the slot the partial unique index guards, so a replacement credential
-- can be inserted. Superseded rows are kept, not deleted: what was tried is
-- part of the record.
-- name: RetireCredentials :exec
UPDATE provider_credentials
SET status     = ?,
    updated_at = ?
WHERE provider_id = ?
  AND kind = ?
  AND status IN ('active', 'unverified');

-- name: DeleteCredential :exec
DELETE FROM provider_credentials
WHERE id = ?;
