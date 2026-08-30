-- The first of the two migrations most of the tests in this package run. It
-- creates a table rather than doing nothing, so that a run applied twice fails
-- loudly on the second attempt instead of quietly succeeding.

-- +goose Up
create table widgets (
    id bigint primary key
);

-- +goose Down
drop table widgets;
