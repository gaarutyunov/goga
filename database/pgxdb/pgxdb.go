// Package pgxdb opens a PostgreSQL connection pool and hands back pgx's own
// *pgxpool.Pool, already instrumented.
//
// # Nothing is erased
//
// This is the half of goga's database access that keeps pgx whole. CopyFrom,
// SendBatch, LISTEN/NOTIFY, pgtype's native values and scanning without a
// driver.Value round-trip are all reached directly on the returned pool,
// because no interface sits in between pretending they are portable. There is
// nothing to unwrap and no capability check to pass: an earlier revision of the
// design had a portable handle with an Unwrap() any on it, and Unwrap existed
// only to escape the port, so both went together.
//
// [github.com/gaarutyunov/goga/database] is the other half, returning the
// standard library's *sql.DB for goose, for sqlc's database/sql mode and for
// every helper already written against that interface.
//
// # There is no bridge between the two, on purpose
//
// This package deliberately exposes no SQLDB(pool) *sql.DB and performs no
// stdlib.OpenDBFromPool dance. A caller that needs the standard interface calls
// [github.com/gaarutyunov/goga/database.Open]; a caller that needs the pool
// calls [Open] here. Two entry points, each returning the thing its caller
// actually wants, and each opened from the same [database.DSN].
//
// A bridge would look like it saved a connection and does not: the two handles
// keep separate pools either way, so the only thing it would add is a second way
// to obtain a *sql.DB whose instrumentation came from a different library.
//
// # Instrumentation
//
// [Open] installs github.com/exaring/otelpgx's tracer on the pool configuration
// before the pool is built, and registers pgx's pool statistics as metrics. No
// exported entry point of this package returns a pool without both, and no
// option — nor any value of any option — removes them. [WithTelemetry] replaces
// the module handle goga's own spans are recorded on; it cannot disable
// anything.
//
// As in goga/database, that is a property of this constructor rather than of the
// returned type: *pgxpool.Pool is pgx's, and anyone can pgxpool.New one
// uninstrumented. goga/lint's database rule and a depguard entry confining
// github.com/jackc/pgx imports to goga/database and its sub-packages are what
// carry the guarantee into project code.
//
// # A query is traced only inside a span
//
// otelpgx starts a span for a query only when the context handed to it already
// carries a recording span, and returns the context untouched otherwise. So a
// query issued from an HTTP handler — where goga/serve has already opened a
// server span — is traced, and the identical query issued from a background job
// or from func main is not.
//
// This differs from goga/database, whose otelsql wrapping traces every statement
// whether or not there is a parent, and it is worth knowing before concluding
// that the pool is uninstrumented: wrap the work in a span of your own, which is
// what [github.com/gaarutyunov/goga/telemetry.Instrumentation.Start] is for, and
// the queries inside it appear. Metrics are unaffected — the pool statistics
// registered here are recorded regardless.
//
// # One tracer field
//
// pgx.ConnConfig has exactly one Tracer field, and [Open] sets it. A project
// that also wants a tracer of its own — a query counter in a test, say — cannot
// have both through this constructor and must build that one pool by hand. That
// is a real limitation, stated here rather than discovered later.
package pgxdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/goforj/wire"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/database"
)

// moduleName is the goga module this package reports itself as to
// goga/telemetry.
//
// It is "database/pgxdb" and not "database", even though the two packages are
// one milestone's two halves. They are separate openers returning types with
// different capabilities, and a project may hold both at once; one name for both
// would put two different constructors behind one span name and one set of
// metric attributes, with nothing in the telemetry to tell them apart.
const moduleName = "database/pgxdb"

// errNilTelemetry answers WithTelemetry called with a nil handle. There is no
// option value that disables instrumentation, and nil is not a way to ask for
// one.
var errNilTelemetry = errors.New(
	"goga/database/pgxdb: telemetry must not be nil — instrumentation can be replaced but never disabled")

// Open opens a PostgreSQL connection pool and returns pgx's own *pgxpool.Pool,
// instrumented.
//
// The otelpgx tracer is installed on the pool's connection configuration before
// the pool is built, so no uninstrumented pool exists at any point, not even
// briefly. Pool statistics are registered as metrics on the same handle.
//
// Opening does not connect: pgxpool returns as soon as the configuration is
// valid and establishes connections on demand, so a bad host or a wrong password
// surfaces at the first acquire. Call pool.Ping to find out whether the database
// is reachable.
//
// The caller owns the returned pool and must Close it. Use [Set] to have wire
// generate that call.
//
// ctx is used for the construction span and is passed to pgxpool, which uses it
// to bound any connection it establishes eagerly; it does not bound the lifetime
// of the pool.
func Open(ctx context.Context, dsn database.DSN, opts ...Option) (*pgxpool.Pool, error) {
	pool, _, err := open(ctx, dsn, opts...)
	return pool, err
}

// openWithCleanup is the wire provider: the same pool, plus the func() that
// closes it.
//
// The cleanup's type is func() and not func() error, because func() is the only
// shape github.com/goforj/wire recognises as a provider's cleanup. pgxpool's
// Close returns nothing, so nothing is lost in the narrowing.
func openWithCleanup(ctx context.Context, dsn database.DSN, opts ...Option) (*pgxpool.Pool, func(), error) {
	pool, cleanup, err := open(ctx, dsn, opts...)
	if err != nil {
		return nil, nil, err
	}
	return pool, cleanup, nil
}

// open is the body [Open] and [openWithCleanup] share.
func open(ctx context.Context, dsn database.DSN, opts ...Option) (_ *pgxpool.Pool, _ func(), err error) {
	set, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("goga/database/pgxdb: open: %w", err)
	}

	_, end := set.instr.Start(ctx, "Open")
	defer func() { end(err) }()

	cfg, err := pgxpool.ParseConfig(string(dsn))
	if err != nil {
		return nil, nil, fmt.Errorf("goga/database/pgxdb: open: parsing dsn: %w", err)
	}
	set.applyTo(cfg)

	// Set after the caller's settings, and unconditionally: this line is the
	// whole of the guarantee that no uninstrumented pool leaves the package, so
	// nothing the caller passed may reach it.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("goga/database/pgxdb: open: %w", err)
	}

	if err = otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("goga/database/pgxdb: open: recording pool statistics: %w", err)
	}

	return pool, pool.Close, nil
}

// Set is the wire provider set for this module: a *pgxpool.Pool and the func()
// that closes it, built from a [database.DSN] the graph supplies.
//
// It provides [openWithCleanup] rather than [Open] so that the generated
// injector closes the pool. The provided type is *pgxpool.Pool either way, so
// nothing downstream in the graph changes; what changes is that a pool nothing
// closes cannot be built.
var Set = wire.NewSet(openWithCleanup)
