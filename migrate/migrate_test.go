// The half of this module's tests that needs no database.
//
// What lives here is everything that can be decided from the options and the
// migration filesystem alone: which arguments are rejected, which of [WithFS]
// and [WithDir] wins, and that [migrate.Migrator.Ready] has the exact shape
// goga/serve's readiness option takes. Everything that depends on what a real
// PostgreSQL does — the advisory lock above all — is in integration_test.go,
// behind the `integration` build tag.
package migrate_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/migrate"
	"github.com/gaarutyunov/goga/serve"
)

// unreachableDSN parses but cannot connect: port 1 on the loopback interface
// refuses immediately. migrate.New does not connect — it reads the migration
// filesystem and builds the provider — so a handle onto nothing is enough for
// every test in this file, and none of them needs Docker.
const unreachableDSN database.DSN = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"

// emptyFS holds no migrations. goose rejects it, which is what makes it useful:
// it is the observable difference between "this filesystem was used" and "this
// filesystem was overridden".
var emptyFS = fstest.MapFS{}

func TestNewRejectsANilHandle(t *testing.T) {
	_, err := migrate.New(nil, migrate.WithDir("testdata/migrations"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/migrate: db must not be nil")
}

// TestNewRejectsAMigratorWithNoMigrations: there is no default filesystem, on
// purpose. A migrator that silently applies nothing is the deployment accident
// embedding exists to remove, so the absence is an error at construction.
func TestNewRejectsAMigratorWithNoMigrations(t *testing.T) {
	_, err := migrate.New(handle(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no migrations")
	assert.Contains(t, err.Error(), "WithFS", "the error names the option that fixes it")
	assert.Contains(t, err.Error(), "WithDir")
}

// TestNewRejectsASingleConnectionPool pins the deadlock guard. The advisory
// lock holds one connection for the whole run and goose needs a second, so a
// pool of one would wait for a connection the run itself is holding — a hang at
// boot, which is the worst possible shape for this failure.
func TestNewRejectsASingleConnectionPool(t *testing.T) {
	db := handle(t)
	db.SetMaxOpenConns(1)

	_, err := migrate.New(db, migrate.WithDir("testdata/migrations"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one open connection")
}

func TestNewRejectsAFilesystemWithNoMigrationsInIt(t *testing.T) {
	_, err := migrate.New(handle(t), migrate.WithFS(emptyFS))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/migrate: ")
	assert.ErrorIs(t, err, goose.ErrNoMigrations)
}

// TestOptionsValidateTheirOwnInput is the house rule from goga's root package:
// a bad value fails at the call site that supplied it, not at first use several
// layers deeper.
func TestOptionsValidateTheirOwnInput(t *testing.T) {
	tests := map[string]struct {
		opt  migrate.Option
		want string
	}{
		"a nil filesystem":     {migrate.WithFS(nil), "migrations fs must not be nil"},
		"an empty dir":         {migrate.WithDir(""), "migrations dir must not be empty"},
		"an empty table name":  {migrate.WithTable(""), "version table name must not be empty"},
		"an empty dialect":     {migrate.WithDialect(""), "dialect must not be empty"},
		"a zero lock timeout":  {migrate.WithLockTimeout(0), "lock timeout must be > 0"},
		"a negative timeout":   {migrate.WithLockTimeout(-time.Second), "lock timeout must be > 0"},
		"a nil session locker": {migrate.WithSessionLocker(nil), "session locker must not be nil"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := migrate.New(handle(t), migrate.WithDir("testdata/migrations"), tt.opt)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// TestWithSessionLockerCannotDisableLocking states the guarantee in the shape a
// caller would try to break it: nil is not a way to ask for an unlocked run.
func TestWithSessionLockerCannotDisableLocking(t *testing.T) {
	_, err := migrate.New(handle(t),
		migrate.WithDir("testdata/migrations"), migrate.WithSessionLocker(nil))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "can never be disabled")
}

// TestWithDirAndWithFSAreOneSetting: they write the same field, so the later
// option wins outright and there is no precedence rule to remember. The empty
// filesystem is what makes the winner observable — goose rejects it, and a
// migrator that was built is a migrator that read the other one.
func TestWithDirAndWithFSAreOneSetting(t *testing.T) {
	t.Run("WithDir after WithFS wins", func(t *testing.T) {
		m, err := migrate.New(handle(t),
			migrate.WithFS(emptyFS), migrate.WithDir("testdata/migrations"))

		require.NoError(t, err)
		assert.NotNil(t, m)
	})

	t.Run("WithFS after WithDir wins", func(t *testing.T) {
		_, err := migrate.New(handle(t),
			migrate.WithDir("testdata/migrations"), migrate.WithFS(emptyFS))

		require.ErrorIs(t, err, goose.ErrNoMigrations)
	})
}

// TestNewReadsTheMigrationsAtConstruction: a duplicate version is a defect in
// the migration set, and it is reported when the migrator is built rather than
// at boot, when the process is already trying to serve.
func TestNewReadsTheMigrationsAtConstruction(t *testing.T) {
	duplicates := fstest.MapFS{
		"00001_first.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nselect 1;\n")},
		"00001_second.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nselect 1;\n")},
	}

	_, err := migrate.New(handle(t), migrate.WithFS(duplicates))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/migrate: new:")
}

// TestProviderIsTheEscapeHatch: item 5.8. The provider underneath is reachable,
// so a project needing Redo or DownTo is not told the framework does not
// support it.
func TestProviderIsTheEscapeHatch(t *testing.T) {
	m, err := migrate.New(handle(t), migrate.WithDir("testdata/migrations"))
	require.NoError(t, err)

	p := m.Provider()

	require.NotNil(t, p)
	assert.Len(t, p.ListSources(), 2, "the provider holds the migrations the migrator read")
}

// TestReadyHasTheReadinessCheckShape is item 5.4's compile-time half: the
// method value goes straight into serve.WithReadinessCheck with no adapter, and
// the option is accepted. Whether it reports correctly is the integration
// suite's question, because it takes a database to have a pending migration.
func TestReadyHasTheReadinessCheckShape(t *testing.T) {
	m, err := migrate.New(handle(t), migrate.WithDir("testdata/migrations"))
	require.NoError(t, err)

	// The conversion is the assertion: readinessCheck is the exact type
	// serve.WithReadinessCheck takes, and Ready has to be it with no adapter.
	check := readinessCheck(m.Ready)
	require.NotNil(t, check)

	srv, err := serve.New(t.Context(), http.NotFoundHandler(),
		serve.WithReadinessCheck("migrations", m.Ready))

	require.NoError(t, err)
	assert.NotNil(t, srv)
}

// readinessCheck is the shape goga/serve's readiness option takes. It is
// written out here so that the conversion above fails to compile the day
// Migrator.Ready stops matching it.
type readinessCheck = func(ctx context.Context) error

func TestSetIsAProviderSet(t *testing.T) {
	assert.NotNil(t, migrate.Set)
}

// handle returns an instrumented *sql.DB that will never reach a server.
func handle(t *testing.T) *sql.DB {
	t.Helper()

	db, err := database.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}
