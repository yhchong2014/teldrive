-- name: CreateImportJob :one
INSERT INTO /* TEMPLATE: schema */import_jobs (
    id,
    user_id,
    channel_id,
    parent_id,
    status,
    total
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.arg(channel_id),
    sqlc.narg(parent_id),
    'pending',
    sqlc.arg(total)
)
RETURNING *;

-- name: CreateImportItem :exec
INSERT INTO /* TEMPLATE: schema */import_items (
    job_id,
    message_id,
    name,
    size,
    mime_type,
    mod_time
)
VALUES (
    sqlc.arg(job_id),
    sqlc.arg(message_id),
    sqlc.arg(name),
    sqlc.arg(size),
    sqlc.narg(mime_type),
    sqlc.arg(mod_time)
)
ON CONFLICT (job_id, message_id) DO NOTHING;

-- name: GetImportJob :one
SELECT *
FROM /* TEMPLATE: schema */import_jobs
WHERE id = sqlc.arg(job_id)
  AND user_id = sqlc.arg(user_id);

-- name: ListImportJobs :many
SELECT *
FROM /* TEMPLATE: schema */import_jobs
WHERE user_id = sqlc.arg(user_id)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_size);

-- name: UpdateImportJobStatus :one
UPDATE /* TEMPLATE: schema */import_jobs
SET status = sqlc.arg(status),
    error = sqlc.narg(error),
    updated_at = now()
WHERE id = sqlc.arg(job_id)
RETURNING *;

-- name: IncrementImportJobProgress :one
UPDATE /* TEMPLATE: schema */import_jobs
SET imported = imported + sqlc.arg(imported)::integer,
    skipped = skipped + sqlc.arg(skipped)::integer,
    failed = failed + sqlc.arg(failed)::integer,
    updated_at = now()
WHERE id = sqlc.arg(job_id)
RETURNING *;

-- name: ListPendingImportItems :many
SELECT *
FROM /* TEMPLATE: schema */import_items
WHERE job_id = sqlc.arg(job_id)
  AND status = 'pending'
ORDER BY message_id;

-- name: MarkImportItemImported :exec
UPDATE /* TEMPLATE: schema */import_items
SET status = 'imported',
    file_id = sqlc.arg(file_id),
    updated_at = now()
WHERE job_id = sqlc.arg(job_id)
  AND message_id = sqlc.arg(message_id);

-- name: MarkImportItemFailed :exec
UPDATE /* TEMPLATE: schema */import_items
SET status = sqlc.arg(status),
    error = sqlc.narg(error),
    updated_at = now()
WHERE job_id = sqlc.arg(job_id)
  AND message_id = sqlc.arg(message_id);

-- name: ListIndexedMessageIDs :many
SELECT DISTINCT fp.message_id
FROM /* TEMPLATE: schema */file_parts fp
JOIN /* TEMPLATE: schema */files f ON f.id = fp.file_id
WHERE f.user_id = sqlc.arg(user_id)
  AND fp.channel_id = sqlc.arg(channel_id)
  AND fp.message_id = ANY(sqlc.arg(message_ids)::bigint[]);

-- name: InsertImportedFile :one
INSERT INTO /* TEMPLATE: schema */files (
    id,
    user_id,
    parent_id,
    name,
    kind,
    mime_type,
    size,
    encryption,
    status,
    mod_time
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(user_id),
    sqlc.narg(parent_id),
    sqlc.arg(name),
    'file',
    sqlc.narg(mime_type),
    sqlc.arg(size),
    FALSE,
    'active',
    sqlc.arg(mod_time)
)
RETURNING *;

-- name: InsertImportedFilePart :exec
INSERT INTO /* TEMPLATE: schema */file_parts (
    file_id,
    part_no,
    channel_id,
    message_id,
    plain_size,
    stored_size
)
VALUES (
    sqlc.arg(file_id),
    sqlc.arg(part_no),
    sqlc.arg(channel_id),
    sqlc.arg(message_id),
    sqlc.arg(plain_size),
    sqlc.arg(stored_size)
)
ON CONFLICT (channel_id, message_id) DO NOTHING;
