// Package databaseutil is the lookalike whose NAME starts with the owner
// package's. It is ordinary project code and the rule must fire here, which is
// what separates "the package that owns the handle" from "any package whose
// name mentions the database".
package databaseutil

import "database/sql"

// Open is a bypass like any other.
func Open(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn) // want `opening a handle with sql\.Open bypasses database\.Open`
}
