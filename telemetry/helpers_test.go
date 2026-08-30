package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gaarutyunov/goga/registry"
)

// silentOptions configure a Setup that exports nothing anywhere. Every test but
// TestPrometheusReaderIsAttachedByDefault uses them, so that no test opens a
// socket to a collector that is not there and no test registers a second
// collector with prometheus.DefaultRegisterer.
func silentOptions() []Option {
	return []Option{
		WithServiceName("goga-telemetry-test"),
		WithServiceVersion("0.0.0-test"),
		WithTraceExporter(exporterNone),
		WithMetricExporter(exporterNone),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	}
}

// noDecode is the decoder the test registry is built with. The exporters
// registered below take no raw settings, so [registry.Registry] never calls it;
// making it fail loudly means a test that starts depending on decoding says so.
func noDecode(registry.Settings, any) error {
	return errors.New("the test registry has no decoder")
}

// exporterSettings is the settings type the test exporter constructors take. It
// is empty on purpose: what the tests exercise is the registration and
// resolution path, not settings decoding, which registry's own tests cover.
type exporterSettings struct{}

// newTestRegistry returns a registry holding an in-memory span exporter and a
// capturing metric exporter, both registered under the same plain name.
//
// The shared name is deliberate: it is what proves the signal-qualified
// registry keys work. The registry is keyed by name alone and shared with every
// other goga module, so without the qualification the second registration here
// would fail as a duplicate.
func newTestRegistry(t *testing.T) (*registry.Registry, *tracetest.InMemoryExporter, *capturingMetricExporter) {
	t.Helper()

	r := registry.New(noDecode)
	spans := tracetest.NewInMemoryExporter()
	metrics := &capturingMetricExporter{}

	require.NoError(t, RegisterTraceExporter(r, "memory",
		func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) {
			return persistentSpanExporter{spans}, nil
		}))
	require.NoError(t, RegisterMetricExporter(r, "memory",
		func(context.Context, exporterSettings) (sdkmetric.Exporter, error) { return metrics, nil }))

	return r, spans, metrics
}

// memoryOptions configure a Setup whose traces and metrics land in r's
// in-memory exporters and whose logs go nowhere.
func memoryOptions(r *registry.Registry) []Option {
	return []Option{
		WithServiceName("goga-telemetry-test"),
		WithExporterRegistry(r),
		WithTraceExporter("memory"),
		WithMetricExporter("memory"),
		WithLogExporter(exporterNone),
		WithPrometheus(false),
	}
}

// persistentSpanExporter keeps what it recorded across Shutdown.
//
// tracetest.InMemoryExporter.Shutdown resets its own buffer, which would make a
// test asserting that the cleanup flushed a batched span indistinguishable from
// a test asserting that the cleanup threw it away.
type persistentSpanExporter struct{ *tracetest.InMemoryExporter }

func (persistentSpanExporter) Shutdown(context.Context) error { return nil }

// capturingMetricExporter is a push exporter that keeps what it was handed, so
// that a test can assert on real recorded metrics rather than on the internals
// of the type that recorded them.
type capturingMetricExporter struct {
	mu        sync.Mutex
	collected []metricdata.Metrics
}

func (e *capturingMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (e *capturingMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

// Export copies what it needs out of rm, which the SDK is free to reuse the
// moment this returns.
func (e *capturingMetricExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, scope := range rm.ScopeMetrics {
		e.collected = append(e.collected, scope.Metrics...)
	}
	return nil
}

func (e *capturingMetricExporter) ForceFlush(context.Context) error { return nil }
func (e *capturingMetricExporter) Shutdown(context.Context) error   { return nil }

// find returns the last exported metric with the given name.
func (e *capturingMetricExporter) find(name string) (metricdata.Metrics, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.collected) - 1; i >= 0; i-- {
		if e.collected[i].Name == name {
			return e.collected[i], true
		}
	}
	return metricdata.Metrics{}, false
}

// failingSpanProcessor fails every flush and every shutdown, so that a test can
// make a tracer provider's Shutdown return an error it can match on.
type failingSpanProcessor struct{ err error }

func (failingSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (failingSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (p failingSpanProcessor) Shutdown(context.Context) error                { return p.err }
func (p failingSpanProcessor) ForceFlush(context.Context) error              { return p.err }

// failingLogProcessor is the same idea for the logger provider.
type failingLogProcessor struct{ err error }

func (failingLogProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (failingLogProcessor) OnEmit(context.Context, *sdklog.Record) error           { return nil }
func (p failingLogProcessor) Shutdown(context.Context) error                       { return p.err }
func (p failingLogProcessor) ForceFlush(context.Context) error                     { return p.err }

// spanNames returns the names of every span the in-memory exporter received.
func spanNames(exp *tracetest.InMemoryExporter) []string {
	stubs := exp.GetSpans()
	names := make([]string, 0, len(stubs))
	for _, s := range stubs {
		names = append(names, s.Name)
	}
	return names
}
