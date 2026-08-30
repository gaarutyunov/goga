// Package dbdep is a dependency that opens its own database handle. It belongs
// to another module, so gogadatabase must stay silent here: how a dependency
// opens its handles is not the adopting project's to police, and a rule that
// reported it would fire on code the project cannot change.
package dbdep

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenDB opens a handle the way any library outside the module may.
func OpenDB(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }

// OpenPool opens a pool the way any library outside the module may.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn)
}
