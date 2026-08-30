// An aliased import is the same bypass spelled differently. The rule keys on
// the import path, so the alias changes nothing — and the diagnostic still
// names the package's own name rather than the local alias, because "sql.Open"
// is what the reader has to go and find.
package dbopen

import (
	"context"

	stdsql "database/sql"

	pool "github.com/jackc/pgx/v5/pgxpool"
)

// AliasedHandle opens through an aliased database/sql.
func AliasedHandle(dsn string) (*stdsql.DB, error) {
	return stdsql.Open("pgx", dsn) // want `opening a handle with sql\.Open bypasses database\.Open`
}

// AliasedPool opens through an aliased pgxpool.
func AliasedPool(ctx context.Context, dsn string) (*pool.Pool, error) {
	return pool.New(ctx, dsn) // want `opening a handle with pgxpool\.New bypasses pgxdb\.Open`
}
