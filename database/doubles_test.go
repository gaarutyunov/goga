package database_test

//go:generate mockgen -source=doubles_test.go -destination=mock_driver_test.go -package=database_test

import (
	"database/sql/driver"
)

// The seam the transaction tests run over.
//
// [database.Tx] is a free function over the standard library's *sql.DB, so the
// only thing between it and the database is database/sql's own driver
// interfaces. Mocking those is what lets the commit, rollback and panic
// behaviour be asserted on every commit, with no container and no Docker: a test
// that needs a database to run is a test that runs later, and the panic path is
// the one that must not wait.
//
// The container-backed tests behind the `integration` build tag re-run the same
// scenarios against a real PostgreSQL and against pgxdb, so what is asserted
// here cannot silently diverge from what the real driver does.
//
// The three interfaces below exist because mockgen generates one mock per named
// interface and database/sql needs a connection that is both a [driver.Conn] and
// a [driver.ConnBeginTx]; there is no such named interface in the standard
// library, so the composition is written out here.

// Driver is [driver.Driver], named so that mockgen can generate a mock of it.
type Driver interface {
	driver.Driver
}

// Conn is a connection that supports context-aware transactions: the shape
// database/sql needs before it will pass isolation level and read-only through
// to the driver.
type Conn interface {
	driver.Conn
	driver.ConnBeginTx
}

// Tx is [driver.Tx], named so that mockgen can generate a mock of it.
type Tx interface {
	driver.Tx
}
