-- A migration that cannot run. The failure it produces is what the error-shape
-- and lock-release tests are built on: the run has to name this version and
-- this file, and it has to leave the advisory lock free for the next attempt.

-- +goose Up
alter table widgets add column name text;
this is not sql;

-- +goose Down
alter table widgets drop column name;
