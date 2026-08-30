package telemetry

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"

	"github.com/gaarutyunov/goga/semconv"
)

// TestForResolvesProvidersLazily is checklist item 1.8, and the most important
// test in the module.
//
// It is written so that it fails if For snapshots a provider. The first Setup
// and its cleanup exist for that reason: OpenTelemetry's globals delegate
// tracers handed out before the *first* SetTracerProvider, and only then, so a
// snapshotting implementation would still pass a naive version of this test in
// a fresh process. After the first Setup that one-shot delegation is spent. The
// handle is then taken while the installed provider is one that has already
// been shut down, and the span is recorded after a second Setup has replaced
// it. A For that captured a provider would write into the dead one and the
// in-memory exporter below would stay empty.
func TestForResolvesProvidersLazily(t *testing.T) {
	_, firstCleanup, err := Setup(t.Context(), silentOptions()...)
	require.NoError(t, err)
	firstCleanup()

	// Taken before the providers this test asserts on exist.
	instr := For("lazyprobe")

	r, spans, _ := newTestRegistry(t)
	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, end := instr.Start(t.Context(), "afterSetup")
	end(nil)
	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))

	assert.Contains(t, spans.Names(), "goga.lazyprobe.afterSetup",
		"a handle taken before Setup emits through the providers Setup installed")
}

// TestForNeverFailsAndWorksBeforeSetup: a library can use a goga module without
// configuring telemetry at all.
func TestForNeverFailsAndWorksBeforeSetup(t *testing.T) {
	instr := For("beforeanything")
	require.NotNil(t, instr)
	assert.Equal(t, "beforeanything", instr.Module())
	require.NotNil(t, instr.Logger())

	ctx, end := instr.Start(t.Context(), "silent")
	assert.NotNil(t, trace.SpanFromContext(ctx))
	assert.NotPanics(t, func() { end(errors.New("nothing is listening")) })
}

func TestForReturnsTheSameHandlePerModule(t *testing.T) {
	assert.Same(t, For("shared"), For("shared"))
	assert.NotSame(t, For("shared"), For("other"))
}

// TestStartRecordsStatusDurationAndErrorType is checklist item 1.7, asserted on
// real recorded telemetry rather than on the internals of Instrumentation.
func TestStartRecordsStatusDurationAndErrorType(t *testing.T) {
	r, spans, metrics := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	instr := For("recordprobe")
	boom := errors.New("the operation failed")

	_, endOK := instr.Start(t.Context(), "succeeds")
	time.Sleep(time.Millisecond)
	endOK(nil)

	_, endErr := instr.Start(t.Context(), "fails")
	time.Sleep(time.Millisecond)
	endErr(boom)

	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))
	require.NoError(t, tel.MeterProvider.ForceFlush(t.Context()))

	t.Run("the span carries the status and the module attributes", func(t *testing.T) {
		byName := map[string]codes.Code{}
		for _, s := range spans.All() {
			byName[s.Name] = s.Status.Code
		}

		require.Contains(t, byName, "goga.recordprobe.succeeds")
		require.Contains(t, byName, "goga.recordprobe.fails")
		assert.Equal(t, codes.Ok, byName["goga.recordprobe.succeeds"])
		assert.Equal(t, codes.Error, byName["goga.recordprobe.fails"],
			"the closer observed the error, not a nil the return expression computed")

		for _, s := range spans.All() {
			if s.Name != "goga.recordprobe.fails" {
				continue
			}
			assert.Contains(t, s.Attributes, semconv.Module("recordprobe"))
			assert.Contains(t, s.Attributes, semconv.Operation("fails"))
			assert.NotEmpty(t, s.Events, "the error is recorded on the span")
		}
	})

	t.Run("the duration histogram records both operations", func(t *testing.T) {
		m, ok := metrics.find(semconv.OperationDurationName)
		require.True(t, ok, "goga.operation.duration was exported")
		assert.Equal(t, semconv.OperationDurationUnit, m.Unit)

		hist, ok := m.Data.(metricdata.Histogram[float64])
		require.True(t, ok, "the duration is a float64 histogram")
		require.Len(t, hist.DataPoints, 2, "one time series per operation")

		var total float64
		for _, dp := range hist.DataPoints {
			assert.Equal(t, uint64(1), dp.Count)
			total += dp.Sum
		}
		assert.Positive(t, total, "the recorded duration is not zero")
	})

	t.Run("error.type is set on failure and absent on success", func(t *testing.T) {
		m, ok := metrics.find(semconv.OperationDurationName)
		require.True(t, ok)
		hist, ok := m.Data.(metricdata.Histogram[float64])
		require.True(t, ok)

		withErrorType := 0
		for _, dp := range hist.DataPoints {
			if _, present := dp.Attributes.Value(semconv.ErrorTypeKey); present {
				withErrorType++
				op, _ := dp.Attributes.Value(semconv.OperationKey)
				assert.Equal(t, "fails", op.AsString())
			}
		}
		assert.Equal(t, 1, withErrorType, "exactly the failed operation carries error.type")
	})

	t.Run("the error counter counts only the failure", func(t *testing.T) {
		m, ok := metrics.find(semconv.OperationErrorsName)
		require.True(t, ok, "goga.operation.errors was exported")

		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sum.DataPoints, 1)
		assert.Equal(t, int64(1), sum.DataPoints[0].Value)

		errType, present := sum.DataPoints[0].Attributes.Value(semconv.ErrorTypeKey)
		require.True(t, present)
		assert.Equal(t, semconv.ErrorType(boom).Value.AsString(), errType.AsString())
	})
}

// TestStartCloserIsIdempotent: a caller that both defers the closer and calls it
// on an early return must not count the operation twice.
func TestStartCloserIsIdempotent(t *testing.T) {
	r, spans, metrics := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, end := For("idempotent").Start(t.Context(), "twice")
	end(nil)
	end(errors.New("ignored: the operation already ended"))

	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))
	require.NoError(t, tel.MeterProvider.ForceFlush(t.Context()))

	assert.Len(t, spans.All(), 1)
	if m, ok := metrics.find(semconv.OperationErrorsName); ok {
		sum, isSum := m.Data.(metricdata.Sum[int64])
		require.True(t, isSum)
		assert.Empty(t, sum.DataPoints, "the second call recorded no error")
	}
}

// TestStartSpanStaysReachableThroughTheContext is the property that lets a
// caller add attributes mid-operation without the closer having to take them.
func TestStartSpanStaysReachableThroughTheContext(t *testing.T) {
	r, spans, _ := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx, end := For("midflight").Start(t.Context(), "op")
	trace.SpanFromContext(ctx).SetAttributes(semconv.Operation("adjusted"))
	end(nil)

	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))

	stubs := spans.All()
	require.Len(t, stubs, 1)
	assert.Contains(t, stubs[0].Attributes, semconv.Operation("adjusted"))
}

func TestStartAttributesReachTheSpan(t *testing.T) {
	r, spans, _ := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, end := For("attrprobe").Start(t.Context(), "op", semconv.ServiceName("caller-supplied"))
	end(nil)
	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))

	stubs := spans.All()
	require.Len(t, stubs, 1)
	assert.Contains(t, stubs[0].Attributes, semconv.ServiceName("caller-supplied"))
}

func TestSameProviderToleratesUncomparableValues(t *testing.T) {
	type uncomparable struct{ _ []int }

	assert.True(t, sameProvider(nil, nil))
	assert.False(t, sameProvider(nil, 1))
	assert.False(t, sameProvider(uncomparable{}, uncomparable{}),
		"an uncomparable provider never matches the cache instead of panicking")

	same := &struct{ n int }{}
	assert.True(t, sameProvider(same, same))
}
