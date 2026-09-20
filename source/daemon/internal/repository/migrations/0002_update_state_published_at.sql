-- The publication time of the release an update installed.
--
-- It is the recency floor a daemon falls back to when its own release document
-- is no longer published: edge releases are pruned, and without a floor a host
-- that serves a 404 for the running build's document while replaying a genuine
-- older channel head walks the daemon backwards.
--
-- NULL means no watermark: a first install, or a row written by a build that
-- did not record one. Such a daemon dates itself at nothing, which is what lets
-- it take the head rather than being pinned to a release it cannot date.

-- +goose Up

ALTER TABLE update_state ADD COLUMN to_published_at TEXT;

-- +goose Down

ALTER TABLE update_state DROP COLUMN to_published_at;
