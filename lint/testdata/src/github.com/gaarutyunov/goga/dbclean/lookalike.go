// A different module's package, bound to the name "sql". Every selector below
// reads exactly like the defect and is not it, which is what proves the rule
// keys on the import path rather than on the identifier or the function name.
package dbclean

import sql "example.com/sqllike"

// Lookalike opens something that is not a database handle.
func Lookalike(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	_ = sql.OpenDB(db)
	return db, nil
}
