-- name: UpsertUser :one
-- Creates the user on first sight of an OIDC subject; refreshes e-mail, name and last-seen otherwise.
INSERT INTO users (id, subject, email, display_name, email_verified)
VALUES (@id, @subject, @email, @display_name, @email_verified)
ON CONFLICT (subject) DO UPDATE
    SET email          = EXCLUDED.email,
        display_name   = EXCLUDED.display_name,
        email_verified = EXCLUDED.email_verified,
        updated_at     = CASE WHEN (users.email, users.display_name, users.email_verified)
                                   IS DISTINCT FROM (EXCLUDED.email, EXCLUDED.display_name, EXCLUDED.email_verified)
                              THEN now() ELSE users.updated_at END,
        last_seen_at = now()
RETURNING *;

-- name: FindUsersByEmail :many
-- Verified addresses only (an unverified claim must never attach someone else's membership). At most two rows:
-- the caller only needs to know whether the address is unambiguous.
SELECT * FROM users WHERE lower(email) = lower(@email) AND email_verified ORDER BY id LIMIT 2;
