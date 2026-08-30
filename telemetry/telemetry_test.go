package telemetry

import (
	"errors"
	"strings"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/gaarutyunov/goga/semconv"
)

func TestSetupInstallsAllThreeProvidersGlobally(t *testing.T) {
	tel, cleanup, err := Setup(t.Context(), silentOptions()...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	require.NotNil(t, tel.TracerProvider)
	require.NotNil(t, tel.MeterProvider)
	require.NotNil(t, tel.LoggerProvider)

	assert.Same(t, tel.TracerProvider, otel.GetTracerProvider(), "the tracer provider is installed globally")
	assert.Same(t, tel.MeterProvider, otel.GetMeterProvider(), "the meter provider is installed globally")
	assert.Same(t, tel.LoggerProvider, global.GetLoggerProvider(), "the logger provider is installed globally")

	// All three or none: the handles are returned as well as installed, because
	// a consumer needing only a subset must not have to read the globals.
	assert.NotNil(t, tel.Tracer)
	assert.NotNil(t, tel.Meter)
	assert.NotNil(t, tel.Logger)
}

// TestSetupAlwaysInstallsAMeterProvider is checklist item 1.6. epos installs a
// tracer and forgets otel.SetMeterProvider; there must be no path through Setup
// that does the same, including the path where every metric exporter is
// switched off and the meter provider has no reader at all.
func TestSetupAlwaysInstallsAMeterProvider(t *testing.T) {
	before := otel.GetMeterProvider()

	tel, cleanup, err := Setup(t.Context(),
		WithTraceExporter(exporterNone),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	installed := otel.GetMeterProvider()
	assert.NotSame(t, before, installed, "Setup replaced whatever meter provider was installed")
	assert.Same(t, tel.MeterProvider, installed)
	assert.IsType(t, &sdkmetric.MeterProvider{}, installed,
		"the installed meter provider is a real SDK provider, not the global no-op")
}

// TestCleanupFlushesRecordedTelemetry is checklist item 1.9: the func() wire
// recognises has to actually flush, or every later module inherits a shutdown
// nothing calls.
func TestCleanupFlushesRecordedTelemetry(t *testing.T) {
	r, spans, _ := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), memoryOptions(r)...)
	require.NoError(t, err)

	_, span := tel.Tracer.Start(t.Context(), "before-cleanup")
	span.End()

	require.Empty(t, spans.GetSpans(), "the batch processor has not flushed yet")

	cleanup()

	assert.Contains(t, spanNames(spans), "before-cleanup",
		"the cleanup returned by Setup flushed the batched span")
}

// TestShutdownJoinsEveryProviderError is checklist item 1.4. A first-wins
// shutdown would report the logger provider's failure and hide the tracer
// provider's, which at shutdown is a permanent loss of data rather than a
// delay.
func TestShutdownJoinsEveryProviderError(t *testing.T) {
	errLogs := errors.New("the log processor is broken")
	errSpans := errors.New("the span processor is broken")

	tel := &Telemetry{
		LoggerProvider: sdklog.NewLoggerProvider(sdklog.WithProcessor(failingLogProcessor{err: errLogs})),
		MeterProvider:  sdkmetric.NewMeterProvider(),
		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(failingSpanProcessor{err: errSpans})),
	}

	err := tel.Shutdown(t.Context())

	require.Error(t, err)
	assert.ErrorIs(t, err, errLogs, "the first provider's failure is reported")
	assert.ErrorIs(t, err, errSpans, "and so is the failure of a provider behind it")
	assert.Contains(t, err.Error(), "goga/telemetry: shutdown")
}

func TestShutdownReportsNoErrorWhenEveryProviderStops(t *testing.T) {
	tel, cleanup, err := Setup(t.Context(), silentOptions()...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	assert.NoError(t, tel.Shutdown(t.Context()))
}

// TestPrometheusReaderIsAttachedByDefault is checklist item 1.5.
//
// It is the only test that leaves the Prometheus reader on. The exporter
// registers with prometheus.DefaultRegisterer — which is what makes a
// promhttp.Handler mounted by the application scrape goga's metrics with no
// further wiring — and two collectors in one process would produce a duplicate
// target_info at gather time.
func TestPrometheusReaderIsAttachedByDefault(t *testing.T) {
	_, cleanup, err := Setup(t.Context(),
		WithServiceName("goga-prometheus-test"),
		WithTraceExporter(exporterNone),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, end := For("prometheusprobe").Start(t.Context(), "scraped")
	end(nil)

	families, err := promclient.DefaultGatherer.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "goga_operation_duration") {
			found = true
			break
		}
	}
	assert.True(t, found, "goga.operation.duration is exposed to a Prometheus scrape by default")
}

// TestUnknownPropagatorFailsSetup keeps propagator selection honest for the
// same reason an unknown exporter name fails: a mistyped propagator that
// silently degraded to no propagation would break distributed tracing without
// breaking anything a test can see.
func TestUnknownPropagatorFailsSetup(t *testing.T) {
	_, _, err := Setup(t.Context(), append(silentOptions(), WithPropagators("not-a-propagator"))...)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/telemetry: propagators")
}

func TestNamedPropagatorsAreInstalled(t *testing.T) {
	_, cleanup, err := Setup(t.Context(), append(silentOptions(), WithPropagators("tracecontext", "baggage"))...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	fields := otel.GetTextMapPropagator().Fields()
	assert.Contains(t, fields, "traceparent")
	assert.Contains(t, fields, "baggage")
}

// TestOptionsValidateTheirInput checks the property the whole option shape
// exists for: a bad value fails at the call site that supplied it, not at first
// use somewhere deeper in the program.
func TestOptionsValidateTheirInput(t *testing.T) {
	tests := map[string]Option{
		"empty service name":    WithServiceName(""),
		"empty service version": WithServiceVersion(""),
		"empty trace exporter":  WithTraceExporter(""),
		"empty metric exporter": WithMetricExporter(""),
		"empty log exporter":    WithLogExporter(""),
		"zero shutdown timeout": WithShutdownTimeout(0),
		"negative timeout":      WithShutdownTimeout(-time.Second),
		"no propagators":        WithPropagators(),
		"empty propagator name": WithPropagators(""),
		"nil exporter registry": WithExporterRegistry(nil),
		"invalid resource attr": WithResourceAttributes(attribute.KeyValue{}),
	}

	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Setup(t.Context(), opt)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "goga/telemetry: setup",
				"the failure names the module it came from")
			assert.Contains(t, err.Error(), "goga: applying option",
				"and reads the same as any other option failure, whatever module produced it")
		})
	}
}

// TestResourceAttributesReachExportedSpans is checklist item 1.3: the service
// attributes are set from goga/semconv constants and end up on the resource
// every signal shares.
func TestResourceAttributesReachExportedSpans(t *testing.T) {
	r, spans, _ := newTestRegistry(t)

	tel, cleanup, err := Setup(t.Context(), append(memoryOptions(r),
		WithServiceVersion("1.2.3"),
		WithResourceAttributes(semconv.Module("resourceprobe")),
	)...)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	_, span := tel.Tracer.Start(t.Context(), "resourced")
	span.End()
	require.NoError(t, tel.TracerProvider.ForceFlush(t.Context()))

	stubs := spans.GetSpans()
	require.NotEmpty(t, stubs)

	attrs := stubs[0].Resource.Attributes()
	assert.Contains(t, attrs, semconv.ServiceName("goga-telemetry-test"))
	assert.Contains(t, attrs, semconv.ServiceVersion("1.2.3"))
	assert.Contains(t, attrs, semconv.Module("resourceprobe"))
}
