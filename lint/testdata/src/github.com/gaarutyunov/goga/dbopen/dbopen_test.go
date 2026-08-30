// A test file is NOT exempt, in deliberate disagreement with gogaconfig and in
// agreement with gogaserve. The handle a test opens is as uninstrumented as any
// other, database.Open takes the same DSN and returns the same type, and this
// module has no portable type to fall back on — so a test helper is exactly
// where an uninstrumented handle gets built once and then shared. See
// databaseDoc for the whole argument.
package dbopen

import (
	"database/sql"
	"testing"
)

func TestOpensItsOwnHandle(t *testing.T) {
	db, err := sql.Open("pgx", "") // want `opening a handle with sql\.Open bypasses database\.Open`
	if err != nil {
		t.Fatal(err)
	}
	_ = db
}
