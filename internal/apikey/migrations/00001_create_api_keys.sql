-- +goose Up
CREATE TABLE api_keys (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    digest BLOB NOT NULL UNIQUE CHECK(length(digest) = 32),
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    user_openid TEXT NOT NULL DEFAULT '',
    pairing_digest BLOB UNIQUE CHECK(pairing_digest IS NULL OR length(pairing_digest) = 32),
    pairing_expires_at TEXT
);


-- +goose Down
DROP TABLE api_keys;
