package telemetry

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/gaarutyunov/goga/semconv"
)

// instrumentations caches one handle per module name, so that repeated [For]
// calls from different constructors in the same module share a handle and its
// instruments.
var instrumentations sync.Map // map[string]*Instrumentation

// Instrumentation is one goga module's handle onto the process telemetry: a
// tracer, a meter with goga's two instruments already created on it, and a
// logger, all attributed to that module.
//
// A handle is obtained with [For] at construction time and stored on the type
// that uses it. It never holds a provider it captured: see [For].
type Instrumentation struct {
	module string

	// resolved caches the tracer, meter, logger and instruments built from one
	// set of global providers. It is invalidated — and rebuilt — the moment any
	// of those globals is replaced, which is what makes a handle taken before
	// Setup start working after it.
	resolved atomic.Pointer[resolved]
}

// resolved is one snapshot of the derived handles, tagged with the providers
// they came from.
type resolved struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	loggerProvider log.LoggerProvider

	tracer trace.Tracer
	logger *slog.Logger

	// duration and errors are nil when the meter rejected the instrument, which
	// is why every use is guarded. [For] never fails, so a bad instrument is
	// reported through otel.Handle and the rest of the handle keeps working.
	duration metric.Float64Histogram
	errors   metric.Int64Counter
}

// For returns the instrumentation handle for a goga module: "serve",
// "database", "migrate".
//
// It never fails, and it is safe to call before [Setup]. It resolves through
// OpenTelemetry's global providers on every use rather than capturing one, so a
// handle taken at construction time — which for a composition root is routinely
// before telemetry is configured — begins emitting as soon as [Setup] installs
// the real providers.
//
// Capturing instead would be the quiet failure this design exists to prevent. A
// registry, an adapter or a config loader built before Setup would hold a no-op
// tracer for the life of the process; nothing would error, no test would fail,
// and precisely the startup paths worth observing would be the ones that were
// permanently invisible.
//
// The instruments behind the handle are created lazily and cached until the
// globals change, so calling For in a constructor costs nothing and calling it
// before Setup does not accumulate instruments on the global no-op meter.
func For(module string) *Instrumentation {
	if v, ok := instrumentations.Load(module); ok {
		if i, ok := v.(*Instrumentation); ok {
			return i
		}
	}
	v, _ := instrumentations.LoadOrStore(module, &Instrumentation{module: module})
	i, ok := v.(*Instrumentation)
	if !ok {
		// Unreachable: nothing else stores into this map. Returning a fresh
		// handle rather than asserting keeps the promise that For never fails.
		return &Instrumentation{module: module}
	}
	return i
}

// Module returns the module name the handle was created for.
func (i *Instrumentation) Module() string { return i.module }

// Start opens a span for one operation and returns the context carrying it
// together with the closer that ends it.
//
// The closer, and not a span plus a start time the caller hands back, is the
// whole point of the signature. The duration belongs to the type that started
// the operation, so it cannot be mis-measured at the call site — the shape it
// replaces was already mis-called in the design that proposed it, recording a
// duration of zero for every iteration of a loop. The span itself stays
// reachable through trace.SpanFromContext(ctx) for a caller that wants to add
// attributes while the operation is still running.
//
// The closer records the span status, the goga.operation.duration histogram
// and, on failure, error.type and the goga.operation.errors counter. It is
// idempotent: calling it twice ends the span once and counts the operation
// once.
//
// The house shape at the call site is a named result and a deferred closure, so
// that the deferred call observes the error variable rather than the value some
// return expression computed:
//
//	func (s *Server) Shutdown(ctx context.Context) (err error) {
//		ctx, end := s.instr.Start(ctx, "serve.Shutdown")
//		defer func() { end(err) }()
//		…
//	}
func (i *Instrumentation) Start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	r := i.current()

	base := make([]attribute.KeyValue, 0, len(attrs)+2)
	base = append(base, semconv.Module(i.module), semconv.Operation(op))
	base = append(base, attrs...)

	ctx, span := r.tracer.Start(ctx, "goga."+i.module+"."+op, trace.WithAttributes(base...))
	start := time.Now()

	var once sync.Once
	end := func(err error) {
		once.Do(func() {
			elapsed := time.Since(start).Seconds()
			set := base
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				// A fresh slice: base is shared with the span's attributes and
				// with any other closer built from it.
				set = append(append(make([]attribute.KeyValue, 0, len(base)+1), base...), semconv.ErrorType(err))
				if r.errors != nil {
					r.errors.Add(ctx, 1, metric.WithAttributes(set...))
				}
			} else {
				span.SetStatus(codes.Ok, "")
			}
			if r.duration != nil {
				r.duration.Record(ctx, elapsed, metric.WithAttributes(set...))
			}
			span.End()
		})
	}
	return ctx, end
}

// Logger returns the module's structured logger, writing through the
// OpenTelemetry log bridge so that a record emitted inside a span carries that
// span's trace and span ids.
//
// Like the rest of the handle it resolves through the global logger provider,
// so a logger obtained before [Setup] starts emitting once [Setup] runs.
func (i *Instrumentation) Logger() *slog.Logger { return i.current().logger }

// current returns the resolved handles for the providers installed right now,
// rebuilding them if any global has been replaced since the last call.
func (i *Instrumentation) current() *resolved {
	tp := otel.GetTracerProvider()
	mp := otel.GetMeterProvider()
	lp := global.GetLoggerProvider()

	if r := i.resolved.Load(); r != nil &&
		sameProvider(r.tracerProvider, tp) &&
		sameProvider(r.meterProvider, mp) &&
		sameProvider(r.loggerProvider, lp) {
		return r
	}

	r := i.build(tp, mp, lp)
	i.resolved.Store(r)
	return r
}

// sameProvider reports whether two provider values are the same installed
// provider.
//
// It exists rather than a bare == because comparing two interface values panics
// when their shared dynamic type is not comparable, and nothing stops a caller
// from installing a provider that is a struct value with a slice field. A
// provider that cannot be compared simply never matches the cache, which costs
// a rebuild per call and stays correct.
func sameProvider(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// build derives the tracer, logger and instruments from one set of providers.
func (i *Instrumentation) build(tp trace.TracerProvider, mp metric.MeterProvider, lp log.LoggerProvider) *resolved {
	r := &resolved{
		tracerProvider: tp,
		meterProvider:  mp,
		loggerProvider: lp,
		tracer: tp.Tracer(scopeName,
			trace.WithInstrumentationVersion(scopeVersion),
			trace.WithSchemaURL(semconv.SchemaURL)),
		logger: otelslog.NewLogger(scopeName,
			otelslog.WithLoggerProvider(lp),
			otelslog.WithVersion(scopeVersion),
			otelslog.WithSchemaURL(semconv.SchemaURL),
			otelslog.WithAttributes(semconv.Module(i.module))),
	}

	meter := mp.Meter(scopeName,
		metric.WithInstrumentationVersion(scopeVersion),
		metric.WithSchemaURL(semconv.SchemaURL))

	duration, err := meter.Float64Histogram(semconv.OperationDurationName,
		metric.WithUnit(semconv.OperationDurationUnit),
		metric.WithDescription(semconv.OperationDurationDescription))
	if err != nil {
		otel.Handle(err)
	} else {
		r.duration = duration
	}

	errCount, err := meter.Int64Counter(semconv.OperationErrorsName,
		metric.WithUnit(semconv.OperationErrorsUnit),
		metric.WithDescription(semconv.OperationErrorsDescription))
	if err != nil {
		otel.Handle(err)
	} else {
		r.errors = errCount
	}

	return r
}
