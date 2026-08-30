// Package pgxdb is a fixture stub of goga/database/pgxdb. It is the second half
// of the owner exemption and the one a segment match would get wrong: the rule
// must stay silent for the whole goga/database SUBTREE, not merely for the
// directory whose last segment is "database". It carries no `want` comment.
package pgxdb

import (
	"context"

	"github.com/gaarutyunov/goga/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns pgx's own pool with the tracer already installed. Both trigger
// spellings appear because both are correct here.
func Open(ctx context.Context, dsn database.DSN) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(string(dsn))
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// OpenSimple is the connection-string form.
func OpenSimple(ctx context.Context, dsn database.DSN) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, string(dsn))
}
