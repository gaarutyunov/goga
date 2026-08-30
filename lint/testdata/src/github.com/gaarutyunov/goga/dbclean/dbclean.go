// Package dbclean is the clean fixture: correct usage, and every near miss the
// rule must not mistake for the defect. It carries no `want` comment anywhere,
// so analysistest fails on any diagnostic reported here.
package dbclean

import (
	"context"
	"database/sql"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/database/pgxdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Correct is the usage the rule exists to route people to. It names *sql.DB,
// which is why database/sql can never be banned by import path: the type is
// what goga's own constructor returns.
func Correct(ctx context.Context, dsn database.DSN) (*sql.DB, error) {
	return database.Open(ctx, dsn)
}

// CorrectPool is the other half, and names *pgxpool.Pool for the same reason.
func CorrectPool(ctx context.Context, dsn database.DSN) (*pgxpool.Pool, error) {
	return pgxdb.Open(ctx, dsn)
}

// referenced names a constructor without calling it. Only a call is the defect,
// so this stays quiet.
var referenced = sql.Open

// NearMisses calls every other function of both packages. None of them opens
// anything, and a rule that had widened to "any sql.* call" would report them.
func NearMisses(dsn string) ([]string, *pgxpool.Config, error) {
	_ = sql.Named("id", 1)
	_ = sql.ErrNoRows
	_ = referenced

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, err
	}

	return sql.Drivers(), cfg, nil
}

// store has methods whose names collide with the trigger set. The selector is
// on a value rather than on the imported package, so nothing here is reported.
type store struct{}

// Open collides with sql.Open by name only.
func (s store) Open(string) error { return nil }

// New collides with pgxpool.New by name only.
func (s store) New(context.Context) error { return nil }

// Collisions calls both.
func Collisions(ctx context.Context) error {
	var s store
	if err := s.Open("dsn"); err != nil {
		return err
	}
	return s.New(ctx)
}
