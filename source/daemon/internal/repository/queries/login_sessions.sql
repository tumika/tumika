-- name: CreateLoginSession :exec
INSERT INTO login_sessions (
    id, provider_id, state, auth_url, prompt, error_code, error_message,
    credential_id, child_pid, transcript, created_at, updated_at, expires_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLoginSession :one
SELECT id, provider_id, state, auth_url, prompt, error_code, error_message,
       credential_id, child_pid, transcript, created_at, updated_at, expires_at
FROM login_sessions
WHERE id = ?;

-- name: UpdateLoginSession :exec
UPDATE login_sessions
SET state         = ?,
    auth_url      = ?,
    prompt        = ?,
    error_code    = ?,
    error_message = ?,
    credential_id = ?,
    child_pid     = ?,
    transcript    = ?,
    updated_at    = ?,
    expires_at    = ?
WHERE id = ?;

-- Runs at daemon startup. A session's PTY and child process do not survive a
-- restart, so any row still mid-flight is describing something that no longer
-- exists.
-- name: FailAllNonTerminalLoginSessions :execrows
UPDATE login_sessions
SET state         = 'failed',
    error_code    = 'daemon_restarted',
    error_message = ?,
    updated_at    = ?
WHERE state NOT IN ('succeeded', 'failed', 'timed_out', 'canceled');
