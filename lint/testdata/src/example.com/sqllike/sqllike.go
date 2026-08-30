// Package sqllike exports Open and OpenDB with the same shapes as
// database/sql's, from a different import path. It exists so the clean fixture
// can prove gogadatabase keys on the import path and not on the identifier or
// the function name: a file may bind the name "sql" to this package, and
// sql.Open(…) then means something the rule has no business reporting.
package sqllike

// DB is this package's own handle type.
type DB struct{}

// Open looks exactly like database/sql's and is not it.
func Open(_, _ string) (*DB, error) { return &DB{}, nil }

// OpenDB looks exactly like database/sql's and is not it.
func OpenDB(_ any) *DB { return &DB{} }
