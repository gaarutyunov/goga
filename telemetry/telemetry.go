// Package telemetry configures OpenTelemetry once, for the whole process, and
// hands every other goga module the handle it instruments itself with.
//
// There are two halves, and they are deliberately independent.
//
// [Setup] is called by the composition root exactly once. It builds a tracer
// provider, a meter provider and a logger provider — all three or none —
// installs them as the OpenTelemetry globals, and returns them so that a
// consumer needing a subset (a metrics-only exporter, a library that takes a
// provider rather than reading the global) can reach them without going through
// package-level state.
//
// [For] is called by every other goga module, at construction time, and never
// fails. It resolves through OpenTelemetry's global *delegating* providers, so
// a handle obtained before [Setup] runs starts emitting the moment [Setup]
// installs the real providers. That is the property the whole design rests on:
// a composition root routinely builds a registry, an adapter or a config loader
// before telemetry exists, and a handle that had snapshotted the no-op provider
// at construction would leave exactly those paths permanently unobserved while
// every test still passed.
//
// The consequence for a library author is that a goga module can be used
// without configuring telemetry at all. Before [Setup] the globals are no-ops
// and the module is silent; the telemetry appears when the consuming binary
// decides to call [Setup].
//
// # Exporter selection
//
// With no explicit exporter name, each signal is resolved by
// [go.opentelemetry.io/contrib/exporters/autoexport], which reads the standard
// OTEL_TRACES_EXPORTER / OTEL_METRICS_EXPORTER / OTEL_LOGS_EXPORTER environment
// variables. With an explicit name — [WithTraceExporter] and its siblings — the
// name is looked up first in the [github.com/gaarutyunov/goga/registry] the
// caller injected with [WithExporterRegistry], then among the standard names
// "otlp", "console" and "none". A name in neither place fails [Setup] with an
// [UnknownExporterError] naming what was available; telemetry is never silently
// switched off.
//
// # Prometheus
//
// A Prometheus reader is attached by default, registered with
// prometheus.DefaultRegisterer so that a promhttp.Handler mounted by the
// application scrapes goga's metrics with no further wiring. A push exporter
// configured with [WithMetricExporter] is additive: both readers feed the same
// meter provider.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/semconv"
)

// scopeName is the instrumentation scope every goga span, metric and log record
// is attributed to. It is goga's module path because that is what identifies
// the instrumenting library to a backend; the goga module that performed the
// operation travels as the semconv.ModuleKey attribute instead, so that one
// scope covers the whole framework.
const scopeName = "github.com/gaarutyunov/goga"

// scopeVersion is the instrumentation version reported alongside scopeName.
//
// It is a constant rather than a runtime/debug.ReadBuildInfo lookup: build info
// is absent in a test binary and in a program built with -buildvcs=false, and a
// scope whose version disappears under test is worse than one that is slightly
// stale.
const scopeVersion = "0.1.0"

// moduleName is the goga module this package reports itself as. telemetry is
// itself instrumented — see [Telemetry.Shutdown].
const moduleName = "telemetry"

// defaultShutdownTimeout bounds the cleanup returned by [Setup]. Five seconds
// is long enough for a batch processor to drain to a local collector and short
// enough that a wedged exporter cannot hold a process open past a container
// runtime's kill deadline.
const defaultShutdownTimeout = 5 * time.Second

// settings is unexported so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
type settings struct {
	serviceName     string
	serviceVersion  string
	resourceAttrs   []attribute.KeyValue
	traceExporter   string
	metricExporter  string
	logExporter     string
	prometheus      bool
	propagators     []string
	shutdownTimeout time.Duration
	exporters       *registry.Registry
}

// defaults returns the settings a [Setup] with no options runs with.
func defaults() settings {
	return settings{
		prometheus:      true,
		shutdownTimeout: defaultShutdownTimeout,
	}
}

// Option configures [Setup]. It is an alias over an unexported settings type,
// so a caller can hold and pass a telemetry.Option and cannot write the struct
// it mutates.
type Option = goga.Option[settings]

// WithServiceName sets the service.name resource attribute.
//
// It returns an error for an empty name rather than accepting one, because an
// unnamed service is indistinguishable in a backend from every other unnamed
// service. Leaving the option off entirely is different and is allowed: the
// resource then falls back to OTEL_SERVICE_NAME, or to the OpenTelemetry SDK's
// "unknown_service" default.
func WithServiceName(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return errors.New("goga/telemetry: service name must not be empty")
		}
		s.serviceName = name
		return nil
	}
}

// WithServiceVersion sets the service.version resource attribute.
func WithServiceVersion(version string) Option {
	return func(s *settings) error {
		if version == "" {
			return errors.New("goga/telemetry: service version must not be empty")
		}
		s.serviceVersion = version
		return nil
	}
}

// WithResourceAttributes appends attributes to the resource shared by all three
// signals. Repeated calls accumulate.
//
// Keys come from [github.com/gaarutyunov/goga/semconv] or from the upstream
// OpenTelemetry registry; goga never writes an attribute key as a literal.
func WithResourceAttributes(kv ...attribute.KeyValue) Option {
	return func(s *settings) error {
		for _, a := range kv {
			if !a.Valid() {
				return fmt.Errorf("goga/telemetry: resource attribute %q is not valid", a.Key)
			}
		}
		s.resourceAttrs = append(s.resourceAttrs, kv...)
		return nil
	}
}

// WithTraceExporter selects the span exporter by name.
//
// The name is resolved against the injected exporter registry first and the
// standard names ("otlp", "console", "none") second. Leaving the option off
// resolves the exporter from OTEL_TRACES_EXPORTER instead.
func WithTraceExporter(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return errors.New("goga/telemetry: trace exporter name must not be empty")
		}
		s.traceExporter = name
		return nil
	}
}

// WithMetricExporter selects the metric exporter by name.
//
// It is additive to the Prometheus reader, which is on by default: a program
// configured with both scrapes and pushes. "prometheus" is not one of this
// option's standard names — use [WithPrometheus] — though the environment-driven
// path does honour OTEL_METRICS_EXPORTER=prometheus through autoexport.
func WithMetricExporter(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return errors.New("goga/telemetry: metric exporter name must not be empty")
		}
		s.metricExporter = name
		return nil
	}
}

// WithLogExporter selects the log record exporter by name.
func WithLogExporter(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return errors.New("goga/telemetry: log exporter name must not be empty")
		}
		s.logExporter = name
		return nil
	}
}

// WithPrometheus turns the default Prometheus reader on or off.
//
// It is on by default because a scrape endpoint is the one metrics transport
// that needs no collector deployed to be useful, and a framework whose metrics
// are invisible until a collector exists is a framework whose metrics are
// invisible.
func WithPrometheus(enabled bool) Option {
	return func(s *settings) error {
		s.prometheus = enabled
		return nil
	}
}

// WithShutdownTimeout bounds the cleanup function [Setup] returns.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf("goga/telemetry: shutdown timeout must be > 0, got %s", d)
		}
		s.shutdownTimeout = d
		return nil
	}
}

// WithPropagators sets the context propagators by name, delegating to
// [go.opentelemetry.io/contrib/propagators/autoprop]: "tracecontext",
// "baggage", "b3", "b3multi", "jaeger", "xray", "ottrace", "none".
//
// Leaving the option off resolves the propagators from OTEL_PROPAGATORS, which
// defaults to tracecontext plus baggage.
func WithPropagators(names ...string) Option {
	return func(s *settings) error {
		if len(names) == 0 {
			return errors.New("goga/telemetry: at least one propagator name is required")
		}
		for _, n := range names {
			if n == "" {
				return errors.New("goga/telemetry: propagator name must not be empty")
			}
		}
		s.propagators = append(s.propagators, names...)
		return nil
	}
}

// WithExporterRegistry injects the registry that house and custom exporter
// names are resolved through.
//
// The registry is injected rather than defaulted to a package-level one on
// purpose. A package-level registry is process-global mutable state: two tests
// in one binary, or two independently wired subsystems in one process, would
// share it and collide on a name, and there would be no way to construct a
// program that did not have it. Registrations go through
// [RegisterTraceExporter] and its siblings.
func WithExporterRegistry(r *registry.Registry) Option {
	return func(s *settings) error {
		if r == nil {
			return errors.New("goga/telemetry: exporter registry must not be nil")
		}
		s.exporters = r
		return nil
	}
}

// Telemetry is the configured telemetry of a process: the three handles a
// module uses directly, and the three providers behind them.
//
// The providers are exported as well as installed globally for two reasons that
// have both already come up. A consumer may need a subset — a metrics-only
// deployment reads MeterProvider and ignores the rest — and a third-party
// library conventionally takes a provider as a constructor argument rather than
// reading the global, so a caller has to be able to name one.
type Telemetry struct {
	// Tracer is the tracer for goga's own instrumentation scope.
	Tracer trace.Tracer
	// Meter is the meter for goga's own instrumentation scope.
	Meter metric.Meter
	// Logger writes through the OpenTelemetry log bridge, so a log record
	// emitted inside a span carries that span's trace and span ids.
	Logger *slog.Logger

	// TracerProvider is the installed tracer provider.
	TracerProvider *sdktrace.TracerProvider
	// MeterProvider is the installed meter provider.
	MeterProvider *sdkmetric.MeterProvider
	// LoggerProvider is the installed logger provider.
	LoggerProvider *sdklog.LoggerProvider
}

// Setup builds the three providers, installs them as the OpenTelemetry globals
// and returns them.
//
// All three or none: if any provider cannot be built, the ones already built
// are shut down, nothing is installed, and the error is returned. A process
// therefore never runs with traces but no metrics, which is the failure mode
// this signature exists to make unreachable.
//
// The second result is the cleanup, and its type is func() rather than
// func(context.Context) error because func() is the only shape
// github.com/goforj/wire recognises as a provider's cleanup. It calls
// [Telemetry.Shutdown] under the configured shutdown timeout, on a context
// derived from context.Background(): by the time cleanup runs the request
// context that reached Setup is long cancelled, and flushing telemetry through
// a cancelled context exports nothing.
//
// ctx is the context the exporters are constructed with. An exporter dials at
// construction, so cancelling ctx aborts a Setup that is waiting on a
// collector; it does not bound the lifetime of the providers that result.
func Setup(ctx context.Context, opts ...Option) (t *Telemetry, cleanup func(), err error) {
	s, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("goga/telemetry: setup: %w", err)
	}

	res, err := newResource(s)
	if err != nil {
		return nil, nil, err
	}

	// Built in order, and unwound in reverse on the first failure, so that a
	// partial Setup leaves no exporter running and no global installed.
	tp, err := newTracerProvider(ctx, s, res)
	if err != nil {
		return nil, nil, err
	}
	mp, err := newMeterProvider(ctx, s, res)
	if err != nil {
		return nil, nil, errors.Join(err, shutdownQuietly(ctx, tp.Shutdown))
	}
	lp, err := newLoggerProvider(ctx, s, res)
	if err != nil {
		return nil, nil, errors.Join(err, shutdownQuietly(ctx, mp.Shutdown), shutdownQuietly(ctx, tp.Shutdown))
	}
	prop, err := newPropagator(s)
	if err != nil {
		return nil, nil, errors.Join(err, shutdownQuietly(ctx, lp.Shutdown),
			shutdownQuietly(ctx, mp.Shutdown), shutdownQuietly(ctx, tp.Shutdown))
	}

	// Nothing above this line touches global state; nothing below it can fail.
	// SetMeterProvider is not conditional on anything: there is no path through
	// Setup that installs a tracer without also installing a meter.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(prop)

	logger := otelslog.NewLogger(scopeName,
		otelslog.WithLoggerProvider(lp),
		otelslog.WithVersion(scopeVersion),
		otelslog.WithSchemaURL(semconv.SchemaURL),
	)
	slog.SetDefault(logger)

	t = &Telemetry{
		Tracer:         tp.Tracer(scopeName, trace.WithInstrumentationVersion(scopeVersion), trace.WithSchemaURL(semconv.SchemaURL)),
		Meter:          mp.Meter(scopeName, metric.WithInstrumentationVersion(scopeVersion), metric.WithSchemaURL(semconv.SchemaURL)),
		Logger:         logger,
		TracerProvider: tp,
		MeterProvider:  mp,
		LoggerProvider: lp,
	}

	// contextcheck sees a context.Background() reachable from a function that
	// has a ctx and objects. It is wrong here, and the alternatives are worse:
	// deriving the cleanup context from ctx would make the flush inherit the
	// cancellation of a context that is guaranteed to be dead by the time
	// cleanup runs, and context.WithoutCancel would keep the request-scoped
	// values — including a span context — and file the process's shutdown under
	// whatever trace happened to call Setup.
	return t, newCleanup(t, s.shutdownTimeout), nil //nolint:contextcheck // the cleanup outlives ctx by construction; see newCleanup.
}

// newCleanup builds the func() [Setup] returns.
//
// It is a function of its own rather than a closure inside Setup so that the
// context it derives is visibly unrelated to the one Setup was called with —
// which is the correct behaviour, and not something to be silenced with a lint
// directive. By the time cleanup runs, the context that reached Setup is long
// cancelled, and flushing telemetry through a cancelled context exports
// nothing.
func newCleanup(t *Telemetry, timeout time.Duration) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := t.Shutdown(ctx); err != nil {
			otel.Handle(err)
		}
	}
}

// Shutdown flushes and stops every provider, in order, and joins the errors.
//
// The errors are joined rather than returned first-wins because a failure in
// the first provider would otherwise hide a failure in the two behind it, and
// shutdown is the one moment where a dropped batch is a permanent loss of data
// rather than a delay. A caller can still branch on any individual failure with
// errors.Is or errors.As.
//
// The order is logs, then metrics, then traces. Traces are last, and the span
// this method opens ends before its own provider is stopped, so that the
// shutdown is itself observable — a tracer provider cannot record the sequence
// that ends with it being shut down. The consequence is that a failure in the
// tracer provider's own flush or shutdown is reported to the caller but is not
// in the span: by then there is nothing left to record it.
//
// Shutdown is safe to call more than once; the SDK providers are idempotent.
func (t *Telemetry) Shutdown(ctx context.Context) (err error) {
	ctx, end := For(moduleName).Start(ctx, "Shutdown")

	var errs []error
	collect := func(steps ...func(context.Context) error) {
		for _, step := range steps {
			if e := step(ctx); e != nil {
				errs = append(errs, e)
			}
		}
	}

	if t.LoggerProvider != nil {
		collect(t.LoggerProvider.ForceFlush, t.LoggerProvider.Shutdown)
	}
	if t.MeterProvider != nil {
		collect(t.MeterProvider.ForceFlush, t.MeterProvider.Shutdown)
	}

	err = joinShutdown(errs)
	end(err)

	if t.TracerProvider != nil {
		collect(t.TracerProvider.ForceFlush, t.TracerProvider.Shutdown)
	}
	return joinShutdown(errs)
}

// joinShutdown wraps the accumulated failures in the house error shape, or
// returns nil when there were none.
func joinShutdown(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("goga/telemetry: shutdown: %w", errors.Join(errs...))
}

// shutdownQuietly runs a provider's Shutdown while Setup is unwinding a partial
// build.
//
// It strips the caller's cancellation with context.WithoutCancel, because that
// cancellation may be the very reason the build failed, and a shutdown on an
// already-cancelled context leaks the exporter's goroutines instead of stopping
// them. Everything else about the context — its values, and so its trace
// context — is kept.
func shutdownQuietly(ctx context.Context, shutdown func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		return fmt.Errorf("goga/telemetry: unwinding partial setup: %w", err)
	}
	return nil
}

// newResource merges goga's attributes over the SDK default resource, which
// already carries the telemetry SDK attributes and anything in
// OTEL_RESOURCE_ATTRIBUTES.
//
// The goga half is built schemaless on purpose: resource.Merge reports an error
// when two resources carry different schema URLs, and a schemaless resource
// merges into any of them. The keys themselves still come from
// [github.com/gaarutyunov/goga/semconv].
func newResource(s settings) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, len(s.resourceAttrs)+2)
	if s.serviceName != "" {
		attrs = append(attrs, semconv.ServiceName(s.serviceName))
	}
	if s.serviceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(s.serviceVersion))
	}
	attrs = append(attrs, s.resourceAttrs...)

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, fmt.Errorf("goga/telemetry: build resource: %w", err)
	}
	return res, nil
}

// newTracerProvider builds the tracer provider. A "none" exporter yields a
// provider with no processor rather than one batching into a discard exporter:
// the batcher's goroutine and its five-second timer are pure cost.
func newTracerProvider(ctx context.Context, s settings, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	exp, err := resolveSpanExporter(ctx, s)
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	return sdktrace.NewTracerProvider(opts...), nil
}

// newMeterProvider builds the meter provider from up to two readers: the
// configured push exporter, and the Prometheus scrape reader that is on by
// default. Either may be absent; a provider with no reader at all is still
// built and still installed, because [Setup] promises that
// otel.SetMeterProvider is always called.
func newMeterProvider(ctx context.Context, s settings, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	reader, err := resolveMetricReader(ctx, s)
	if err != nil {
		return nil, err
	}
	if reader != nil {
		opts = append(opts, sdkmetric.WithReader(reader))
	}

	if s.prometheus {
		promReader, err := newPrometheusReader()
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdkmetric.WithReader(promReader))
	}

	return sdkmetric.NewMeterProvider(opts...), nil
}

// newLoggerProvider builds the logger provider.
func newLoggerProvider(ctx context.Context, s settings, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	exp, err := resolveLogExporter(ctx, s)
	if err != nil {
		return nil, err
	}
	opts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	if exp != nil {
		opts = append(opts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	}
	return sdklog.NewLoggerProvider(opts...), nil
}

// newPropagator resolves the context propagators, by name when the caller named
// them and from OTEL_PROPAGATORS otherwise. Both paths go through autoprop, so
// goga has no propagator table of its own to drift from the specification.
func newPropagator(s settings) (propagation.TextMapPropagator, error) {
	if len(s.propagators) == 0 {
		return autoprop.NewTextMapPropagator(), nil
	}
	p, err := autoprop.TextMapPropagator(s.propagators...)
	if err != nil {
		return nil, fmt.Errorf("goga/telemetry: propagators: %w", err)
	}
	return p, nil
}
