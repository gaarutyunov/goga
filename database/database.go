// Package database opens a PostgreSQL database and hands back the standard
// library's *sql.DB, already instrumented.
//
// # There is no port here, and that is the point
//
// Every other runtime module in goga is built around a port: a small interface
// with one or more implementations behind it. This one is not. An earlier
// revision of the design specified a six-method driver.DB with pgx and
// database/sql adapters behind it, a portable *database.DB in front of it and a
// URL scheme to select between them. All of it is gone.
//
// The evidence that killed it is gocloud.dev, which is the model this design
// follows everywhere else. It is an eight-year-old portability library that
// ships driver-based ports for blobs, queues, documents, secrets and runtime
// configuration — and it declined to build one for SQL. Its postgres/postgres.go
// returns *sql.DB and instruments by wrapping the sql driver underneath. The
// reason is the same one that makes such a port attractive and then hollow:
// pgx's value over database/sql *is* the part a common interface erases —
// CopyFrom, SendBatch, LISTEN/NOTIFY, native types — so a port spanning the two
// either drops those or routes them through an escape hatch that admits the
// port was the wrong shape.
//
// So goga ships two honest return types instead of one lossy union:
//
//   - This package, for everything that speaks database/sql: goose, sqlc's
//     database/sql mode, and every helper already written against *sql.DB.
//   - [github.com/gaarutyunov/goga/database/pgxdb], which returns pgx's own
//     *pgxpool.Pool with nothing erased and nothing to unwrap.
//
// Choosing between them is an import, made in the composition root and checked
// by the compiler. There is deliberately no adapter table, no Register(scheme,
// …), no Schemes() and no UnknownSchemeError: a [DSN] is content handed to one
// known driver, never a selector that picks an implementation. Encoding a
// compile-time fact as a runtime string costs compile-time checking and buys
// late binding goga does not use.
//
// # The instrumentation guarantee is real, but it is weaker here than elsewhere
//
// [Open] instruments by wrapping the sql *driver*, exactly as gocloud.dev does,
// so no uninstrumented handle leaves this package: there is no exported entry
// point that returns one, and no option — nor any value of any option — that
// removes the wrapping. [WithTelemetry] replaces the module handle goga's own
// spans are recorded on; it cannot disable anything.
//
// Unlike every other goga module, though, that is a property of this
// *constructor* rather than of the returned *type*. *sql.DB is the standard
// library's, and any caller can sql.Open one with no instrumentation at all, so
// holding a *sql.DB does not by itself prove it came from here. For this one
// module the enforcement drops from compile-time to lint-time: goga/lint's
// database rule reports sql.Open, sql.OpenDB and pgxpool.New in project code,
// and depguard confines github.com/jackc/pgx imports to goga/database and its
// sub-packages. That is stated plainly rather than described as structural,
// because a claim a reader can disprove in four lines of Go costs more than it
// buys.
//
// # PostgreSQL, one driver
//
// The driver underneath is pgx's database/sql compatibility layer. There is one,
// and the DSN is handed to it as configuration. A second database engine would
// arrive as a second package with its own opener, the way pgxdb did, rather than
// as a scheme in a table here.
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/XSAM/otelsql"
	"github.com/goforj/wire"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/gaarutyunov/goga"
)

// moduleName is the goga module this package reports itself as to
// goga/telemetry.
const moduleName = "database"

var (
	// errNilTelemetry answers WithTelemetry called with a nil handle. There is
	// no option value that disables instrumentation, and nil is not a way to
	// ask for one.
	errNilTelemetry = errors.New(
		"goga/database: telemetry must not be nil — instrumentation can be replaced but never disabled")

	// errDriverNotWrapped answers the unreachable case where the instrumented
	// driver cannot open a connector. It exists so that the impossible path
	// returns an error rather than panicking on a type assertion.
	errDriverNotWrapped = errors.New(
		"goga/database: the instrumented driver does not implement driver.DriverContext")
)

// DSN is a PostgreSQL connection string, in either of the two forms pgx accepts:
// a URL ("postgres://user:pass@host:5432/db?sslmode=disable") or a keyword/value
// string ("host=localhost port=5432 dbname=db").
//
// It is a named type rather than a bare string so that a dependency-injection
// graph can supply it unambiguously: a wire provider set that also carries a
// listen address and a service name would otherwise have three providers of
// string and no way to tell them apart.
//
// The DSN is content, handed to one known driver. Its scheme does not select an
// implementation, because there is no implementation to select — see the package
// documentation.
//
// An empty DSN is not an error. pgx reads the standard libpq environment
// variables (PGHOST, PGPORT, PGDATABASE, PGUSER, …) for anything the string does
// not say, and an empty string means "take all of it from the environment",
// which is how a twelve-factor deployment usually configures a database.
type DSN string

// Open opens a PostgreSQL database and returns the standard library's *sql.DB,
// instrumented.
//
// The return type is the standard library's on purpose. There is nothing a goga
// wrapper would add that otelsql and database/sql do not already do, and a
// wrapper would make goose, sqlc and every existing helper take an unwrap step
// before they could be used at all.
//
// Instrumentation is applied by wrapping the sql driver, so every statement,
// transaction and connection attempt made through the returned handle produces a
// span and records its duration, and pool statistics are observed as metrics.
// See the package documentation for the exact strength of that guarantee.
//
// Opening does not connect. database/sql establishes connections lazily, so a
// bad host or a wrong password surfaces at the first query rather than here;
// only a DSN the driver cannot parse fails at this point. Call db.PingContext to
// find out whether the database is reachable.
//
// The caller owns the returned handle and must Close it. Use [Set] to have wire
// generate that call.
//
// ctx is used for the construction span; it is not retained, and it does not
// bound the lifetime of the handle.
func Open(ctx context.Context, dsn DSN, opts ...Option) (*sql.DB, error) {
	db, _, err := open(ctx, dsn, opts...)
	return db, err
}

// openWithCleanup is the wire provider: the same handle, plus the func() that
// closes it.
//
// The cleanup's type is func() and not func() error, because func() is the only
// shape github.com/goforj/wire recognises as a provider's cleanup. Getting it
// wrong here would mean every later module inherits a shutdown nothing calls.
// A Close that fails is reported through otel.Handle, which is the only place
// left to put it once the signature has no error to return.
func openWithCleanup(ctx context.Context, dsn DSN, opts ...Option) (*sql.DB, func(), error) {
	db, reg, err := open(ctx, dsn, opts...)
	if err != nil {
		return nil, nil, err
	}
	return db, func() {
		if reg != nil {
			if err := reg.Unregister(); err != nil {
				otel.Handle(err)
			}
		}
		if err := db.Close(); err != nil {
			otel.Handle(err)
		}
	}, nil
}

// open is the body [Open] and [openWithCleanup] share. It returns the metric
// registration alongside the handle so that the wire path can unregister the
// pool-statistics callback, which holds a reference to db, when the handle goes
// away.
func open(ctx context.Context, dsn DSN, opts ...Option) (_ *sql.DB, _ metric.Registration, err error) {
	set, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("goga/database: open: %w", err)
	}

	_, end := set.instr.Start(ctx, "Open")
	defer func() { end(err) }()

	// Parsed here, and thrown away: pgx's stdlib driver defers parsing to the
	// first connection, so without this line a DSN with a typo in it would open
	// cleanly and fail at the first query, several layers away from the code
	// that supplied it. Parsing twice costs nothing measurable and buys the
	// house rule that a bad value fails at the call site that passed it.
	if _, perr := pgx.ParseConfig(string(dsn)); perr != nil {
		return nil, nil, fmt.Errorf("goga/database: open: parsing dsn: %w", perr)
	}

	// The wrapping is unconditional. It is not behind an option, and no option
	// value reaches this line: this is the whole of the guarantee that no
	// uninstrumented handle leaves the package.
	wrapped := otelsql.WrapDriver(stdlib.GetDefaultDriver(), set.otelOptions()...)

	// pgx's stdlib driver implements driver.DriverContext, and otelsql's wrapper
	// preserves that, so a connector is what the handle is built over. Going
	// through it rather than through otelsql.Open also keeps the wrapped driver
	// out of database/sql's process-wide registry, which otelsql.Register would
	// add a numbered entry to on every call.
	dc, ok := wrapped.(driver.DriverContext)
	if !ok {
		return nil, nil, errDriverNotWrapped
	}
	connector, err := dc.OpenConnector(string(dsn))
	if err != nil {
		return nil, nil, fmt.Errorf("goga/database: open: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(set.maxOpenConns)
	db.SetMaxIdleConns(set.maxIdleConns)
	db.SetConnMaxLifetime(set.connMaxLifetime)

	reg, err := otelsql.RegisterDBStatsMetrics(db, set.otelOptions()...)
	if err != nil {
		// Pool statistics are an addition to the per-statement telemetry, not a
		// precondition for it. Failing Open over them would deny the caller a
		// working, traced handle over a missing gauge, so the error is reported
		// and the handle is returned.
		otel.Handle(fmt.Errorf("goga/database: open: registering pool statistics: %w", err))
		err = nil
	}

	return db, reg, nil
}

// Set is the wire provider set for this module: a *sql.DB and the func() that
// closes it, built from a [DSN] the graph supplies.
//
// It provides [openWithCleanup] rather than [Open] so that the generated
// injector closes the handle. A provider set that hands out a pool nothing
// closes is the defect this shape exists to prevent.
var Set = wire.NewSet(openWithCleanup)
