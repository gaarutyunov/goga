package pgxdb_test

import (
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/database/pgxdb"
	"github.com/gaarutyunov/goga/serve/servetest"
	"github.com/gaarutyunov/goga/telemetry"
)

// otelpgxPkgPath is the package the pool's tracer has to come from, so a tracer
// declared there is the runtime proof that the instrumentation was installed.
const otelpgxPkgPath = "github.com/exaring/otelpgx"

// unreachableDSN parses but cannot connect. pgxpool establishes connections on
// demand, so every test below builds a real pool over it without a database.
const unreachableDSN database.DSN = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"

// recordSpans installs a span recorder as the process tracer provider for the
// duration of the test. See the identical helper in goga/database's tests for
// why the recorder is servetest's rather than the OpenTelemetry SDK's.
func recordSpans(t *testing.T) *servetest.SpanRecorder {
	t.Helper()

	rec := servetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(rec)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// tracerPkgPath returns the package the pool's pgx tracer is declared in.
func tracerPkgPath(t *testing.T, cfgTracer any) string {
	t.Helper()
	require.NotNil(t, cfgTracer, "the pool has a tracer")

	typ := reflect.TypeOf(cfgTracer)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ.PkgPath()
}

// -----------------------------------------------------------------------------
// Open: the pool is instrumented, and there is no way to ask for one that is not
// -----------------------------------------------------------------------------

func TestOpenInstallsTheTracer(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Equal(t, otelpgxPkgPath, tracerPkgPath(t, pool.Config().ConnConfig.Tracer))
}

// TestNoOptionCombinationYieldsAnUninstrumentedPool walks every subset of the
// package's options, including the empty one. A test that checked the default
// and one interesting option would pass against an implementation where some
// third option removed the tracer.
func TestNoOptionCombinationYieldsAnUninstrumentedPool(t *testing.T) {
	opts := []struct {
		name string
		opt  pgxdb.Option
	}{
		{"WithMaxConns", pgxdb.WithMaxConns(5)},
		{"WithMinConns", pgxdb.WithMinConns(1)},
		{"WithTelemetry", pgxdb.WithTelemetry(telemetry.For("adopting-project"))},
	}

	for mask := range 1 << len(opts) {
		var (
			name     string
			selected []pgxdb.Option
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
			pool, err := pgxdb.Open(t.Context(), unreachableDSN, selected...)
			require.NoError(t, err)
			t.Cleanup(pool.Close)

			assert.Equal(t, otelpgxPkgPath, tracerPkgPath(t, pool.Config().ConnConfig.Tracer))
		})
	}
}

// TestOpenRecordsPoolStatistics proves otelpgx.RecordStats is on the path, by
// counting the callback registration it makes.
//
// The meter below is hand-written rather than generated, which is the one place
// in this module's tests where that is true: [metric.Meter] embeds
// go.opentelemetry.io/otel/metric/embedded.Meter, whose method is unexported, so
// no type declared outside OpenTelemetry can satisfy the interface without
// embedding one of OpenTelemetry's own. A generated mock cannot, and the
// noop implementation is what the API ships for exactly this purpose.
func TestOpenRecordsPoolStatistics(t *testing.T) {
	m := &countingMeter{}
	installMeterProvider(t, m)

	pool, err := pgxdb.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Positive(t, m.registrations.Load(),
		"otelpgx.RecordStats registered its observable callback")
}

// TestOpenFailsWhenPoolStatisticsCannotBeRecorded: a pool whose statistics could
// not be registered is not returned half-instrumented. It is closed and the
// error is surfaced, because a caller holding a pool it believes is instrumented
// is worse than a caller holding an error.
func TestOpenFailsWhenPoolStatisticsCannotBeRecorded(t *testing.T) {
	sentinel := errors.New("meter refused the callback")
	installMeterProvider(t, &countingMeter{err: sentinel})

	pool, err := pgxdb.Open(t.Context(), unreachableDSN)

	require.ErrorIs(t, err, sentinel)
	assert.Nil(t, pool)
}

func TestOpenRecordsItsOwnSpan(t *testing.T) {
	rec := recordSpans(t)

	pool, err := pgxdb.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Contains(t, rec.Names(), "goga.database/pgxdb.Open")
}

func TestWithTelemetryReplacesTheModuleHandle(t *testing.T) {
	rec := recordSpans(t)

	pool, err := pgxdb.Open(t.Context(), unreachableDSN,
		pgxdb.WithTelemetry(telemetry.For("adopting-project")))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Contains(t, rec.Names(), "goga.adopting-project.Open")
	assert.NotContains(t, rec.Names(), "goga.database/pgxdb.Open")

	// Replaced, not removed: the pool still has its tracer.
	assert.Equal(t, otelpgxPkgPath, tracerPkgPath(t, pool.Config().ConnConfig.Tracer))
}

func TestWithTelemetryRejectsNil(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(), unreachableDSN, pgxdb.WithTelemetry(nil))

	require.Error(t, err)
	assert.Nil(t, pool)
	assert.Contains(t, err.Error(), "never disabled")
}

// -----------------------------------------------------------------------------
// Open: settings and validation
// -----------------------------------------------------------------------------

func TestOpenRejectsAnUnparseableDSN(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(), "postgres://user@host:not-a-port/db")

	require.Error(t, err)
	assert.Nil(t, pool)
	assert.Contains(t, err.Error(), "parsing dsn")
}

func TestOpenAppliesPoolSettings(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(), unreachableDSN, pgxdb.WithMaxConns(6), pgxdb.WithMinConns(2))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Equal(t, int32(6), pool.Config().MaxConns)
	assert.Equal(t, int32(2), pool.Config().MinConns)
}

// TestOpenLeavesTheDSNsOwnPoolSettingsAlone: pgx accepts pool_max_conns in the
// connection string, and an option nobody passed must not silently overwrite it
// with a goga default.
func TestOpenLeavesTheDSNsOwnPoolSettingsAlone(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(),
		unreachableDSN+"&pool_max_conns=9")
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	assert.Equal(t, int32(9), pool.Config().MaxConns)
}

func TestOptionsRejectBadValues(t *testing.T) {
	tests := map[string]pgxdb.Option{
		"zero max conns":     pgxdb.WithMaxConns(0),
		"negative max conns": pgxdb.WithMaxConns(-1),
		"zero min conns":     pgxdb.WithMinConns(0),
		"negative min conns": pgxdb.WithMinConns(-3),
	}

	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			pool, err := pgxdb.Open(t.Context(), unreachableDSN, opt)

			require.Error(t, err)
			assert.Nil(t, pool)
		})
	}
}

// -----------------------------------------------------------------------------
// A meter that counts the callbacks registered on it
// -----------------------------------------------------------------------------

// countingMeter embeds the OpenTelemetry API's no-op meter and overrides the one
// method the assertions read back. Everything else — every instrument
// constructor otelpgx calls before it registers — comes from the no-op and does
// what it always does.
type countingMeter struct {
	metricnoop.Meter

	registrations atomic.Int64
	err           error
}

func (m *countingMeter) RegisterCallback(
	f metric.Callback, instruments ...metric.Observable,
) (metric.Registration, error) {
	m.registrations.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	return m.Meter.RegisterCallback(f, instruments...)
}

// countingMeterProvider hands out one countingMeter.
type countingMeterProvider struct {
	metricnoop.MeterProvider

	m *countingMeter
}

func (p *countingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter { return p.m }

// installMeterProvider makes m the process meter provider for the duration of
// the test. It installs a global, so a test that calls it must not be parallel.
func installMeterProvider(t *testing.T, m *countingMeter) {
	t.Helper()

	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(&countingMeterProvider{m: m})
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
}
