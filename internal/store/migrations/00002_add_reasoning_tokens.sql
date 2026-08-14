-- +goose Up
ALTER TABLE activity RENAME COLUMN output_tokens TO generated_tokens;
ALTER TABLE activity ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE activity ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;

-- Historical output_tokens contained the full generated token count. There is
-- no persisted reasoning split to recover, so preserve that total as visible
-- output and leave reasoning_tokens at zero.
UPDATE activity SET output_tokens = generated_tokens;

-- +goose Down
ALTER TABLE activity DROP COLUMN output_tokens;
ALTER TABLE activity DROP COLUMN reasoning_tokens;
ALTER TABLE activity RENAME COLUMN generated_tokens TO output_tokens;
