-- Initial schema.
--
-- Conventions used throughout, and deliberately:
--
--   * Timestamps are TEXT in RFC3339 UTC. SQLite has no date type; storing text
--     keeps the file readable during support, and UTC RFC3339 sorts correctly
--     lexicographically, so ORDER BY and range comparisons work without a
--     conversion. Nullable columns mean "not known yet", never zero-time.
--   * Booleans are INTEGER 0/1 with a CHECK, because SQLite has no boolean.
--   * Enumerations are TEXT with a CHECK listing the permitted values. The Go
--     constants in source/internal/domain are the other half of each of these;
--     changing one means changing both, in the same migration.
--   * Tables are not STRICT: sqlc's SQLite parser is the constraint here, and
--     the CHECKs recover most of what STRICT would have given us.

-- +goose Up

-- Generic key/value configuration. Values are JSON so a new knob needs no
-- migration  -  which is the entire point of the table.
CREATE TABLE settings (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Providers are seeded from the driver registry at boot, not here: the registry
-- is the source of truth for which providers exist, and this table holds only
-- the mutable half an operator can change.
CREATE TABLE providers (
    id           TEXT    NOT NULL PRIMARY KEY,
    display_name TEXT    NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('cli', 'http')),
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    config       TEXT    NOT NULL DEFAULT '{}',
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL
);

-- Credentials are stored sealed: AES-256-GCM ciphertext lives here, and only
-- the key leaves, into whichever custody backend the platform provides
-- (ADR-0002). key_ref records which backend sealed the row, so re-keying after
-- a platform change is a query rather than an excavation.
CREATE TABLE provider_credentials (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id        TEXT    NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    kind               TEXT    NOT NULL CHECK (kind IN ('oauth_token', 'api_key')),

    ciphertext         BLOB    NOT NULL,
    nonce              BLOB    NOT NULL,
    key_ref            TEXT    NOT NULL,
    cipher             TEXT    NOT NULL DEFAULT 'aes-256-gcm',

    hint               TEXT    NOT NULL DEFAULT '',
    account_email      TEXT    NOT NULL DEFAULT '',
    status             TEXT    NOT NULL CHECK (
                           status IN ('unverified', 'active', 'invalid', 'expired', 'revoked')
                       ),
    issued_at          TEXT,
    expires_at         TEXT,
    -- The OAuth token is opaque, so its expiry is usually inferred as
    -- issued_at + 365d rather than read. Clients must be able to tell the
    -- difference between a known expiry and a guess.
    expiry_is_estimate INTEGER NOT NULL DEFAULT 0 CHECK (expiry_is_estimate IN (0, 1)),
    last_verified_at   TEXT,
    last_verify_error  TEXT    NOT NULL DEFAULT '',

    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL
);

-- A provider has at most ONE live credential per kind. Superseded credentials
-- are kept with a terminal status rather than deleted, so the history of what
-- was tried survives; the partial index is what stops two of them being live at
-- once. domain.CredentialStatus.Live() is the Go statement of this predicate,
-- and the two must not drift.
CREATE UNIQUE INDEX provider_credentials_live
    ON provider_credentials (provider_id, kind)
    WHERE status IN ('active', 'unverified');

CREATE INDEX provider_credentials_by_provider
    ON provider_credentials (provider_id, status);

-- Only interactive auth methods create a row here. A session cannot survive a
-- daemon restart, because its PTY and child process do not  -  every non-terminal
-- row is failed at startup.
CREATE TABLE login_sessions (
    id            TEXT    NOT NULL PRIMARY KEY,
    provider_id   TEXT    NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    state         TEXT    NOT NULL CHECK (
                      state IN (
                          'pending', 'launching', 'awaiting_browser', 'awaiting_code',
                          'verifying', 'succeeded', 'failed', 'timed_out', 'canceled'
                      )
                  ),

    auth_url      TEXT    NOT NULL DEFAULT '',
    prompt        TEXT    NOT NULL DEFAULT '',
    error_code    TEXT    NOT NULL DEFAULT '',
    error_message TEXT    NOT NULL DEFAULT '',

    credential_id INTEGER REFERENCES provider_credentials (id) ON DELETE SET NULL,
    child_pid     INTEGER,
    -- Redacted at capture time, before it is written here. It contains the
    -- token by construction; scrubbing it afterwards would be too late.
    transcript    TEXT    NOT NULL DEFAULT '',

    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    expires_at    TEXT    NOT NULL
);

-- One in-flight login per provider, enforced here rather than by a service-layer
-- check that races with itself.
CREATE UNIQUE INDEX login_sessions_one_in_flight
    ON login_sessions (provider_id)
    WHERE state NOT IN ('succeeded', 'failed', 'timed_out', 'canceled');

-- A single row, guarded by CHECK (id = 1). It exists to survive the process
-- restart that completes an update  -  which is exactly why it cannot live in
-- memory (ADR-0003).
CREATE TABLE update_state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    status        TEXT    NOT NULL CHECK (
                      status IN ('idle', 'pending', 'confirmed', 'rolled_back')
                  ),
    from_version  TEXT    NOT NULL DEFAULT '',
    to_version    TEXT    NOT NULL DEFAULT '',
    boot_attempts INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT,
    updated_at    TEXT    NOT NULL
);

INSERT INTO update_state (id, status, updated_at)
VALUES (1, 'idle', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

-- +goose Down

DROP TABLE update_state;
DROP INDEX login_sessions_one_in_flight;
DROP TABLE login_sessions;
DROP INDEX provider_credentials_by_provider;
DROP INDEX provider_credentials_live;
DROP TABLE provider_credentials;
DROP TABLE providers;
DROP TABLE settings;
