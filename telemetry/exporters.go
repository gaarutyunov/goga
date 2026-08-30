package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/gaarutyunov/goga/registry"
)

// The three signals an exporter can be registered for. They are part of the
// registry key, so that one name — "memory", "kafka" — can be registered
// independently for each signal in the same registry.
const (
	signalTrace  = "trace"
	signalMetric = "metric"
	signalLog    = "log"
)

// The standard exporter names, matching the values the OpenTelemetry
// specification defines for OTEL_<SIGNAL>_EXPORTER.
const (
	exporterOTLP    = "otlp"
	exporterConsole = "console"
	exporterNone    = "none"
)

// standardExporters is the set of names goga resolves without a registry. It is
// the same list for all three signals.
var standardExporters = []string{exporterOTLP, exporterConsole, exporterNone}

// The OTLP transport-protocol environment variables, read exactly as
// autoexport reads them so that an explicitly named "otlp" exporter and an
// environment-selected one behave identically.
const (
	envOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envOTLPTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envOTLPMetricProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envOTLPLogsProtocol   = "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"

	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

// ErrUnknownExporter reports an exporter name that is neither registered nor
// standard. A caller branches on it with errors.Is, and reaches for
// [UnknownExporterError] with errors.As when it wants the list of names that
// were available.
var ErrUnknownExporter = errors.New("goga/telemetry: unknown exporter name")

// UnknownExporterError is returned by [Setup] when a configured exporter name
// resolves to nothing.
//
// It is an error and not a fallback to a no-op exporter on purpose. A typo in
// OTEL_TRACES_EXPORTER or in a config file would otherwise disable telemetry
// silently, and the whole point of a framework that instruments by default is
// that turning instrumentation off has to be a decision somebody made on
// purpose rather than a mistake nobody noticed.
type UnknownExporterError struct {
	// Signal is "trace", "metric" or "log".
	Signal string
	// Name is the name that was asked for.
	Name string
	// Supported holds every name that would have worked, sorted: the standard
	// names plus whatever was registered for this signal in the injected
	// registry.
	Supported []string
}

func (e *UnknownExporterError) Error() string {
	return fmt.Sprintf("goga/telemetry: no %s exporter named %q (supported: %s)",
		e.Signal, e.Name, strings.Join(e.Supported, ", "))
}

// Is reports whether target is [ErrUnknownExporter], so that a caller can
// branch on the condition without naming this type.
func (e *UnknownExporterError) Is(target error) bool { return target == ErrUnknownExporter }

// RegisterTraceExporter registers a span exporter constructor under name in the
// shared registry r.
//
// The constructor takes a context because an exporter dials its collector at
// construction; it is called with the context [Setup] was called with. S is
// inferred from ctor, so the adapter's settings type stays unexported.
//
// A duplicate name returns an error — the registry's behaviour — rather than
// panicking: registration happens in ordinary startup code, where an error can
// be reported.
func RegisterTraceExporter[S any](r *registry.Registry, name string, ctor func(ctx context.Context, s S) (sdktrace.SpanExporter, error)) error {
	return registerExporter[sdktrace.SpanExporter](r, signalTrace, name, ctor)
}

// RegisterMetricExporter registers a metric exporter constructor under name in
// the shared registry r.
//
// The exporter is a push exporter: goga wraps it in a periodic reader. An
// adapter needing a pull reader instead is not expressible here, which is the
// one shape this helper deliberately does not cover — see [WithPrometheus] for
// the scrape path.
func RegisterMetricExporter[S any](r *registry.Registry, name string, ctor func(ctx context.Context, s S) (sdkmetric.Exporter, error)) error {
	return registerExporter[sdkmetric.Exporter](r, signalMetric, name, ctor)
}

// RegisterLogExporter registers a log record exporter constructor under name in
// the shared registry r.
func RegisterLogExporter[S any](r *registry.Registry, name string, ctor func(ctx context.Context, s S) (sdklog.Exporter, error)) error {
	return registerExporter[sdklog.Exporter](r, signalLog, name, ctor)
}

// registerExporter is the shared body of the three helpers above. It registers
// under a signal-qualified key so that the same plain name can be used for more
// than one signal in one registry.
func registerExporter[P any, S any](r *registry.Registry, signal, name string, ctor func(context.Context, S) (P, error)) error {
	if r == nil {
		return fmt.Errorf("goga/telemetry: register %s exporter %q: registry must not be nil", signal, name)
	}
	if name == "" {
		return fmt.Errorf("goga/telemetry: register %s exporter: name must not be empty", signal)
	}
	if err := r.Register(registryKey(signal, name), ctor); err != nil {
		return fmt.Errorf("goga/telemetry: register %s exporter %q: %w", signal, name, err)
	}
	return nil
}

// registryKey qualifies an exporter name with its signal. The registry is keyed
// by name alone and shared with every other goga module, so "otlp" registered
// for traces must not collide with "otlp" registered for metrics.
func registryKey(signal, name string) string {
	return "telemetry." + signal + "." + name
}

// registeredNames returns the plain exporter names registered for one signal,
// with the key prefix stripped.
func registeredNames(r *registry.Registry, signal string) []string {
	if r == nil {
		return nil
	}
	prefix := registryKey(signal, "")
	var names []string
	for _, key := range r.Names() {
		if after, ok := strings.CutPrefix(key, prefix); ok {
			names = append(names, after)
		}
	}
	return names
}

// unknownExporter builds the error for a name that resolved to nothing,
// listing everything that would have worked.
func unknownExporter(r *registry.Registry, signal, name string) error {
	supported := append(registeredNames(r, signal), standardExporters...)
	slices.Sort(supported)
	return &UnknownExporterError{Signal: signal, Name: name, Supported: slices.Compact(supported)}
}

// openFromRegistry resolves name for one signal through the injected registry.
//
// The three results are "found", "found but broken" and "not mine": ok reports
// whether the registry claims the name at all, so that a name it does not know
// falls through to the standard table rather than being reported as a registry
// failure.
func openFromRegistry[P any](ctx context.Context, r *registry.Registry, signal, name string) (p P, ok bool, err error) {
	var zero P
	if r == nil {
		return zero, false, nil
	}
	key := registryKey(signal, name)
	if !slices.Contains(r.Names(), key) {
		return zero, false, nil
	}
	p, err = r.Open[P](ctx, key, nil)
	if err != nil {
		return zero, true, fmt.Errorf("goga/telemetry: %s exporter %q: %w", signal, name, err)
	}
	return p, true, nil
}

// otlpProtocol resolves the OTLP transport for one signal: the signal-specific
// environment variable, then the general one, then http/protobuf.
func otlpProtocol(signalEnv string) (string, error) {
	proto := os.Getenv(signalEnv)
	if proto == "" {
		proto = os.Getenv(envOTLPProtocol)
	}
	if proto == "" {
		proto = protocolHTTP
	}
	switch proto {
	case protocolGRPC, protocolHTTP:
		return proto, nil
	default:
		return "", fmt.Errorf("goga/telemetry: unsupported OTLP protocol %q (supported: %s, %s)",
			proto, protocolGRPC, protocolHTTP)
	}
}

// resolveSpanExporter returns the span exporter for s, or nil when the
// configuration asks for none.
//
// With no explicit name the whole selection is delegated to autoexport, which
// is the OpenTelemetry-specified environment-driven behaviour. With an explicit
// name the registry is consulted first, so that a house exporter is purely
// additive to the standard set and can never be shadowed by it.
func resolveSpanExporter(ctx context.Context, s settings) (sdktrace.SpanExporter, error) {
	if s.traceExporter == "" {
		exp, err := autoexport.NewSpanExporter(ctx)
		if err != nil {
			return nil, fmt.Errorf("goga/telemetry: trace exporter: %w", err)
		}
		if autoexport.IsNoneSpanExporter(exp) {
			return nil, nil
		}
		return exp, nil
	}

	if exp, ok, err := openFromRegistry[sdktrace.SpanExporter](ctx, s.exporters, signalTrace, s.traceExporter); ok {
		return exp, err
	}

	switch s.traceExporter {
	case exporterOTLP:
		proto, err := otlpProtocol(envOTLPTracesProtocol)
		if err != nil {
			return nil, err
		}
		if proto == protocolGRPC {
			return otlptracegrpc.New(ctx)
		}
		return otlptracehttp.New(ctx)
	case exporterConsole:
		return stdouttrace.New()
	case exporterNone:
		return nil, nil
	default:
		return nil, unknownExporter(s.exporters, signalTrace, s.traceExporter)
	}
}

// resolveMetricReader returns the push reader for s, or nil when the
// configuration asks for none. A registry-supplied exporter is wrapped in a
// periodic reader here, so that adapters register the exporter — the part that
// is adapter-specific — rather than the reader.
func resolveMetricReader(ctx context.Context, s settings) (sdkmetric.Reader, error) {
	if s.metricExporter == "" {
		reader, err := autoexport.NewMetricReader(ctx)
		if err != nil {
			return nil, fmt.Errorf("goga/telemetry: metric exporter: %w", err)
		}
		if autoexport.IsNoneMetricReader(reader) {
			return nil, nil
		}
		return reader, nil
	}

	if exp, ok, err := openFromRegistry[sdkmetric.Exporter](ctx, s.exporters, signalMetric, s.metricExporter); ok {
		if err != nil {
			return nil, err
		}
		return sdkmetric.NewPeriodicReader(exp), nil
	}

	switch s.metricExporter {
	case exporterOTLP:
		proto, err := otlpProtocol(envOTLPMetricProtocol)
		if err != nil {
			return nil, err
		}
		var exp sdkmetric.Exporter
		if proto == protocolGRPC {
			exp, err = otlpmetricgrpc.New(ctx)
		} else {
			exp, err = otlpmetrichttp.New(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("goga/telemetry: metric exporter: %w", err)
		}
		return sdkmetric.NewPeriodicReader(exp), nil
	case exporterConsole:
		exp, err := stdoutmetric.New()
		if err != nil {
			return nil, fmt.Errorf("goga/telemetry: metric exporter: %w", err)
		}
		return sdkmetric.NewPeriodicReader(exp), nil
	case exporterNone:
		return nil, nil
	default:
		return nil, unknownExporter(s.exporters, signalMetric, s.metricExporter)
	}
}

// resolveLogExporter returns the log record exporter for s, or nil when the
// configuration asks for none.
func resolveLogExporter(ctx context.Context, s settings) (sdklog.Exporter, error) {
	if s.logExporter == "" {
		exp, err := autoexport.NewLogExporter(ctx)
		if err != nil {
			return nil, fmt.Errorf("goga/telemetry: log exporter: %w", err)
		}
		if autoexport.IsNoneLogExporter(exp) {
			return nil, nil
		}
		return exp, nil
	}

	if exp, ok, err := openFromRegistry[sdklog.Exporter](ctx, s.exporters, signalLog, s.logExporter); ok {
		return exp, err
	}

	switch s.logExporter {
	case exporterOTLP:
		proto, err := otlpProtocol(envOTLPLogsProtocol)
		if err != nil {
			return nil, err
		}
		if proto == protocolGRPC {
			return otlploggrpc.New(ctx)
		}
		return otlploghttp.New(ctx)
	case exporterConsole:
		return stdoutlog.New()
	case exporterNone:
		return nil, nil
	default:
		return nil, unknownExporter(s.exporters, signalLog, s.logExporter)
	}
}

// newPrometheusReader builds the scrape reader that is attached by default.
//
// It registers with prometheus.DefaultRegisterer — the exporter's own default —
// rather than with an isolated registry, because that is what makes a
// promhttp.Handler mounted by the application scrape goga's metrics with no
// further wiring. The cost is that two Setups in one process register two
// collectors, whose target_info series then collide at scrape time; a process
// calling Setup twice has a larger problem than that.
func newPrometheusReader() (sdkmetric.Reader, error) {
	reader, err := promexporter.New()
	if err != nil {
		return nil, fmt.Errorf("goga/telemetry: prometheus reader: %w", err)
	}
	return reader, nil
}
