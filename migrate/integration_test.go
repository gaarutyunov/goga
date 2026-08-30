//go:build integration

// The container-backed half of this module's tests.
//
// Everything here needs a Docker daemon, so it is behind the `integration`
// build tag and the default `go test ./...` does not see it. What lives here is
// what cannot honestly be asserted against a double: that the advisory lock
// serialises two replicas booting together, that it is released when a
// migration fails, that each migration's span carries its own version, name and
// duration, and that a pending migration takes an instance out of rotation.
//
// Run with:
//
//	go test -race -tags integration ./migrate/...
package migrate_test

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3/lock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/migrate"
	"github.com/gaarutyunov/goga/semconv"
	"github.com/gaarutyunov/goga/serve"
	"github.com/gaarutyunov/goga/serve/servetest"
)

// postgresImage is pinned. An unpinned tag makes a test that passed yesterday
// fail today for a reason nothing in the repository records.
const postgresImage = "postgres:17-alpine"

// The migration sets, embedded — which is also the point of the first of them:
// [migrate.WithFS] taking an embed.FS is the house default, and a test that
// only ever read a directory would never exercise it.
//
//go:embed testdata/migrations/*.sql
var embeddedMigrations embed.FS

//go:embed testdata/slow/*.sql
var slowMigrations embed.FS

//go:embed testdata/broken/*.sql
var brokenMigrations embed.FS

// adminDSN points at the container's initial database. Every test creates a
// database of its own from it, so that one test's schema and one test's version
// table cannot be another's starting state.
var adminDSN string

// dbSeq numbers the per-test databases.
var dbSeq atomic.Int64

func TestMain(m *testing.M) {
	code, err := runWithPostgres(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "goga/migrate integration tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runWithPostgres starts the container, runs the suite and tears the container
// down. It is a function of its own because os.Exit skips deferred calls, so the
// teardown has to happen before TestMain reaches it.
func runWithPostgres(m *testing.M) (int, error) {
	// The trap this line exists for: testcontainers falls back to running psql
	// inside the container when the SQL driver it was told to use is not linked
	// into the test binary, and the error that comes back then names the wrong
	// thing entirely. pgx's stdlib driver registers itself from goga/database's
	// import of it, so this asserts the import chain rather than hoping for it.
	if !hasDriver("pgx") {
		return 0, fmt.Errorf("the pgx database/sql driver is not registered; registered drivers are %v", sql.Drivers())
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("goga"),
		tcpostgres.WithUsername("goga"),
		tcpostgres.WithPassword("goga"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return 0, fmt.Errorf("starting %s: %w", postgresImage, err)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintln(os.Stderr, "terminating the postgres container:", err)
		}
	}()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, fmt.Errorf("reading the connection string: %w", err)
	}
	adminDSN = dsn

	return m.Run(), nil
}

// hasDriver reports whether name is registered with database/sql.
func hasDriver(name string) bool {
	for _, d := range sql.Drivers() {
		if d == name {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// newDatabase creates an empty database on the shared container and returns its
// DSN.
//
// A database per test rather than a schema per test: the version table's name
// is one of the things under test, so it has to be able to sit at its default
// name without two tests fighting over it.
func newDatabase(t *testing.T) database.DSN {
	t.Helper()

	name := fmt.Sprintf("goga_migrate_%d", dbSeq.Add(1))

	admin, err := database.Open(t.Context(), database.DSN(adminDSN))
	require.NoError(t, err)
	defer func() { assert.NoError(t, admin.Close()) }()

	// create database cannot run inside a transaction, so it is executed
	// directly. The name is generated here and never comes from a test's input.
	_, err = admin.ExecContext(t.Context(), "create database "+name)
	require.NoError(t, err)

	u, err := url.Parse(adminDSN)
	require.NoError(t, err)
	u.Path = "/" + name
	return database.DSN(u.String())
}

// openOn returns an instrumented handle onto dsn, closed when the test ends.
func openOn(t *testing.T, dsn database.DSN) *sql.DB {
	t.Helper()

	db, err := database.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

// newMigrator builds a migrator with its own handle onto dsn. Each call is one
// "replica": its own pool, its own provider, one shared database.
func newMigrator(t *testing.T, dsn database.DSN, opts ...migrate.Option) *migrate.Migrator {
	t.Helper()

	m, err := migrate.New(openOn(t, dsn), opts...)
	require.NoError(t, err)
	return m
}

// sub returns the root of one embedded migration set. goose reads the root of
// the filesystem it is handed, so the directory is peeled off here rather than
// with an option only one of the two sources could honour.
func sub(t *testing.T, fsys embed.FS, dir string) fs.FS {
	t.Helper()

	out, err := fs.Sub(fsys, dir)
	require.NoError(t, err)
	return out
}

// tableExists reports whether a table of that name exists in dsn's database.
func tableExists(t *testing.T, dsn database.DSN, name string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, openOn(t, dsn).QueryRowContext(t.Context(),
		`select exists (select 1 from information_schema.tables
		                where table_schema = 'public' and table_name = $1)`,
		name).Scan(&exists))
	return exists
}

// appliedVersions reads the version table directly, so that "applied once" is a
// fact about the database rather than about what a method returned.
func appliedVersions(t *testing.T, dsn database.DSN, table string) []int64 {
	t.Helper()

	rows, err := openOn(t, dsn).QueryContext(t.Context(),
		//nolint:gosec // the table name is a constant in every caller
		"select version_id from "+table+" where version_id > 0 order by version_id")
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()

	var out []int64
	for rows.Next() {
		var v int64
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

// advisoryLocksHeld counts the sessions on the whole server holding goose's
// advisory lock. The tests in this file do not run in parallel, so a non-zero
// count after a run is this module's lock and nobody else's.
func advisoryLocksHeld(t *testing.T) int {
	t.Helper()

	admin, err := database.Open(t.Context(), database.DSN(adminDSN))
	require.NoError(t, err)
	defer func() { assert.NoError(t, admin.Close()) }()

	var n int
	require.NoError(t, admin.QueryRowContext(t.Context(),
		`select count(*) from pg_locks
		 where locktype = 'advisory' and ((classid::bigint << 32) | objid::bigint) = $1`,
		lock.DefaultLockID).Scan(&n))
	return n
}

// -----------------------------------------------------------------------------
// Up applies the migrations, once
// -----------------------------------------------------------------------------

// TestIntegrationUpAppliesEveryMigrationExactlyOnce also pins the embedded
// path: the filesystem here is an embed.FS, which is the shape a binary
// carrying its own schema has.
func TestIntegrationUpAppliesEveryMigrationExactlyOnce(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))

	applied, err := m.Up(t.Context())
	require.NoError(t, err)

	require.Len(t, applied, 2)
	assert.Equal(t, int64(1), applied[0].Version)
	assert.Contains(t, applied[0].Source, "00001_create_widgets.sql")
	assert.Equal(t, "up", applied[0].Direction)
	assert.Positive(t, applied[0].Duration)
	assert.Equal(t, int64(2), applied[1].Version)

	assert.True(t, tableExists(t, dsn, "widgets"), "the migration ran against the real database")

	// The second run is the assertion that matters for a boot path: every
	// replica calls Up, and every replica after the first must find nothing.
	again, err := m.Up(t.Context())
	require.NoError(t, err)
	assert.Empty(t, again)
	assert.Equal(t, []int64{1, 2}, appliedVersions(t, dsn, "goga_db_version"))
}

// TestIntegrationTheVersionTableIsGogaDbVersion is item 5.5. The name is
// asserted as a literal rather than against the constant that produces it: the
// point of naming it once is that the name is stable, and a test reading the
// same constant the code reads would not notice it changing.
func TestIntegrationTheVersionTableIsGogaDbVersion(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))

	_, err := m.Up(t.Context())
	require.NoError(t, err)

	assert.True(t, tableExists(t, dsn, "goga_db_version"))
	assert.False(t, tableExists(t, dsn, "goose_db_version"),
		"goose's own default name is not used: a goga-managed database is worth telling apart")
	assert.Equal(t, []int64{1, 2}, appliedVersions(t, dsn, "goga_db_version"))
}

// TestIntegrationWithDirOverridesTheEmbeddedFilesystem: the two options are one
// setting, and the later one wins outright — here proved against a real
// database by which tables exist afterwards.
func TestIntegrationWithDirOverridesTheEmbeddedFilesystem(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn,
		migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")),
		migrate.WithDir("testdata/slow"))

	_, err := m.Up(t.Context())
	require.NoError(t, err)

	assert.True(t, tableExists(t, dsn, "slow_widgets"), "the directory's migrations ran")
	assert.False(t, tableExists(t, dsn, "widgets"), "the embedded ones did not")
}

func TestIntegrationUpToStopsAtTheGivenVersion(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))

	applied, err := m.UpTo(t.Context(), 1)
	require.NoError(t, err)

	require.Len(t, applied, 1)
	assert.Equal(t, int64(1), applied[0].Version)
	assert.Equal(t, []int64{1}, appliedVersions(t, dsn, "goga_db_version"))
}

func TestIntegrationDownRollsBackTheMostRecentMigration(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))
	_, err := m.Up(t.Context())
	require.NoError(t, err)

	rolled, err := m.Down(t.Context())
	require.NoError(t, err)

	assert.Equal(t, int64(2), rolled.Version)
	assert.Equal(t, "down", rolled.Direction)
	assert.Equal(t, []int64{1}, appliedVersions(t, dsn, "goga_db_version"))
	assert.True(t, tableExists(t, dsn, "widgets"), "only the last migration came back off")
}

// TestIntegrationAnOutOfOrderMigrationIsRejectedUnlessAllowed: two branches
// merged in the wrong order produce exactly this shape, and applying the older
// migration afterwards leaves two databases with different schemas from the
// same migration set.
func TestIntegrationAnOutOfOrderMigrationIsRejectedUnlessAllowed(t *testing.T) {
	dsn := newDatabase(t)

	first := fstest.MapFS{
		"00001_a.sql": sqlMigration("create table a (id bigint)", "drop table a"),
		"00003_c.sql": sqlMigration("create table c (id bigint)", "drop table c"),
	}
	withGap := fstest.MapFS{
		"00001_a.sql": first["00001_a.sql"],
		"00002_b.sql": sqlMigration("create table b (id bigint)", "drop table b"),
		"00003_c.sql": first["00003_c.sql"],
	}

	_, err := newMigrator(t, dsn, migrate.WithFS(first)).Up(t.Context())
	require.NoError(t, err)

	_, err = newMigrator(t, dsn, migrate.WithFS(withGap)).Up(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "00002_b.sql")
	assert.Contains(t, err.Error(), "WithAllowMissing")
	assert.False(t, tableExists(t, dsn, "b"), "nothing was applied")

	allowed := newMigrator(t, dsn, migrate.WithFS(withGap), migrate.WithAllowMissing(true))
	applied, err := allowed.Up(t.Context())

	require.NoError(t, err)
	require.Len(t, applied, 1)
	assert.Equal(t, int64(2), applied[0].Version)
	assert.True(t, tableExists(t, dsn, "b"))
}

// sqlMigration builds a goose SQL migration file in memory.
func sqlMigration(up, down string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(
		"-- +goose Up\n" + up + ";\n\n-- +goose Down\n" + down + ";\n")}
}

// -----------------------------------------------------------------------------
// The advisory lock
// -----------------------------------------------------------------------------

// TestIntegrationTwoConcurrentUpsAreSerialised is the test this module exists
// for, and the one nobody runs by hand: two replicas booting at the same
// instant against one database.
//
// The synchronisation is a barrier channel and a WaitGroup — no sleeps, so
// there is no timing to get wrong and nothing to make the test flaky. The
// goroutines record their results and every assertion happens on the test's own
// goroutine, because require's FailNow is only valid there.
//
// Three things make the assertion sharp rather than decorative:
//
//   - Both calls must succeed. Without the lock both replicas compute the same
//     pending list and both run "create table widgets", so one of them comes
//     back with a duplicate-table error. A green NoError on both is the
//     serialisation.
//   - Exactly one replica reports having applied anything. The loser blocks in
//     SessionLock, and only reads the pending list once it holds the lock, so
//     it sees the winner's committed work and has nothing to do. That is
//     deterministic whichever replica wins.
//   - The version table holds each version exactly once, read back from the
//     database rather than taken from what a method returned.
func TestIntegrationTwoConcurrentUpsAreSerialised(t *testing.T) {
	const replicas = 2

	dsn := newDatabase(t)
	migrators := make([]*migrate.Migrator, replicas)
	for i := range migrators {
		migrators[i] = newMigrator(t, dsn,
			migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))
	}

	type outcome struct {
		applied []migrate.Applied
		err     error
	}
	outcomes := make([]outcome, replicas)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, m := range migrators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied, err := m.Up(t.Context())
			outcomes[i] = outcome{applied: applied, err: err}
		}()
	}
	close(start)
	wg.Wait()

	appliedBy := 0
	for i, o := range outcomes {
		require.NoErrorf(t, o.err, "replica %d", i)
		if len(o.applied) > 0 {
			appliedBy++
			assert.Len(t, o.applied, 2, "the replica that won the lock applied the whole set")
		}
	}
	assert.Equal(t, 1, appliedBy, "exactly one replica did the work; the other found nothing to do")
	assert.Equal(t, []int64{1, 2}, appliedVersions(t, dsn, "goga_db_version"))
	assert.Zero(t, advisoryLocksHeld(t), "both runs released the lock")
}

// TestIntegrationTheLockIsReleasedWhenAMigrationFails is the constraint the
// design flagged as expensive to discover late: a run that fails inside the
// lock has to leave the lock free, or one bad migration becomes an outage that
// outlives it.
//
// It is proved twice, because `defer release()` being present in the source is
// not evidence that it ran: once against pg_locks, which is the server's own
// answer, and once by running a second migration afterwards with a lock timeout
// far too short to have waited a leaked lock out.
func TestIntegrationTheLockIsReleasedWhenAMigrationFails(t *testing.T) {
	dsn := newDatabase(t)
	broken := newMigrator(t, dsn, migrate.WithFS(sub(t, brokenMigrations, "testdata/broken")))

	applied, err := broken.Up(t.Context())

	require.Error(t, err)
	assert.Len(t, applied, 1, "the migration before the failing one is committed and reported")
	assert.Zero(t, advisoryLocksHeld(t), "the failed run released the lock")

	good := newMigrator(t, dsn,
		migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")),
		migrate.WithLockTimeout(2*time.Second))

	recovered, err := good.Up(t.Context())

	require.NoError(t, err, "a later attempt is not blocked behind the failed one's lock")
	require.Len(t, recovered, 1)
	assert.Equal(t, int64(2), recovered[0].Version)
}

// TestIntegrationAFailedMigrationNamesItsVersionAndFile is item 5.7. The
// version alone sends the reader to a directory listing and the file alone does
// not say what the version table now holds, so the message carries both, inside
// the house wrapper.
func TestIntegrationAFailedMigrationNamesItsVersionAndFile(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, brokenMigrations, "testdata/broken")))

	_, err := m.Up(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/migrate: migration 2 (")
	assert.Contains(t, err.Error(), "00002_broken.sql")
}

// TestIntegrationUpWaitsForALockSomebodyElseHolds proves the lock is consulted
// at all — a release test alone would pass against a module that never took one.
//
// The lock is held here on a connection of the test's own, so there is nothing
// to wait for and nothing to race: the run cannot proceed until the test
// releases it.
func TestIntegrationUpWaitsForALockSomebodyElseHolds(t *testing.T) {
	dsn := newDatabase(t)

	holder, err := openOn(t, dsn).Conn(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, holder.Close()) }()

	var locked bool
	require.NoError(t, holder.QueryRowContext(t.Context(),
		"select pg_try_advisory_lock($1)", lock.DefaultLockID).Scan(&locked))
	require.True(t, locked, "the test holds the lock the migrator wants")

	m := newMigrator(t, dsn,
		migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")),
		migrate.WithLockTimeout(2*time.Second))

	began := time.Now()
	_, err = m.Up(t.Context())
	waited := time.Since(began)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/migrate: acquiring lock")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, waited, 2*time.Second, "it waited for the lock rather than failing outright")
	assert.False(t, tableExists(t, dsn, "widgets"), "nothing was applied without the lock")

	var unlocked bool
	require.NoError(t, holder.QueryRowContext(t.Context(),
		"select pg_advisory_unlock($1)", lock.DefaultLockID).Scan(&unlocked))
	require.True(t, unlocked)

	applied, err := m.Up(t.Context())
	require.NoError(t, err, "the run proceeds once the lock is free")
	assert.Len(t, applied, 2)
}

// -----------------------------------------------------------------------------
// A span per migration
// -----------------------------------------------------------------------------

// TestIntegrationEachMigrationGetsItsOwnSpanAndDuration is item 5.6, and it is
// the regression test for the defect the design caught in itself: an earlier
// revision passed time.Now() as the start at the point the migration ended,
// recording a duration of zero for every migration — the exact number this
// item exists to produce.
//
// A test asserting only that a span exists would have passed that version. This
// one cannot:
//
//   - The slow migration sleeps for 0.4s, so its recorded duration must be at
//     least that. The buggy shape records ~0 and fails here.
//   - The fast migration runs straight after it and must record well under
//     that. A closer holding the *run's* start rather than its own would report
//     the slow migration's time again, and fail here.
//
// The durations are read off goga.operation.duration — the histogram the module
// promises, not a number the test computed for itself — and the version and
// name are asserted on both the span and the measurement.
func TestIntegrationEachMigrationGetsItsOwnSpanAndDuration(t *testing.T) {
	const slept = 400 * time.Millisecond

	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, slowMigrations, "testdata/slow")))

	spans := recordSpans(t)
	durations := recordMetrics(t)

	applied, err := m.Up(t.Context())
	require.NoError(t, err)
	require.Len(t, applied, 2)

	// The spans: one per migration, each carrying its own version and file.
	var applySpans []servetest.RecordedSpan
	for _, s := range spans.Ended() {
		if s.Name == "goga.migrate.apply" {
			applySpans = append(applySpans, s)
		}
	}
	require.Len(t, applySpans, 2, "one span per migration, not one for the run")

	assert.Equal(t, int64(1), spanInt64(t, applySpans[0], semconv.MigrationVersionKey))
	assert.Contains(t, spanString(t, applySpans[0], semconv.MigrationNameKey), "00001_slow_widgets.sql")
	assert.Equal(t, int64(2), spanInt64(t, applySpans[1], semconv.MigrationVersionKey))
	assert.Contains(t, spanString(t, applySpans[1], semconv.MigrationNameKey), "00002_fast_widgets.sql")

	// The durations: each migration's own, and not the run's.
	slow := durations.forMigration(t, 1)
	fast := durations.forMigration(t, 2)

	assert.GreaterOrEqual(t, slow, slept.Seconds(),
		"the slow migration's own duration was recorded, not zero")
	assert.Less(t, fast, slept.Seconds()/2,
		"the fast migration's duration is its own and does not include the migration before it")
	assert.Positive(t, fast)
}

// recordSpans installs goga/serve/servetest's span recorder as the process
// tracer provider for the duration of the test.
//
// It is built on the OpenTelemetry API rather than on the SDK, which
// goga/telemetry owns and goga's lint configuration bans importing elsewhere.
// It installs a global, so a test that calls it must not be parallel.
func recordSpans(t *testing.T) *servetest.SpanRecorder {
	t.Helper()

	rec := servetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(rec)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// spanInt64 reads one integer attribute off a recorded span.
func spanInt64(t *testing.T, s servetest.RecordedSpan, key attribute.Key) int64 {
	t.Helper()

	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value.AsInt64()
		}
	}
	require.Failf(t, "attribute missing", "span %s carries no %s", s.Name, key)
	return 0
}

// spanString reads one string attribute off a recorded span.
func spanString(t *testing.T, s servetest.RecordedSpan, key attribute.Key) string {
	t.Helper()

	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value.AsString()
		}
	}
	require.Failf(t, "attribute missing", "span %s carries no %s", s.Name, key)
	return ""
}

// -----------------------------------------------------------------------------
// The duration recorder
// -----------------------------------------------------------------------------

// durationSink holds every value recorded on goga.operation.duration, with the
// attributes it was recorded under.
//
// It is hand-written rather than generated, for the same reason
// servetest.SpanRecorder is: the OpenTelemetry API interfaces embed the
// `embedded` package's interfaces, whose methods are unexported, so a type can
// only satisfy them by embedding — which is exactly what a generated mock does
// not do. It is a recorder and not a double: it asserts nothing and expects
// nothing, it only remembers what it was handed.
type durationSink struct {
	mu      sync.Mutex
	records []durationRecord
}

// durationRecord is one measurement.
type durationRecord struct {
	name  string
	value float64
	attrs attribute.Set
}

func (s *durationSink) record(r durationRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

// forMigration returns the duration recorded for the apply operation of one
// migration version, in seconds.
func (s *durationSink) forMigration(t *testing.T, version int64) float64 {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.records {
		if r.name != semconv.OperationDurationName {
			continue
		}
		if op, ok := r.attrs.Value(semconv.OperationKey); !ok || op.AsString() != "apply" {
			continue
		}
		if v, ok := r.attrs.Value(semconv.MigrationVersionKey); ok && v.AsInt64() == version {
			return r.value
		}
	}
	require.Failf(t, "measurement missing",
		"no %s recorded for migration %d", semconv.OperationDurationName, version)
	return 0
}

// recordMetrics installs a meter provider that remembers every histogram
// measurement, for the duration of the test.
func recordMetrics(t *testing.T) *durationSink {
	t.Helper()

	sink := &durationSink{}
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(&recordingMeterProvider{sink: sink})
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return sink
}

// recordingMeterProvider hands out meters whose float histograms record into a
// sink. Every other instrument is the API's no-op.
type recordingMeterProvider struct {
	embedded.MeterProvider

	sink *durationSink
}

func (p *recordingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return &recordingMeter{sink: p.sink}
}

// recordingMeter embeds the API's no-op meter so that it satisfies the whole of
// metric.Meter as that interface grows, and overrides only the one constructor
// the assertions read back.
type recordingMeter struct {
	metricnoop.Meter

	sink *durationSink
}

func (m *recordingMeter) Float64Histogram(
	name string, _ ...metric.Float64HistogramOption,
) (metric.Float64Histogram, error) {
	return &recordingHistogram{sink: m.sink, name: name}, nil
}

// recordingHistogram writes every measurement into the sink.
type recordingHistogram struct {
	metricnoop.Float64Histogram

	sink *durationSink
	name string
}

func (h *recordingHistogram) Record(_ context.Context, value float64, opts ...metric.RecordOption) {
	cfg := metric.NewRecordConfig(opts)
	h.sink.record(durationRecord{name: h.name, value: value, attrs: cfg.Attributes()})
}

// -----------------------------------------------------------------------------
// Readiness
// -----------------------------------------------------------------------------

// TestIntegrationPendingGatesReadiness is item 5.4, end to end: the method value
// goes into goga/serve's readiness option with no adapter, and the instance is
// out of rotation until its schema is up to date.
//
// The point is the failure it replaces. Without this, a service whose schema is
// behind accepts traffic and errors once per request against a table that is
// not there yet; with it, the load balancer never sends it any.
func TestIntegrationPendingGatesReadiness(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))

	pending, err := m.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 2, "both migrations are pending before the run")
	assert.Equal(t, int64(1), pending[0].Version)
	assert.True(t, pending[0].Pending)
	assert.Zero(t, pending[0].AppliedAt)

	require.Error(t, m.Ready(t.Context()), "a behind schema is not ready")

	h := servetest.Start(t.Context(), t, http.NotFoundHandler(),
		serve.WithReadinessCheck("migrations", m.Ready))

	status, body := h.Get(serve.ReadyzPath)
	assert.Equal(t, 503, status, "the instance is out of rotation while a migration is pending")
	assert.Contains(t, body, "migrations")

	_, err = m.Up(t.Context())
	require.NoError(t, err)

	after, err := m.Pending(t.Context())
	require.NoError(t, err)
	assert.Empty(t, after)
	assert.NoError(t, m.Ready(t.Context()))

	status, _ = h.Get(serve.ReadyzPath)
	assert.Equal(t, 200, status, "the same instance is ready once its schema is current")
}

// TestIntegrationStatusReportsEveryMigration covers the read side: Status names
// what the binary knows and what this database has, applied and pending alike.
func TestIntegrationStatusReportsEveryMigration(t *testing.T) {
	dsn := newDatabase(t)
	m := newMigrator(t, dsn, migrate.WithFS(sub(t, embeddedMigrations, "testdata/migrations")))

	_, err := m.UpTo(t.Context(), 1)
	require.NoError(t, err)

	statuses, err := m.Status(t.Context())
	require.NoError(t, err)

	require.Len(t, statuses, 2)
	assert.False(t, statuses[0].Pending)
	assert.NotZero(t, statuses[0].AppliedAt)
	assert.True(t, statuses[1].Pending)
	assert.Zero(t, statuses[1].AppliedAt)
}
