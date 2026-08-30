package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gaarutyunov/goga/registry"
)

// TestUnknownExporterNameFailsSetup is checklist item 1.2. A typo in a config
// file must not silently disable telemetry, and the failure must say what would
// have worked.
func TestUnknownExporterNameFailsSetup(t *testing.T) {
	r, _, _ := newTestRegistry(t)

	tests := map[string]struct {
		option Option
		signal string
	}{
		"trace":  {option: WithTraceExporter("nope"), signal: signalTrace},
		"metric": {option: WithMetricExporter("nope"), signal: signalMetric},
		"log":    {option: WithLogExporter("nope"), signal: signalLog},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Setup(t.Context(),
				WithExporterRegistry(r),
				WithTraceExporter(exporterNone),
				WithMetricExporter(exporterNone),
				WithLogExporter(exporterNone),
				WithPrometheus(false),
				tt.option,
			)

			require.Error(t, err)
			require.ErrorIs(t, err, ErrUnknownExporter)

			var unknown *UnknownExporterError
			require.ErrorAs(t, err, &unknown)
			assert.Equal(t, tt.signal, unknown.Signal)
			assert.Equal(t, "nope", unknown.Name)
			assert.Subset(t, unknown.Supported, standardExporters,
				"the error names every standard exporter")
			assert.Contains(t, err.Error(), "supported: ")
		})
	}
}

// TestUnknownExporterErrorListsRegisteredNames proves the message is actionable
// for the case that produces it: somebody registered "memory" and typed
// "memroy".
func TestUnknownExporterErrorListsRegisteredNames(t *testing.T) {
	r, _, _ := newTestRegistry(t)

	_, _, err := Setup(t.Context(),
		WithExporterRegistry(r),
		WithTraceExporter("memroy"),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	)

	require.Error(t, err)
	var unknown *UnknownExporterError
	require.ErrorAs(t, err, &unknown)
	assert.Contains(t, unknown.Supported, "memory",
		"the registered name is listed under its plain name, not its registry key")
}

// TestCustomExporterFromRegistryIsUsed is the other half of 1.2: a house
// exporter registered through the injected registry resolves and receives the
// telemetry.
func TestCustomExporterFromRegistryIsUsed(t *testing.T) {
	r, spans, metrics := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, span := tel.Tracer.Start(t.Context(), "custom-exporter")
	span.End()

	counter, err := tel.Meter.Int64Counter("goga.test.counter")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)

	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))
	require.NoError(t, tel.MeterProvider.ForceFlush(t.Context()))

	assert.Contains(t, spanNames(spans), "custom-exporter")
	_, ok := metrics.find("goga.test.counter")
	assert.True(t, ok, "the registered metric exporter received the recorded metric")
}

// TestRegistryNameTakesPrecedenceOverStandardName pins the resolution order.
// The registry is consulted first, so a house exporter is purely additive to
// the standard set and can never be shadowed by a standard name that happens to
// collide with it.
func TestRegistryNameTakesPrecedenceOverStandardName(t *testing.T) {
	r := registry.New(noDecode)
	spans := tracetest.NewInMemoryExporter()
	require.NoError(t, RegisterTraceExporter(r, exporterConsole,
		func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) { return spans, nil }))

	tel, cleanup, err := Setup(t.Context(),
		WithExporterRegistry(r),
		WithTraceExporter(exporterConsole),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, span := tel.Tracer.Start(t.Context(), "shadowed")
	span.End()
	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))

	assert.Contains(t, spanNames(spans), "shadowed",
		"the registered exporter won, and nothing was printed to stdout")
}

// TestDuplicateExporterNameIsAnErrorNotAPanic: registration happens in ordinary
// startup code, where an error can be reported.
func TestDuplicateExporterNameIsAnErrorNotAPanic(t *testing.T) {
	r := registry.New(noDecode)
	ctor := func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) {
		return tracetest.NewInMemoryExporter(), nil
	}

	require.NoError(t, RegisterTraceExporter(r, "twice", ctor))

	err := RegisterTraceExporter(r, "twice", ctor)

	require.Error(t, err)
	assert.ErrorIs(t, err, registry.ErrDuplicateName)
	assert.Contains(t, err.Error(), `register trace exporter "twice"`)
}

// TestSameNameForDifferentSignals: the registry is keyed by name alone and
// shared with every other goga module, so the three exporter kinds must not
// collide on a plain name.
func TestSameNameForDifferentSignals(t *testing.T) {
	r, _, _ := newTestRegistry(t) // registers "memory" for traces and metrics

	assert.Contains(t, r.Names(), registryKey(signalTrace, "memory"))
	assert.Contains(t, r.Names(), registryKey(signalMetric, "memory"))
}

func TestRegisterExporterRejectsBadArguments(t *testing.T) {
	ctor := func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) {
		return tracetest.NewInMemoryExporter(), nil
	}

	t.Run("nil registry", func(t *testing.T) {
		err := RegisterTraceExporter(nil, "memory", ctor)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "registry must not be nil")
	})

	t.Run("empty name", func(t *testing.T) {
		err := RegisterTraceExporter(registry.New(noDecode), "", ctor)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name must not be empty")
	})
}

// TestConstructorErrorFailsSetup: an exporter that dials at construction and
// cannot reach its collector fails startup rather than being dropped.
func TestConstructorErrorFailsSetup(t *testing.T) {
	r := registry.New(noDecode)
	boom := assert.AnError
	require.NoError(t, RegisterTraceExporter(r, "broken",
		func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) { return nil, boom }))

	_, _, err := Setup(t.Context(),
		WithExporterRegistry(r),
		WithTraceExporter("broken"),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), `goga/telemetry: trace exporter "broken"`)
}

func TestOTLPProtocolResolution(t *testing.T) {
	t.Run("defaults to http/protobuf", func(t *testing.T) {
		proto, err := otlpProtocol(envOTLPTracesProtocol)
		require.NoError(t, err)
		assert.Equal(t, protocolHTTP, proto)
	})

	t.Run("the signal specific variable wins", func(t *testing.T) {
		t.Setenv(envOTLPProtocol, protocolHTTP)
		t.Setenv(envOTLPTracesProtocol, protocolGRPC)

		proto, err := otlpProtocol(envOTLPTracesProtocol)
		require.NoError(t, err)
		assert.Equal(t, protocolGRPC, proto)
	})

	t.Run("an unsupported protocol is an error", func(t *testing.T) {
		t.Setenv(envOTLPProtocol, "carrier-pigeon")

		_, err := otlpProtocol(envOTLPTracesProtocol)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported OTLP protocol")
	})
}
