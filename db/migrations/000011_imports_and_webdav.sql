-- +goose Up

CREATE TABLE /* TEMPLATE: schema */import_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id BIGINT NOT NULL REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE CASCADE,
    channel_id BIGINT NOT NULL,
    parent_id UUID,
    status TEXT NOT NULL DEFAULT 'pending',
    total INTEGER NOT NULL DEFAULT 0,
    imported INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT import_jobs_status_valid CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    CONSTRAINT import_jobs_counters_nonnegative CHECK (
        total >= 0 AND imported >= 0 AND skipped >= 0 AND failed >= 0
    )
);

CREATE INDEX import_jobs_user_idx
    ON /* TEMPLATE: schema */import_jobs (user_id, created_at DESC);

CREATE TABLE /* TEMPLATE: schema */import_items (
    job_id UUID NOT NULL REFERENCES /* TEMPLATE: schema */import_jobs(id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    size BIGINT NOT NULL,
    mime_type TEXT,
    mod_time TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'pending',
    file_id UUID,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, message_id),
    CONSTRAINT import_items_status_valid CHECK (status IN ('pending', 'imported', 'skipped', 'failed')),
    CONSTRAINT import_items_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT import_items_size_nonnegative CHECK (size >= 0)
);

CREATE TABLE /* TEMPLATE: schema */webdav_credentials (
    user_id BIGINT PRIMARY KEY REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE CASCADE,
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT webdav_credentials_username_not_blank CHECK (length(btrim(username)) > 0)
);

CREATE UNIQUE INDEX webdav_credentials_username_idx
    ON /* TEMPLATE: schema */webdav_credentials (lower(username));

-- +goose Down

DROP TABLE IF EXISTS /* TEMPLATE: schema */webdav_credentials;
DROP TABLE IF EXISTS /* TEMPLATE: schema */import_items;
DROP TABLE IF EXISTS /* TEMPLATE: schema */import_jobs;
