// Package database is a fixture stub of goga/database, and simultaneously the
// fixture for the package gogadatabase must never report: the constructor that
// attaches the instrumentation necessarily calls the very functions every other
// package is forbidden to call. It carries no `want` comment, so analysistest
// fails if the rule ever starts firing here.
//
// It declares only the surface the clean fixtures call, and it is not a model
// of the real package.
package database

import (
	"context"
	"database/sql"
)

// DSN is the connection string both constructors take.
type DSN string

// Open returns the standard library's handle, wrapped. Both spellings appear
// because both are in the rule's trigger set and both are correct here.
func Open(_ context.Context, dsn DSN) (*sql.DB, error) {
	db, err := sql.Open("pgx", string(dsn))
	if err != nil {
		return nil, err
	}
	return db, nil
}

// OpenFromConnector is the connector-taking form, which is what the real
// package uses once it has wrapped the driver.
func OpenFromConnector(c driverConnector) *sql.DB { return sql.OpenDB(c) }
