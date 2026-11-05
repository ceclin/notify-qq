-- +goose Up
ALTER TABLE api_keys RENAME COLUMN user_openid TO recipient_openid;
ALTER TABLE api_keys ADD COLUMN recipient_type TEXT NOT NULL DEFAULT ''
    CHECK (recipient_type IN ('', 'user', 'group'));
UPDATE api_keys SET recipient_type = 'user' WHERE recipient_openid <> '';
