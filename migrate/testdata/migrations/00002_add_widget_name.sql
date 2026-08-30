-- The second migration, so that a run has more than one step and the tests can
-- tell a per-migration span from a per-run one.

-- +goose Up
alter table widgets add column name text;

-- +goose Down
alter table widgets drop column name;
