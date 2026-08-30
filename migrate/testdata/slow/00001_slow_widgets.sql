-- A migration that takes measurable time, so that a duration recorded for it
-- can be told apart from zero and from the duration of the migration beside it.

-- +goose Up
select pg_sleep(0.4);
create table slow_widgets (
    id bigint primary key
);

-- +goose Down
drop table slow_widgets;
