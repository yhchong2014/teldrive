-- name: ListImportedTelegramMessages :many
SELECT source_message_id
FROM /* TEMPLATE: schema */telegram_import_sources
WHERE user_id = sqlc.arg(user_id)
  AND source_channel_id = sqlc.arg(source_channel_id)
  AND source_message_id = ANY(sqlc.arg(message_ids)::bigint[]);

-- name: RecordTelegramImport :exec
INSERT INTO /* TEMPLATE: schema */telegram_import_sources (
    user_id,
    source_channel_id,
    source_message_id,
    file_id
) VALUES (
    sqlc.arg(user_id),
    sqlc.arg(source_channel_id),
    sqlc.arg(source_message_id),
    sqlc.arg(file_id)
)
ON CONFLICT (user_id, source_channel_id, source_message_id) DO NOTHING;
