package database

import (
	"fmt"
	"time"

	"github.com/XSAM/otelsql"
	otelsemconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
//
// There is deliberately no field here that could turn instrumentation off. The
// otelsql wrapping in open does not read this struct to decide whether to
// happen; it reads it only to decide how.
type settings struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration

	sqlCommenter bool

	instr *telemetry.Instrumentation
}

// The pool defaults. database/sql's own defaults are an unlimited number of open
// connections and two idle ones, which is the shape that turns a burst of slow
// queries into an unbounded number of PostgreSQL backends and takes the server
// down rather than the caller. Every value here is modest, bounded and
// overridable.
const (
	// defaultMaxOpenConns bounds the connections one process holds. PostgreSQL's
	// own max_connections defaults to 100 and is shared by every process
	// pointing at the server, so a per-process default has to leave room for the
	// replicas beside it.
	defaultMaxOpenConns = 10

	// defaultMaxIdleConns is deliberately equal to defaultMaxOpenConns. A max
	// idle count below the max open count makes database/sql close and reopen
	// connections under steady load — the pool churns exactly when it is busiest
	// — and that is the single most common misconfiguration of this pool.
	defaultMaxIdleConns = defaultMaxOpenConns

	// defaultConnMaxLifetime retires a connection after an hour so that a
	// failover, a DNS change or a rolling restart of the database is picked up
	// without restarting the application.
	defaultConnMaxLifetime = time.Hour
)

// defaults returns the settings an [Open] with no options runs with.
func defaults() settings {
	return settings{
		maxOpenConns:    defaultMaxOpenConns,
		maxIdleConns:    defaultMaxIdleConns,
		connMaxLifetime: defaultConnMaxLifetime,
		instr:           telemetry.For(moduleName),
	}
}

// otelOptions is the configuration handed to otelsql.
//
// No tracer or meter provider is passed, on purpose: otelsql then resolves
// OpenTelemetry's globals on every use, which is the same rule
// [telemetry.For] follows and for the same reason — a handle built in a
// composition root, before goga/telemetry has installed the real providers,
// still has to start emitting once it does.
//
// db.system.name is taken from the upstream OpenTelemetry registry rather than
// from goga/semconv. goga/semconv declares what goga's *own* instrumentation
// emits; this attribute goes on otelsql's spans, and aliasing the upstream
// constant is what keeps it from drifting from the specification.
func (s *settings) otelOptions() []otelsql.Option {
	return []otelsql.Option{
		otelsql.WithAttributes(otelsemconv.DBSystemNamePostgreSQL),
		otelsql.WithSQLCommenter(s.sqlCommenter),
	}
}

// Option configures [Open]. It is an exported alias over an unexported settings
// type, so a caller can hold and pass a database.Option and cannot write the
// struct it mutates.
type Option = goga.Option[settings]

// WithMaxOpenConns bounds the number of connections the pool opens, counting
// those in use and those idle.
//
// Zero and negative values are rejected rather than passed through:
// database/sql reads a non-positive value as "unlimited", and an unlimited pool
// is what turns a slow query into an outage on the database server that every
// other process shares.
//
// The default is 10.
func WithMaxOpenConns(n int) Option {
	return func(s *settings) error {
		if n <= 0 {
			return fmt.Errorf("goga/database: max open conns must be > 0, got %d", n)
		}
		s.maxOpenConns = n
		return nil
	}
}

// WithMaxIdleConns sets how many idle connections the pool keeps.
//
// Keep it equal to the max open count unless there is a measured reason not to.
// A lower value makes database/sql close and immediately reopen connections
// under steady load, so the pool churns hardest exactly when it is busiest.
//
// The default equals the max open count.
func WithMaxIdleConns(n int) Option {
	return func(s *settings) error {
		if n <= 0 {
			return fmt.Errorf("goga/database: max idle conns must be > 0, got %d", n)
		}
		s.maxIdleConns = n
		return nil
	}
}

// WithConnMaxLifetime retires a connection this long after it was created.
//
// It is what lets a failover, a DNS change or a rolling restart of the database
// be picked up without restarting the application, so it is bounded by default
// rather than left to run forever.
//
// The default is one hour.
func WithConnMaxLifetime(d time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf("goga/database: conn max lifetime must be > 0, got %s", d)
		}
		s.connMaxLifetime = d
		return nil
	}
}

// WithSQLCommenter writes the active trace context into a comment appended to
// every statement, in the sqlcommenter format PostgreSQL's own logging and
// pg_stat_statements can be joined on.
//
// It is off by default. The comment changes the text of every query, which
// defeats prepared-statement reuse in some drivers and makes statement
// fingerprints differ per trace, so it is a decision a project makes knowingly.
func WithSQLCommenter(on bool) Option {
	return func(s *settings) error {
		s.sqlCommenter = on
		return nil
	}
}

// WithTelemetry replaces the instrumentation handle goga's own spans — the
// construction span and the transaction span — are recorded on.
//
// It replaces; it can never disable. There is no option, and no value of any
// option, that yields an uninstrumented handle: the otelsql driver wrapping in
// [Open] does not consult the settings to decide whether to happen, and a nil
// handle is rejected here rather than accepted as a way to ask for silence.
//
// Passing a handle for a different module renames goga's spans and re-attributes
// their metrics, which is what an adopting project wants when it embeds this
// module inside a module of its own.
func WithTelemetry(i *telemetry.Instrumentation) Option {
	return func(s *settings) error {
		if i == nil {
			return errNilTelemetry
		}
		s.instr = i
		return nil
	}
}
