package database_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/serve/servetest"
	"github.com/gaarutyunov/goga/telemetry"
)

// otelsqlPkgPath is the package the instrumented driver has to come from. It is
// the wrapping otelsql applies, so the returned handle's driver being of a type
// declared here is the runtime proof that the wrapping happened.
const otelsqlPkgPath = "github.com/XSAM/otelsql"

// unreachableDSN parses but cannot connect: port 1 on the loopback interface
// refuses immediately. It is what lets the tests below exercise the paths that
// need a connection attempt — and the telemetry those paths emit — without a
// database and without a timeout.
const unreachableDSN database.DSN = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"

// recordSpans installs a span recorder as the process tracer provider for the
// duration of the test and returns it.
//
// The recorder is goga/serve/servetest's, built on the OpenTelemetry API rather
// than on the SDK: goga/telemetry owns the SDK and goga's own lint configuration
// bans importing it anywhere else, so asserting on real spans from outside that
// package is exactly what this type exists for.
//
// It installs a global, so a test that calls it must not be parallel.
func recordSpans(t *testing.T) *servetest.SpanRecorder {
	t.Helper()

	rec := servetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(rec)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// driverPkgPath returns the package the handle's driver type is declared in.
func driverPkgPath(t *testing.T, db *sql.DB) string {
	t.Helper()
	require.NotNil(t, db)

	typ := reflect.TypeOf(db.Driver())
	require.NotNil(t, typ, "the handle has a driver")
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ.PkgPath()
}

// -----------------------------------------------------------------------------
// Open: the handle is instrumented, and there is no way to ask for one that is not
// -----------------------------------------------------------------------------

// TestOpenReturnsAHandleWhoseDriverIsWrapped is the runtime half of the module's
// guarantee. The structural half — that Open is the only exported way to get a
// handle at all — is in api_test.go.
func TestOpenReturnsAHandleWhoseDriverIsWrapped(t *testing.T) {
	db, err := database.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	assert.Equal(t, otelsqlPkgPath, driverPkgPath(t, db),
		"the handle's driver is otelsql's wrapper, not the raw pgx driver")
}

// TestNoOptionCombinationYieldsAnUninstrumentedHandle walks every subset of the
// module's options, including the empty one, and asserts the driver is wrapped
// in all of them.
//
// A test that checked the default and one interesting option would pass against
// an implementation where some third option disabled the wrapping, and "no
// option combination" is the actual promise the package documentation makes.
func TestNoOptionCombinationYieldsAnUninstrumentedHandle(t *testing.T) {
	opts := []struct {
		name string
		opt  database.Option
	}{
		{"WithMaxOpenConns", database.WithMaxOpenConns(3)},
		{"WithMaxIdleConns", database.WithMaxIdleConns(2)},
		{"WithConnMaxLifetime", database.WithConnMaxLifetime(time.Minute)},
		{"WithSQLCommenter(true)", database.WithSQLCommenter(true)},
		{"WithSQLCommenter(false)", database.WithSQLCommenter(false)},
		{"WithTelemetry", database.WithTelemetry(telemetry.For("adopting-project"))},
	}

	for mask := range 1 << len(opts) {
		var (
			name     string
			selected []database.Option
		)
		for i, o := range opts {
			if mask&(1<<i) != 0 {
				name += o.name + " "
				selected = append(selected, o.opt)
			}
		}
		if name == "" {
			name = "no options"
		}

		t.Run(name, func(t *testing.T) {
			db, err := database.Open(t.Context(), unreachableDSN, selected...)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, db.Close()) })

			assert.Equal(t, otelsqlPkgPath, driverPkgPath(t, db))
		})
	}
}

// TestOpenTracesAConnectionAttempt proves the wrapping is live rather than
// merely present: a real connection attempt through the returned handle produces
// a real span from otelsql, with no database involved.
func TestOpenTracesAConnectionAttempt(t *testing.T) {
	rec := recordSpans(t)

	db, err := database.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	rec.Reset()
	require.Error(t, db.PingContext(t.Context()), "127.0.0.1:1 refuses connections")

	assert.Contains(t, rec.Names(), "sql.connector.connect",
		"the failed connection was traced by the wrapped driver")

	var connect servetest.RecordedSpan
	for _, s := range rec.Ended() {
		if s.Name == "sql.connector.connect" {
			connect = s
		}
	}
	assert.Equal(t, codes.Error, connect.Status, "the refused connection is recorded as an error")

	var systems []string
	for _, kv := range connect.Attributes {
		if string(kv.Key) == "db.system.name" {
			systems = append(systems, kv.Value.AsString())
		}
	}
	assert.Equal(t, []string{"postgresql"}, systems,
		"the span carries the official database attribute")
}

// TestOpenRecordsItsOwnSpan checks goga's construction span, which is separate
// from otelsql's per-statement spans and is what [database.WithTelemetry]
// re-attributes.
func TestOpenRecordsItsOwnSpan(t *testing.T) {
	rec := recordSpans(t)

	db, err := database.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	assert.Contains(t, rec.Names(), "goga.database.Open")
}

// TestWithTelemetryReplacesTheModuleHandle is the "replaces" half of the option's
// promise. The "never disables" half is [TestNoOptionCombinationYieldsAnUninstrumentedHandle]
// above and [TestWithTelemetryRejectsNil] below.
func TestWithTelemetryReplacesTheModuleHandle(t *testing.T) {
	rec := recordSpans(t)

	db, err := database.Open(t.Context(), unreachableDSN,
		database.WithTelemetry(telemetry.For("adopting-project")))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	assert.Contains(t, rec.Names(), "goga.adopting-project.Open")
	assert.NotContains(t, rec.Names(), "goga.database.Open")

	// Replaced, not removed: the driver is still wrapped.
	assert.Equal(t, otelsqlPkgPath, driverPkgPath(t, db))
}

// TestWithTelemetryRejectsNil: nil is not a way to ask for silence.
func TestWithTelemetryRejectsNil(t *testing.T) {
	db, err := database.Open(t.Context(), unreachableDSN, database.WithTelemetry(nil))

	require.Error(t, err)
	assert.Nil(t, db)
	assert.Contains(t, err.Error(), "never disabled")
}

// -----------------------------------------------------------------------------
// Open: settings and validation
// -----------------------------------------------------------------------------

func TestOpenRejectsAnUnparseableDSN(t *testing.T) {
	db, err := database.Open(t.Context(), "postgres://user@host:not-a-port/db")

	require.Error(t, err)
	assert.Nil(t, db)
	assert.Contains(t, err.Error(), "parsing dsn")
}

func TestOpenAppliesPoolSettings(t *testing.T) {
	db, err := database.Open(t.Context(), unreachableDSN, database.WithMaxOpenConns(7))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	assert.Equal(t, 7, db.Stats().MaxOpenConnections)
}

func TestOpenDefaultsAreBounded(t *testing.T) {
	db, err := database.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	assert.Positive(t, db.Stats().MaxOpenConnections,
		"the default pool is bounded; database/sql's own default is unlimited")
}

func TestOptionsRejectBadValues(t *testing.T) {
	tests := map[string]database.Option{
		"zero max open conns":     database.WithMaxOpenConns(0),
		"negative max open conns": database.WithMaxOpenConns(-1),
		"zero max idle conns":     database.WithMaxIdleConns(0),
		"zero conn max lifetime":  database.WithConnMaxLifetime(0),
		"negative lifetime":       database.WithConnMaxLifetime(-time.Second),
	}

	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			db, err := database.Open(t.Context(), unreachableDSN, opt)

			require.Error(t, err)
			assert.Nil(t, db)
		})
	}
}

func TestTxOptionsRejectBadValues(t *testing.T) {
	db, _, _ := newMockDB(t)

	err := database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { return nil },
		database.WithTxTimeout(0))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be > 0")
}
