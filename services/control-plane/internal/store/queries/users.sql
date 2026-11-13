-- name: UpsertUser :one
-- Creates the user on first sight of an OIDC subject; refreshes e-mail, name and last-seen otherwise.
INSERT INTO users (id, subject, email, display_name)
VALUES (@id, @subject, @email, @display_name)
ON CONFLICT (subject) DO UPDATE
    SET email        = EXCLUDED.email,
        display_name = EXCLUDED.display_name,
        updated_at   = CASE WHEN (users.email, users.display_name) IS DISTINCT FROM (EXCLUDED.email, EXCLUDED.display_name)
                            THEN now() ELSE users.updated_at END,
        last_seen_at = now()
RETURNING *;

-- name: FindUsersByEmail :many
-- At most two rows: the caller only needs to know whether the address is unambiguous.
SELECT * FROM users WHERE lower(email) = lower(@email) ORDER BY id LIMIT 2;
