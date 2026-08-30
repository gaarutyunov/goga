// Package dbopen is goga's own library code opening database handles itself.
// It is the violating fixture: all four constructors in the trigger set, each
// reported once.
package dbopen

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Handle takes the obvious spelling.
func Handle(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn) // want `opening a handle with sql\.Open bypasses database\.Open`
}

// FromConnector takes the connector-taking spelling, which is the one a rule
// naming only Open would miss.
func FromConnector(c connector) *sql.DB {
	return sql.OpenDB(c) // want `opening a handle with sql\.OpenDB bypasses database\.Open`
}

// Pool takes the pgx pool.
func Pool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn) // want `opening a handle with pgxpool\.New bypasses pgxdb\.Open`
}

// PoolFromConfig parses first — the parse is fine, the open is not.
func PoolFromConfig(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg) // want `opening a handle with pgxpool\.NewWithConfig bypasses pgxdb\.Open`
}
