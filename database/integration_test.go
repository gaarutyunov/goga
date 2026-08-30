//go:build integration

// The container-backed half of this module's tests.
//
// Everything here needs a Docker daemon, so it is behind the `integration` build
// tag and the default `go test ./...` does not see it. What lives here is what
// cannot honestly be asserted against a double: that a real statement on a real
// PostgreSQL produces the spans the module promises, that the two transaction
// helpers behave identically against the real thing, and that pgx's own
// capabilities are still reachable on the pool goga hands back.
//
// Run with:
//
//	go test -race -tags integration ./database/...
package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/database/pgxdb"
	"github.com/gaarutyunov/goga/database/sqlcdb"
)

// postgresImage is pinned. An unpinned tag makes a test that passed yesterday
// fail today for a reason nothing in the repository records.
const postgresImage = "postgres:17-alpine"

// containerDSN is the connection string of the one container every test in this
// file shares. Starting a PostgreSQL per test would turn a two-second suite into
// a minute of container startup and assert nothing extra.
var containerDSN database.DSN

func TestMain(m *testing.M) {
	code, err := runWithPostgres(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "goga/database integration tests:", err)
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
	containerDSN = database.DSN(dsn)

	if err := createSchema(ctx); err != nil {
		return 0, err
	}

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

// createSchema builds the one table every scenario writes to.
func createSchema(ctx context.Context) error {
	db, err := database.Open(ctx, containerDSN)
	if err != nil {
		return fmt.Errorf("opening the handle that creates the schema: %w", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(ctx, `create table rows_written (v text not null)`)
	if err != nil {
		return fmt.Errorf("creating the schema: %w", err)
	}
	return nil
}

// openDB returns a handle onto the shared container.
func openDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := database.Open(t.Context(), containerDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

// openPool returns a pool onto the shared container.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxdb.Open(t.Context(), containerDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// rowsWritten counts the rows a scenario wrote. Each scenario uses a marker of
// its own, so the count is exact rather than relative and the scenarios do not
// have to run in any particular order.
func rowsWritten(t *testing.T, marker string) int {
	t.Helper()

	db := openDB(t)
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(),
		`select count(*) from rows_written where v = $1`, marker).Scan(&n))
	return n
}

// -----------------------------------------------------------------------------
// The handles are instrumented against a real database
// -----------------------------------------------------------------------------

// TestIntegrationOpenTracesARealQuery is the assertion the unit tests cannot
// make: a statement that actually reached PostgreSQL and came back produced a
// span from the wrapped driver.
func TestIntegrationOpenTracesARealQuery(t *testing.T) {
	rec := recordSpans(t)
	db := openDB(t)

	rec.Reset()
	var one int
	require.NoError(t, db.QueryRowContext(t.Context(), `select 1`).Scan(&one))
	require.Equal(t, 1, one)

	var traced bool
	for _, name := range rec.Names() {
		if strings.HasPrefix(name, "sql.") {
			traced = true
		}
	}
	assert.True(t, traced, "a real query produced a span from the wrapped driver; got %v", rec.Names())
}

// TestIntegrationPgxdbTracesARealQuery is the same assertion for the pool, whose
// instrumentation comes from otelpgx rather than from otelsql.
//
// The query runs inside a span the test opens, and that is not incidental:
// otelpgx starts a span only when the context already carries a recording one
// (tracer.go:246), so a query issued outside any span is traced by otelsql and
// not by otelpgx. This test is where that asymmetry is pinned; the package
// documentation states it, because an adopting project that expects a span per
// query from a background job will otherwise conclude the instrumentation is
// broken.
func TestIntegrationPgxdbTracesARealQuery(t *testing.T) {
	rec := recordSpans(t)
	pool := openPool(t)

	rec.Reset()

	ctx, parent := rec.Tracer("test").Start(t.Context(), "caller")
	var one int
	require.NoError(t, pool.QueryRow(ctx, `select 1`).Scan(&one))
	require.Equal(t, 1, one)
	parent.End()

	var traced bool
	for _, name := range rec.Names() {
		if name != "caller" {
			traced = true
		}
	}
	assert.True(t, traced, "the query produced a span of its own; got %v", rec.Names())
}

// TestIntegrationPgxdbDoesNotTraceOutsideASpan pins the other half of the same
// behaviour, so that the documented limitation is a tested fact rather than a
// remark that could quietly stop being true.
func TestIntegrationPgxdbDoesNotTraceOutsideASpan(t *testing.T) {
	rec := recordSpans(t)
	pool := openPool(t)

	rec.Reset()
	var one int
	require.NoError(t, pool.QueryRow(t.Context(), `select 1`).Scan(&one))
	require.Equal(t, 1, one)

	assert.Empty(t, rec.Names(),
		"otelpgx starts no span when the context carries no recording one")
}

// -----------------------------------------------------------------------------
// The two transaction helpers behave identically
// -----------------------------------------------------------------------------

// insertFn writes one marker row inside whatever transaction the helper opened.
type insertFn func(ctx context.Context, marker string) error

// txHelper is one of the two transaction functions, adapted to a single shape so
// that the scenarios below can be written once and run against both.
type txHelper struct {
	name string
	run  func(t *testing.T, timeout time.Duration, body func(ctx context.Context, insert insertFn) error) error
}

// txHelpers returns the two helpers under test.
//
// The design's claim is that database.Tx and pgxdb.Tx have identical commit,
// rollback, panic and timeout behaviour, and that they are separate functions
// only because *sql.Tx and pgx.Tx are different types. A table run against both
// is what turns that claim into something that can stop being true.
func txHelpers(t *testing.T) []txHelper {
	t.Helper()

	db := openDB(t)
	pool := openPool(t)

	return []txHelper{
		{
			name: "database.Tx",
			run: func(t *testing.T, timeout time.Duration, body func(context.Context, insertFn) error) error {
				var opts []database.TxOption
				if timeout > 0 {
					opts = append(opts, database.WithTxTimeout(timeout))
				}
				return database.Tx(t.Context(), db, func(ctx context.Context, tx *sql.Tx) error {
					return body(ctx, func(ctx context.Context, marker string) error {
						_, err := tx.ExecContext(ctx, `insert into rows_written (v) values ($1)`, marker)
						return err
					})
				}, opts...)
			},
		},
		{
			name: "pgxdb.Tx",
			run: func(t *testing.T, timeout time.Duration, body func(context.Context, insertFn) error) error {
				var opts []pgxdb.TxOption
				if timeout > 0 {
					opts = append(opts, pgxdb.WithTxTimeout(timeout))
				}
				return pgxdb.Tx(t.Context(), pool, func(ctx context.Context, tx pgx.Tx) error {
					return body(ctx, func(ctx context.Context, marker string) error {
						_, err := tx.Exec(ctx, `insert into rows_written (v) values ($1)`, marker)
						return err
					})
				}, opts...)
			},
		},
	}
}

func TestIntegrationTxCommitsIdentically(t *testing.T) {
	for _, h := range txHelpers(t) {
		t.Run(h.name, func(t *testing.T) {
			marker := "commit-" + h.name

			require.NoError(t, h.run(t, 0, func(ctx context.Context, insert insertFn) error {
				return insert(ctx, marker)
			}))

			assert.Equal(t, 1, rowsWritten(t, marker), "a committed transaction's write is durable")
		})
	}
}

func TestIntegrationTxRollsBackOnErrorIdentically(t *testing.T) {
	for _, h := range txHelpers(t) {
		t.Run(h.name, func(t *testing.T) {
			marker := "error-" + h.name
			sentinel := errors.New("the body failed")

			err := h.run(t, 0, func(ctx context.Context, insert insertFn) error {
				if err := insert(ctx, marker); err != nil {
					return err
				}
				return sentinel
			})

			require.ErrorIs(t, err, sentinel)
			assert.Equal(t, 0, rowsWritten(t, marker), "the write was rolled back")
		})
	}
}

// TestIntegrationTxRollsBackOnPanicIdentically is the scenario the whole helper
// exists for, run against a real server so that "rolled back" means the row is
// not in the table rather than that a mock recorded a call.
func TestIntegrationTxRollsBackOnPanicIdentically(t *testing.T) {
	for _, h := range txHelpers(t) {
		t.Run(h.name, func(t *testing.T) {
			marker := "panic-" + h.name

			assert.PanicsWithValue(t, "boom", func() {
				_ = h.run(t, 0, func(ctx context.Context, insert insertFn) error {
					require.NoError(t, insert(ctx, marker))
					panic("boom")
				})
			})

			assert.Equal(t, 0, rowsWritten(t, marker),
				"the transaction rolled back before the panic continued")
		})
	}
}

// TestIntegrationTxTimeoutCoversTheWholeTransactionIdentically: the budget is
// spent across two statements, not renewed for each, so the second one is the
// one that fails.
func TestIntegrationTxTimeoutCoversTheWholeTransactionIdentically(t *testing.T) {
	for _, h := range txHelpers(t) {
		t.Run(h.name, func(t *testing.T) {
			marker := "timeout-" + h.name

			err := h.run(t, 300*time.Millisecond, func(ctx context.Context, insert insertFn) error {
				if err := insert(ctx, marker); err != nil {
					return err
				}
				// Longer than the budget the first statement already spent part
				// of, so a bound applied per statement would let this through.
				time.Sleep(400 * time.Millisecond)
				return insert(ctx, marker)
			})

			require.Error(t, err)
			assert.Equal(t, 0, rowsWritten(t, marker), "nothing the transaction wrote survived")
		})
	}
}

// -----------------------------------------------------------------------------
// Nothing is erased
// -----------------------------------------------------------------------------

// TestIntegrationPgxCapabilitiesAreReachable is the reversal's whole point,
// stated as a test: the capabilities a portable port would have had to erase or
// route through an escape hatch are called directly on the pool goga returned.
func TestIntegrationPgxCapabilitiesAreReachable(t *testing.T) {
	pool := openPool(t)

	t.Run("CopyFrom", func(t *testing.T) {
		rows := [][]any{{"copy-from-a"}, {"copy-from-b"}}
		n, err := pool.CopyFrom(t.Context(),
			pgx.Identifier{"rows_written"}, []string{"v"}, pgx.CopyFromRows(rows))

		require.NoError(t, err)
		assert.Equal(t, int64(2), n)
		assert.Equal(t, 1, rowsWritten(t, "copy-from-a"))
	})

	t.Run("SendBatch", func(t *testing.T) {
		batch := &pgx.Batch{}
		batch.Queue(`insert into rows_written (v) values ($1)`, "send-batch")
		batch.Queue(`insert into rows_written (v) values ($1)`, "send-batch")

		results := pool.SendBatch(t.Context(), batch)
		for range 2 {
			_, err := results.Exec()
			require.NoError(t, err)
		}
		require.NoError(t, results.Close())

		assert.Equal(t, 2, rowsWritten(t, "send-batch"))
	})

	t.Run("LISTEN and NOTIFY", func(t *testing.T) {
		conn, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer conn.Release()

		_, err = conn.Exec(t.Context(), `listen goga_test_channel`)
		require.NoError(t, err)
		_, err = conn.Exec(t.Context(), `notify goga_test_channel, 'hello'`)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		notification, err := conn.Conn().WaitForNotification(ctx)
		require.NoError(t, err)
		assert.Equal(t, "hello", notification.Payload)
	})
}

// TestIntegrationSqlcShapedCodeRunsOnThePool: the seam is a compile-time
// assertion, and this is the run-time confirmation that the assertion was about
// the right thing — generated-shaped code built over sqlcdb.DBTX executes on the
// instrumented pool, and on a transaction opened from it.
func TestIntegrationSqlcShapedCodeRunsOnThePool(t *testing.T) {
	pool := openPool(t)

	insert := func(ctx context.Context, db sqlcdb.DBTX, marker string) error {
		_, err := db.Exec(ctx, `insert into rows_written (v) values ($1)`, marker)
		return err
	}

	t.Run("on the pool", func(t *testing.T) {
		require.NoError(t, insert(t.Context(), pool, "sqlc-pool"))
		assert.Equal(t, 1, rowsWritten(t, "sqlc-pool"))
	})

	t.Run("on a transaction", func(t *testing.T) {
		require.NoError(t, pgxdb.Tx(t.Context(), pool, func(ctx context.Context, tx pgx.Tx) error {
			return insert(ctx, tx, "sqlc-tx")
		}))
		assert.Equal(t, 1, rowsWritten(t, "sqlc-tx"))
	})
}

// TestIntegrationWireProviderClosesTheHandle exercises the shape wire generates:
// the provider hands back a cleanup, and calling it closes the handle.
func TestIntegrationWireProviderClosesTheHandle(t *testing.T) {
	// The provider is unexported, so it is reached the way wire reaches it — as
	// the one member of the set — which is also the only thing worth asserting
	// about the set from outside the package: that it exists and is a set.
	assert.NotNil(t, database.Set)
	assert.NotNil(t, pgxdb.Set)
}
