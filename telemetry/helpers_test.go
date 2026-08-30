package telemetry

//go:generate go tool mockgen -destination mock_spanexporter_test.go -package telemetry go.opentelemetry.io/otel/sdk/trace SpanExporter
//go:generate go tool mockgen -destination mock_spanprocessor_test.go -package telemetry go.opentelemetry.io/otel/sdk/trace SpanProcessor
//go:generate go tool mockgen -destination mock_logprocessor_test.go -package telemetry go.opentelemetry.io/otel/sdk/log Processor
//go:generate go tool mockgen -destination mock_metricexporter_test.go -package telemetry go.opentelemetry.io/otel/sdk/metric Exporter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/mock/gomock"

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

// newTestRegistry returns a registry holding a span exporter and a metric
// exporter, both registered under the same plain name, together with the sinks
// holding whatever they were handed.
//
// The shared name is deliberate: it is what proves the signal-qualified
// registry keys work. The registry is keyed by name alone and shared with every
// other goga module, so without the qualification the second registration here
// would fail as a duplicate.
func newTestRegistry(t *testing.T) (*registry.Registry, *spanSink, *metricSink) {
	t.Helper()

	r := registry.New(noDecode)
	spanExporter, spans := newSpanExporter(t)
	metricExporter, metrics := newMetricExporter(t)

	require.NoError(t, RegisterTraceExporter(r, "memory",
		func(context.Context, exporterSettings) (sdktrace.SpanExporter, error) { return spanExporter, nil }))
	require.NoError(t, RegisterMetricExporter(r, "memory",
		func(context.Context, exporterSettings) (sdkmetric.Exporter, error) { return metricExporter, nil }))

	return r, spans, metrics
}

// memoryOptions configure a Setup whose traces and metrics land in r's sinks
// and whose logs go nowhere.
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

// spanSink holds every span an exporter was handed. It keeps them as
// tracetest.SpanStubs — the upstream value snapshot — because a ReadOnlySpan
// belongs to the SDK and is not promised to stay readable after the export call
// returns.
type spanSink struct {
	mu    sync.Mutex
	spans tracetest.SpanStubs
}

func (s *spanSink) record(spans []sdktrace.ReadOnlySpan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, tracetest.SpanStubsFromReadOnlySpans(spans)...)
}

// All returns every span exported so far.
func (s *spanSink) All() tracetest.SpanStubs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slicesClone(s.spans)
}

// Names returns the names of every span exported so far.
func (s *spanSink) Names() []string {
	stubs := s.All()
	names := make([]string, 0, len(stubs))
	for _, stub := range stubs {
		names = append(names, stub.Name)
	}
	return names
}

// newSpanExporter returns a span exporter that records into a sink.
//
// Its Shutdown succeeds without discarding anything, which
// tracetest.InMemoryExporter cannot do — that one resets its buffer on
// Shutdown, so a test asserting that the cleanup flushed a batched span would
// be indistinguishable from a test asserting that the cleanup threw it away.
func newSpanExporter(t *testing.T) (*MockSpanExporter, *spanSink) {
	t.Helper()

	sink := &spanSink{}
	exporter := NewMockSpanExporter(gomock.NewController(t))
	exporter.EXPECT().
		ExportSpans(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
			sink.record(spans)
			return nil
		}).
		AnyTimes()
	exporter.EXPECT().Shutdown(gomock.Any()).Return(nil).AnyTimes()

	return exporter, sink
}

// metricSink holds every metric a push exporter was handed.
type metricSink struct {
	mu        sync.Mutex
	collected []metricdata.Metrics
}

func (s *metricSink) record(rm *metricdata.ResourceMetrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range rm.ScopeMetrics {
		s.collected = append(s.collected, scope.Metrics...)
	}
}

// find returns the most recently exported metric with the given name.
func (s *metricSink) find(name string) (metricdata.Metrics, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.collected) - 1; i >= 0; i-- {
		if s.collected[i].Name == name {
			return s.collected[i], true
		}
	}
	return metricdata.Metrics{}, false
}

// newMetricExporter returns a push exporter that records into a sink, so that a
// test can assert on real recorded metrics rather than on the internals of the
// type that recorded them. The OpenTelemetry Go SDK ships no in-memory metric
// exporter, which is why this one is generated rather than borrowed.
func newMetricExporter(t *testing.T) (*MockExporter, *metricSink) {
	t.Helper()

	sink := &metricSink{}
	exporter := NewMockExporter(gomock.NewController(t))
	exporter.EXPECT().Temporality(gomock.Any()).DoAndReturn(sdkmetric.DefaultTemporalitySelector).AnyTimes()
	exporter.EXPECT().Aggregation(gomock.Any()).DoAndReturn(sdkmetric.DefaultAggregationSelector).AnyTimes()
	exporter.EXPECT().
		Export(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, rm *metricdata.ResourceMetrics) error {
			// rm may be reused the moment this returns.
			sink.record(rm)
			return nil
		}).
		AnyTimes()
	exporter.EXPECT().ForceFlush(gomock.Any()).Return(nil).AnyTimes()
	exporter.EXPECT().Shutdown(gomock.Any()).Return(nil).AnyTimes()

	return exporter, sink
}

// newFailingSpanProcessor returns a span processor that fails every flush and
// every shutdown, so that a test can make a tracer provider's Shutdown return
// an error it can match on.
func newFailingSpanProcessor(t *testing.T, err error) *MockSpanProcessor {
	t.Helper()

	processor := NewMockSpanProcessor(gomock.NewController(t))
	processor.EXPECT().OnStart(gomock.Any(), gomock.Any()).AnyTimes()
	processor.EXPECT().OnEnd(gomock.Any()).AnyTimes()
	processor.EXPECT().ForceFlush(gomock.Any()).Return(err).AnyTimes()
	processor.EXPECT().Shutdown(gomock.Any()).Return(err).AnyTimes()

	return processor
}

// newFailingLogProcessor is the same idea for the logger provider.
func newFailingLogProcessor(t *testing.T, err error) *MockProcessor {
	t.Helper()

	processor := NewMockProcessor(gomock.NewController(t))
	processor.EXPECT().Enabled(gomock.Any(), gomock.Any()).Return(true).AnyTimes()
	processor.EXPECT().OnEmit(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	processor.EXPECT().ForceFlush(gomock.Any()).Return(err).AnyTimes()
	processor.EXPECT().Shutdown(gomock.Any()).Return(err).AnyTimes()

	return processor
}

// slicesClone copies a slice of span stubs so that a caller iterating one is
// not racing the exporter appending to another.
func slicesClone(in tracetest.SpanStubs) tracetest.SpanStubs {
	out := make(tracetest.SpanStubs, len(in))
	copy(out, in)
	return out
}
