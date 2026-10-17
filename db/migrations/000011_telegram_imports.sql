-- +goose Up

-- Telegram document messages imported into TelDrive. A copied document is
-- stored under a new message ID, so this table is what lets a repeated channel
-- scan skip documents that were already imported. Rows go away with the
-- imported file, after which the message can be imported again.
CREATE TABLE /* TEMPLATE: schema */telegram_import_sources (
    user_id BIGINT NOT NULL REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE CASCADE,
    source_channel_id BIGINT NOT NULL,
    source_message_id BIGINT NOT NULL,
    file_id UUID NOT NULL REFERENCES /* TEMPLATE: schema */files(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, source_channel_id, source_message_id),
    CONSTRAINT telegram_import_sources_message_positive CHECK (source_message_id > 0)
);

CREATE INDEX telegram_import_sources_file_idx
    ON /* TEMPLATE: schema */telegram_import_sources (file_id);

-- +goose Down

DROP TABLE IF EXISTS /* TEMPLATE: schema */telegram_import_sources;
