-- name: UpsertWebDAVCredential :one
INSERT INTO /* TEMPLATE: schema */webdav_credentials (
    user_id,
    username,
    password_hash
)
VALUES (
    sqlc.arg(user_id),
    sqlc.arg(username),
    sqlc.arg(password_hash)
)
ON CONFLICT (user_id) DO UPDATE
SET username = EXCLUDED.username,
    password_hash = EXCLUDED.password_hash,
    updated_at = now()
RETURNING *;

-- name: GetWebDAVCredentialByUser :one
SELECT *
FROM /* TEMPLATE: schema */webdav_credentials
WHERE user_id = sqlc.arg(user_id);

-- name: GetWebDAVCredentialByUsername :one
SELECT *
FROM /* TEMPLATE: schema */webdav_credentials
WHERE lower(username) = lower(sqlc.arg(username));

-- name: DeleteWebDAVCredential :exec
DELETE FROM /* TEMPLATE: schema */webdav_credentials
WHERE user_id = sqlc.arg(user_id);
