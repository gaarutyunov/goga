// Package migrate runs a project's database migrations, once, under a lock,
// with a span per migration.
//
// The engine is goose, and this package does not replace its API — [Migrator]
// wraps a *goose.Provider and [Migrator.Provider] hands it back. What it adds
// is the four things goose leaves to the caller and that a service gets wrong
// exactly once:
//
//   - Migrations embedded in the binary ([WithFS]), so a deployment cannot be
//     missing them.
//   - A PostgreSQL session-level advisory lock around the whole run, so two
//     replicas booting together take turns instead of both migrating.
//   - [Migrator.Ready], so a service whose schema is behind refuses traffic
//     instead of erroring per request.
//   - A span per migration carrying its version and name, which is how the
//     forty-second migration is found.
//
// # The input is a *sql.DB
//
// goose migrates through database/sql, and [github.com/gaarutyunov/goga/database.Open]
// returns the standard library's *sql.DB already instrumented, so the two meet
// with nothing in between. There is no portable handle to unwrap and no
// pgxpool bridge: an earlier revision of the design had both, and both went
// away with the portable database type.
//
// # The lock is this package's, not goose's
//
// goose can take the advisory lock itself, through goose.WithSessionLocker, and
// this package deliberately does not use that. goose takes and releases the
// lock inside *each* operation, on the connection that operation borrowed —
// so a run built as a loop over ApplyVersion, which is what a span per
// migration requires, would take and release the lock once per migration and
// let a second replica interleave between two of them. Worse, this package's
// own lock is held on a connection of its own, and a session-level advisory
// lock is not re-entrant across connections: with both enabled, goose's
// acquisition would block on this package's and the run would deadlock against
// itself until the timeout.
//
// So the lock is taken here, once, around the whole run, and released
// unconditionally — including when a migration fails, because a lock left held
// by a failed run blocks every later attempt, which turns one bad migration
// into an outage that outlives it.
//
// # PostgreSQL
//
// The default dialect is postgres and the default lock is PostgreSQL's session
// advisory lock. Another engine needs [WithDialect] and a [WithSessionLocker]
// of its own; nothing else in goga is tested against one.
package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/goforj/wire"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"go.opentelemetry.io/otel"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
	"github.com/gaarutyunov/goga/telemetry"
)

// moduleName is the goga module this package reports itself as to
// goga/telemetry.
const moduleName = "migrate"

// The operations this module opens spans for. They are named after the methods
// that open them, except opApply, which has no method of its own: it is the
// span covering one migration, and it is the one this module exists to emit.
const (
	opUp     = "Up"
	opUpTo   = "UpTo"
	opDown   = "Down"
	opStatus = "Status"
	opApply  = "apply"
)

var (
	// errNilDB answers New called without a handle. goose has nothing to
	// migrate without one and would report it several frames deeper.
	errNilDB = errors.New("goga/migrate: db must not be nil")

	// errNilFS answers WithFS(nil). Passing nil is not a way to ask for a
	// default filesystem, because there is no default: see WithFS.
	errNilFS = errors.New("goga/migrate: migrations fs must not be nil")

	// errNoMigrationSource answers a New with neither WithFS nor WithDir. It
	// names both options because which one is right depends on whether the
	// migrations are embedded, and only the caller knows that.
	errNoMigrationSource = errors.New(
		"goga/migrate: no migrations: pass WithFS(embed.FS) to embed them in the binary, or WithDir(path) to read them from disk")

	// errNilLocker answers WithSessionLocker(nil). There is no option value
	// that runs migrations without a lock.
	errNilLocker = errors.New(
		"goga/migrate: session locker must not be nil — the locker can be replaced but locking can never be disabled")

	// errSingleConnection answers a pool that can hand out exactly one
	// connection. The lock holds one for the whole run and goose needs another,
	// so such a pool deadlocks: the run waits for a connection the run itself
	// is holding. It is rejected at construction rather than discovered as a
	// hang at boot.
	errSingleConnection = errors.New(
		"goga/migrate: the pool allows only one open connection: the advisory lock holds one for the whole run and goose needs a second, so a migration would deadlock — raise database.WithMaxOpenConns")
)

// Applied describes one migration a run applied.
type Applied struct {
	// Version is the migration's version, as recorded in the version table.
	Version int64
	// Source is the migration file, as goose reports its path.
	Source string
	// Direction is "up" or "down".
	Direction string
	// Duration is how long the migration itself took. It is goose's
	// measurement of the statements, not the span's, so it excludes the
	// bookkeeping around them.
	Duration time.Duration
	// Empty reports a migration that was versioned but did nothing: no
	// statements for SQL, a nil function for Go.
	Empty bool
}

// Status is one migration's position: known to the binary, and applied to this
// database or not.
type Status struct {
	// Version is the migration's version.
	Version int64
	// Source is the migration file, as goose reports its path.
	Source string
	// Pending reports a migration that exists in the binary and has not been
	// applied to this database.
	Pending bool
	// AppliedAt is when it was applied. It is the zero time when Pending.
	AppliedAt time.Time
}

// Migrator runs one project's migrations against one database.
//
// It is safe for concurrent use: goose's provider serialises operations within
// the process, and the advisory lock serialises them across processes.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
	instr    *telemetry.Instrumentation

	locker      lock.SessionLocker
	lockTimeout time.Duration

	// allowMissing mirrors the provider option, because the rule it turns off
	// is enforced here rather than by goose: see checkForMissing.
	allowMissing bool
}

// New builds a migrator over db.
//
// db is the standard library's handle, which is what goose migrates through and
// what [github.com/gaarutyunov/goga/database.Open] returns; there is nothing to
// unwrap and no pool to bridge.
//
// The migrations come from [WithFS] or [WithDir], and there is no default:
// a migrator with no migration source is a program that silently applies
// nothing, so it is an error here instead.
//
// New does not connect and does not migrate. It reads the migration
// filesystem, which is where a duplicate version or an unparseable filename is
// reported, so those fail at construction rather than at boot.
func New(db *sql.DB, opts ...Option) (*Migrator, error) {
	set, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, fmt.Errorf("goga/migrate: new: %w", err)
	}
	if db == nil {
		return nil, errNilDB
	}
	if set.fsys == nil {
		return nil, errNoMigrationSource
	}
	// Stats reports the configured limit, and zero means unlimited. One is the
	// only value that cannot work.
	if db.Stats().MaxOpenConnections == 1 {
		return nil, errSingleConnection
	}

	locker, err := set.newLocker()
	if err != nil {
		return nil, err
	}

	// No goose.WithSessionLocker here, on purpose: this package holds the lock
	// itself, around the whole run. See the package documentation.
	provider, err := goose.NewProvider(goose.Dialect(set.dialect), db, set.fsys,
		goose.WithTableName(set.table),
		goose.WithAllowOutofOrder(set.allowMissing),
	)
	if err != nil {
		return nil, fmt.Errorf("goga/migrate: new: %w", err)
	}

	return &Migrator{
		db:           db,
		provider:     provider,
		instr:        set.instr,
		locker:       locker,
		lockTimeout:  set.lockTimeout,
		allowMissing: set.allowMissing,
	}, nil
}

// Up applies every pending migration, in version order.
//
// The whole run happens under one advisory lock, so a second replica calling Up
// at the same moment waits and then finds nothing to do rather than applying
// the same migration twice. Each migration gets a span of its own carrying its
// version and name.
//
// On failure the migrations applied before the failing one are returned
// alongside the error: they are committed, and a caller that logs only the
// error loses the record of what the database now contains.
func (m *Migrator) Up(ctx context.Context) (applied []Applied, err error) {
	ctx, end := m.instr.Start(ctx, opUp)
	defer func() { end(err) }()

	return m.run(ctx, math.MaxInt64)
}

// UpTo applies every pending migration up to and including version, and stops.
//
// It takes the same lock and emits the same per-migration spans as [Migrator.Up].
func (m *Migrator) UpTo(ctx context.Context, version int64) (applied []Applied, err error) {
	ctx, end := m.instr.Start(ctx, opUpTo, semconv.MigrationVersion(version))
	defer func() { end(err) }()

	if version < 1 {
		return nil, fmt.Errorf("goga/migrate: up to: version must be > 0, got %d", version)
	}
	return m.run(ctx, version)
}

// Down rolls back the most recently applied migration.
//
// It takes the same lock as [Migrator.Up], because a rollback racing another
// replica's boot is the same failure in the other direction.
func (m *Migrator) Down(ctx context.Context) (a Applied, err error) {
	ctx, end := m.instr.Start(ctx, opDown)
	defer func() { end(err) }()

	release, err := m.lock(ctx)
	if err != nil {
		return Applied{}, err
	}
	defer release()

	current, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("goga/migrate: down: %w", err)
	}
	if current == 0 {
		return Applied{}, fmt.Errorf("goga/migrate: down: %w", goose.ErrNoNextVersion)
	}

	res, err := m.apply(ctx, current, m.sourceOf(current), false)
	if err != nil {
		return Applied{}, err
	}
	return res, nil
}

// Status reports every migration the binary knows about and whether this
// database has it.
//
// It takes no lock: it reads, and a read that waited for a running migration
// would be useless to the operator asking what is happening right now.
func (m *Migrator) Status(ctx context.Context) (out []Status, err error) {
	ctx, end := m.instr.Start(ctx, opStatus)
	defer func() { end(err) }()

	return m.status(ctx)
}

// Pending reports the migrations that exist in the binary and have not been
// applied to this database, oldest first.
//
// It is the input [Migrator.Ready] answers from, and it takes no lock for the
// same reason [Migrator.Status] does not.
func (m *Migrator) Pending(ctx context.Context) (out []Status, err error) {
	return m.pending(ctx)
}

// Ready reports an error while any migration is pending, and nil when the
// database is at the version this binary expects.
//
// Its signature is [github.com/gaarutyunov/goga/serve.WithReadinessCheck]'s, so
// the method value drops straight in:
//
//	srv, err := serve.New(h, serve.WithReadinessCheck("migrations", m.Ready))
//
// That is the whole point of the shape. A service whose schema is behind then
// fails /readyz and is taken out of the load balancer's rotation, instead of
// accepting traffic and erroring once per request against a table that is not
// there yet.
//
// It opens no span. goga/serve keeps the operational endpoints out of the trace
// on purpose, and a readiness probe polled every second is exactly the traffic
// that exclusion exists for.
func (m *Migrator) Ready(ctx context.Context) error {
	pending, err := m.pending(ctx)
	if err != nil {
		return fmt.Errorf("goga/migrate: ready: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("goga/migrate: %d migration(s) pending, oldest is %d (%s)",
			len(pending), pending[0].Version, pending[0].Source)
	}
	return nil
}

// Provider returns the goose provider underneath.
//
// It is the escape hatch, and it is deliberate: goose has a larger API than
// this package wraps — Redo, DownTo, the Go-migration registry — and a project
// that needs one of those should reach it rather than be told the framework
// does not support it. What it gives up is this package's guarantees: an
// operation driven through the provider takes no advisory lock and emits no
// goga span.
func (m *Migrator) Provider() *goose.Provider { return m.provider }

// Set is the wire provider set for this module.
var Set = wire.NewSet(New)

// run applies every pending migration up to and including target, under one
// lock, with one span each.
func (m *Migrator) run(ctx context.Context, target int64) (applied []Applied, err error) {
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	// Unconditional, so a failure inside the lock — a migration that does not
	// compile, a context cancelled mid-run, a panic unwinding through here —
	// still leaves the lock free for the next attempt.
	defer release()

	// Read inside the lock. Reading it outside would compute the work from a
	// state another replica is still changing, and the loser of the race would
	// then try to apply migrations the winner had already committed.
	pending, err := m.pending(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.checkForMissing(ctx, pending); err != nil {
		return nil, err
	}

	for _, src := range pending {
		if src.Version > target {
			break
		}
		res, aerr := m.apply(ctx, src.Version, src.Source, true)
		if aerr != nil {
			return applied, aerr
		}
		applied = append(applied, res)
	}
	return applied, nil
}

// apply runs one migration inside a span of its own.
//
// The span is the point of the module: its attributes name the version and the
// file, and the closer the instrumentation handed back holds *this* migration's
// start time, so the duration recorded is this migration's and not the run's.
func (m *Migrator) apply(ctx context.Context, version int64, source string, up bool) (_ Applied, err error) {
	mctx, end := m.instr.Start(ctx, opApply,
		semconv.MigrationVersion(version), semconv.MigrationName(source))
	defer func() { end(err) }()

	res, err := m.provider.ApplyVersion(mctx, version, up)
	if err != nil {
		// Item 5.7: the version and the file, both, because a version alone
		// sends the reader to the directory listing and a file alone does not
		// say what the version table now holds.
		err = fmt.Errorf("goga/migrate: migration %d (%s): %w", version, source, err)
		return Applied{}, err
	}
	return appliedFrom(res), nil
}

// checkForMissing rejects a pending migration older than the newest one already
// applied, unless [WithAllowMissing] asked for it.
//
// goose enforces this inside its own Up, which this package does not use — the
// per-migration span needs the loop — so the rule is enforced here instead. It
// is not a formality: two branches merged in the wrong order produce exactly
// this shape, and applying the older migration afterwards leaves two databases
// with different schemas from the same migration set.
func (m *Migrator) checkForMissing(ctx context.Context, pending []Status) error {
	if m.allowMissing || len(pending) == 0 {
		return nil
	}

	current, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("goga/migrate: reading the current version: %w", err)
	}
	for _, src := range pending {
		if src.Version < current {
			return fmt.Errorf(
				"goga/migrate: migration %d (%s) is older than the applied version %d;"+
					" pass WithAllowMissing(true) to apply it out of order",
				src.Version, src.Source, current)
		}
	}
	return nil
}

// status is the body [Migrator.Status] wraps in a span, so that the paths that
// need the list from inside another span do not nest a second one.
func (m *Migrator) status(ctx context.Context) ([]Status, error) {
	statuses, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("goga/migrate: status: %w", err)
	}

	out := make([]Status, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, statusFrom(s))
	}
	return out, nil
}

// pending is status filtered to what has not been applied, oldest first. goose
// returns statuses in ascending version order, and this preserves it.
func (m *Migrator) pending(ctx context.Context) ([]Status, error) {
	all, err := m.status(ctx)
	if err != nil {
		return nil, err
	}

	var out []Status
	for _, s := range all {
		if s.Pending {
			out = append(out, s)
		}
	}
	return out, nil
}

// sourceOf returns the file a version came from, or "" when the migration was
// registered rather than read from a filesystem.
func (m *Migrator) sourceOf(version int64) string {
	for _, src := range m.provider.ListSources() {
		if src.Version == version {
			return src.Path
		}
	}
	return ""
}

// lock takes the session-level advisory lock and returns the function that
// releases it.
//
// The lock lives on a connection of its own, held for the whole run, because a
// session-level advisory lock belongs to a session: taking it on a connection
// goose then returns to the pool would release it at an arbitrary later moment.
//
// Acquisition is bounded by a deadline on the context rather than by the
// locker's own retry budget, so the bound holds for a [WithSessionLocker]
// implementation this package knows nothing about.
func (m *Migrator) lock(ctx context.Context) (func(), error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("goga/migrate: acquiring lock: %w", err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, m.lockTimeout)
	defer cancel()

	if err := m.locker.SessionLock(lockCtx, conn); err != nil {
		closeConn(conn)
		return nil, fmt.Errorf("goga/migrate: acquiring lock: %w", err)
	}

	return func() { m.release(ctx, conn) }, nil
}

// release unlocks the session and gives the connection back.
func (m *Migrator) release(ctx context.Context, conn *sql.Conn) {
	// Detached from the caller's cancellation on purpose. The commonest reason
	// a run ends early is that its context was cancelled, and a release that
	// inherited that cancellation would fail exactly when it is most needed —
	// leaving the lock held and every later attempt blocked behind it.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.lockTimeout)
	defer cancel()

	if err := m.locker.SessionUnlock(uctx, conn); err != nil {
		otel.Handle(fmt.Errorf("goga/migrate: releasing the advisory lock: %w", err))

		// The unlock did not happen, so this session still holds the lock.
		// Returning the connection to the pool would leave it held for the life
		// of the process and block every later run. Marking the connection bad
		// ends the session instead, and PostgreSQL releases a session's
		// advisory locks when its session ends — which is the only remedy left
		// once the explicit unlock has failed.
		if err := conn.Raw(discardConn); err != nil && !errors.Is(err, driver.ErrBadConn) {
			otel.Handle(fmt.Errorf("goga/migrate: discarding the lock connection: %w", err))
		}
	}
	closeConn(conn)
}

// discardConn is the [database/sql.Conn.Raw] callback that marks a connection
// bad. Returning driver.ErrBadConn is how database/sql is told never to reuse
// it.
func discardConn(any) error { return driver.ErrBadConn }

// closeConn returns a connection to the pool, reporting a failure that is not
// simply "already done" the way every other non-fatal cleanup error in goga is
// reported.
func closeConn(conn *sql.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		otel.Handle(fmt.Errorf("goga/migrate: closing the lock connection: %w", err))
	}
}

// appliedFrom converts goose's result. The conversion exists so that goose's
// types stay out of this package's surface: goga/lint confines the goose import
// to this package, and a result type a caller has to name would make that
// confinement a lie.
func appliedFrom(res *goose.MigrationResult) Applied {
	if res == nil {
		return Applied{}
	}
	a := Applied{
		Direction: res.Direction,
		Duration:  res.Duration,
		Empty:     res.Empty,
	}
	if res.Source != nil {
		a.Version = res.Source.Version
		a.Source = res.Source.Path
	}
	return a
}

// statusFrom converts goose's status, for the same reason as [appliedFrom].
func statusFrom(s *goose.MigrationStatus) Status {
	if s == nil {
		return Status{}
	}
	out := Status{
		Pending:   s.State == goose.StatePending,
		AppliedAt: s.AppliedAt,
	}
	if s.Source != nil {
		out.Version = s.Source.Version
		out.Source = s.Source.Path
	}
	return out
}
