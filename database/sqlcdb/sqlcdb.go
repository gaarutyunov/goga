// Package sqlcdb is the seam between sqlc's generated code and goga's
// instrumented pgx pool.
//
// It is one interface and three compile-time assertions. There is no
// constructor, no adapter and no error, because there is nothing for any of
// them to do.
//
// # Why the package is this small
//
// sqlc's pgx/v5 mode generates a New(db DBTX) *Queries and declares DBTX itself,
// in pgx's own types. An earlier revision of goga's database design put a
// portable *database.DB in front of pgx, which meant this seam had to be a
// New(*database.DB) (DBTX, error) that inspected the adapter underneath and
// returned an ErrNotPgx when it was not pgx — a run-time failure for a condition
// that was decided at build time, in the composition root, several layers away
// from the call that reported it.
//
// With the port gone, that failure mode does not exist. [github.com/jackc/pgx/v5/pgxpool.Pool]
// satisfies [DBTX] directly, so generated sqlc queries run on goga's
// instrumented pool with no conversion, no capability check, no goga type in the
// way and not one generated line changed. The caller either has a pool or does
// not, and the compiler knows which — a run-time error for something the type
// system already prevents is strictly worse than no error at all.
//
// So there is no ErrNotPgx here, and there is nothing to call. Import
// [github.com/gaarutyunov/goga/database/pgxdb], pass the pool it returns
// straight to your generated New, and the queries inherit the pool's telemetry.
//
// # What the assertions are for
//
// [DBTX] is a copy of the interface sqlc generates, and a copy can drift from
// its original when pgx changes a signature. The assertions below are what turn
// that drift into a compile error in goga's own build, rather than into a
// mismatch discovered in an adopting project. That is the whole package.
package sqlcdb

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the interface sqlc's pgx/v5 mode generates its queries against.
//
// The signatures are pgx's, so this seam is pgx-only and says so rather than
// being documented as adapter-neutral. sqlc's database/sql mode generates a
// different DBTX, in database/sql's types, which
// [github.com/gaarutyunov/goga/database.Open]'s *sql.DB satisfies for exactly
// the same reason and with exactly as little code.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

// The whole seam.
//
// The pool is the one that matters: it is what
// [github.com/gaarutyunov/goga/database/pgxdb.Open] returns, and the assertion
// is the proof that generated sqlc code takes it unmodified.
//
// The other two are the shapes the same generated code is used with in practice
// and which therefore have to keep working: a transaction, so that a *Queries
// can be built inside
// [github.com/gaarutyunov/goga/database/pgxdb.Tx] with q.WithTx(tx), and a
// single connection, for the session-scoped work — advisory locks, temporary
// tables, LISTEN — that has to run on one connection rather than on the pool.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (*pgxpool.Conn)(nil)
	_ DBTX = (pgx.Tx)(nil)
)
