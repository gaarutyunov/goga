-- The fast one that runs straight after the slow one. Its recorded duration is
-- what proves each migration's closer holds its own start time: a closer built
-- from the run's start would report this migration as having taken as long as
-- the one before it.

-- +goose Up
create table fast_widgets (
    id bigint primary key
);

-- +goose Down
drop table fast_widgets;
