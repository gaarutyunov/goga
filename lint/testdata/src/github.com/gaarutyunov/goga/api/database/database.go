// Package database here is the second lookalike, and the one that decides how
// the owner exemption is written: its LAST SEGMENT is "database", but its
// position in the tree is not goga/database. A segment match anywhere in the
// path would exempt it, and exempting a package because of its name is how a
// rule quietly stops covering the code it was written for.
package database

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is a bypass like any other.
func Pool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn) // want `opening a handle with pgxpool\.New bypasses pgxdb\.Open`
}
