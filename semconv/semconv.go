// Package semconv is goga's semantic-convention registry: every attribute key,
// metric name and unit that goga's own telemetry emits is declared here, and
// nowhere else.
//
// The rule the package exists to enforce is that no goga module ever writes an
// attribute key or a metric name as a string literal at the point of use. A
// literal is invisible to review, ungreppable once it is misspelled, and
// impossible to rename across fifteen modules. A constant is none of those
// things, and a constant in one package is also the list a reader can consult
// to learn what goga emits.
//
// Two kinds of declaration live here. The OpenTelemetry-standard keys —
// service.name, service.version, error.type — are aliases of the upstream
// registry in go.opentelemetry.io/otel/semconv, so that goga cannot drift from
// the specification by retyping them. The goga.* keys are goga's own, and are
// declared directly.
//
// # Provenance
//
// This package is HAND-WRITTEN, pending M12.
//
// The design calls for it to be generated from an OpenTelemetry Weaver
// registry, and the //go:generate line below records the invocation that would
// do it. Weaver ships as a Rust binary rather than a Go module — the Go module
// github.com/open-telemetry/weaver has no cmd/weaver package, so it cannot be a
// tool directive in go.mod — and it is not installed on the machine this
// package was written on. Hand-writing the constants keeps the rule that
// matters (no string literals at the point of use) true today, and leaves
// exactly one file to replace when the generators land in M12.
//
// The registry the generator would read lives at semconv/registry/. It does not
// exist yet either; creating it is M12's job, not this file's.
package semconv

//go:generate weaver registry generate --registry ./registry --templates ./templates go .

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	otelsemconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// SchemaURL is the OpenTelemetry schema the keys in this package belong to. It
// is attached to every instrumentation scope goga opens, so that a backend can
// apply the upstream schema transformations to goga's telemetry.
const SchemaURL = otelsemconv.SchemaURL

// The OpenTelemetry-standard keys goga emits. They are aliases of the upstream
// registry rather than copies of its strings: an alias cannot be misspelled,
// and it moves with the semconv version this package pins.
const (
	// ServiceNameKey is service.name — the logical name of the service, set on
	// the resource by [github.com/gaarutyunov/goga/telemetry.WithServiceName].
	ServiceNameKey = otelsemconv.ServiceNameKey
	// ServiceVersionKey is service.version — the version of the service,
	// set by [github.com/gaarutyunov/goga/telemetry.WithServiceVersion].
	ServiceVersionKey = otelsemconv.ServiceVersionKey
	// ErrorTypeKey is error.type — the type of the error that ended an
	// operation. goga sets it from the concrete Go type of the error.
	ErrorTypeKey = otelsemconv.ErrorTypeKey
)

// goga's own keys. They are namespaced under goga. so that they cannot collide
// with an application's attributes or with a future OpenTelemetry convention.
const (
	// ModuleKey is goga.module — the goga module that performed the operation,
	// as passed to [github.com/gaarutyunov/goga/telemetry.For]: "serve",
	// "database", "migrate".
	ModuleKey = attribute.Key("goga.module")
	// OperationKey is goga.operation — the operation within that module, as
	// passed to Start: "serve.Shutdown", "migrate.Up".
	OperationKey = attribute.Key("goga.operation")
	// ConfigSourcesKey is goga.config.sources — the configuration sources
	// [github.com/gaarutyunov/goga/config.Load] actually merged, as an ordered
	// string slice: "defaults", "file:<path>", "env:<prefix>", "flags".
	//
	// # The order is the house order, not the option order
	//
	// goga/config merges sources in one fixed sequence — defaults, then files,
	// then the environment, then flags — with each beating the ones before it.
	// That sequence is written into Load. It is NOT the order the WithFile,
	// WithEnv and WithFlags options were passed, it is not configurable, and
	// no option can change it; passing WithEnv before WithFile is the same
	// program as passing them the other way round.
	//
	// So this slice reports precedence, lowest first, filtered to the sources
	// that actually contributed — an optional file that was not there is
	// absent from it. A reader comparing two loads is comparing which sources
	// were present, never which order somebody wrote the options in.
	//
	// That is the value's whole point. A configuration bug is almost never
	// "the value is wrong" and almost always "a different source won than the
	// one the operator edited", and that question is unanswerable from the
	// resulting value alone. Recording the list on the load span answers it
	// without the operator having to reproduce the environment.
	ConfigSourcesKey = attribute.Key("goga.config.sources")

	// MigrationVersionKey is goga.migration.version — the version of the one
	// migration a goga/migrate span covers, as an integer:
	// 20260714120000. It is set on the per-migration span
	// [github.com/gaarutyunov/goga/migrate.Migrator.Up] opens, never on the
	// span covering the run as a whole.
	//
	// The version rather than only the file name, because the version is what
	// the version table records and what a rollback names. A backend joining
	// goga's spans to the schema history joins on this.
	MigrationVersionKey = attribute.Key("goga.migration.version")

	// MigrationNameKey is goga.migration.name — the migration file that
	// version came from, as goose reports its source path:
	// "20260714120000_add_index.sql".
	//
	// It sits beside [MigrationVersionKey] rather than replacing it because
	// the two answer different questions. The version identifies the row; the
	// name is what a reader recognises, and it is the half that makes a span
	// list readable without a lookup — which is the whole point of a span per
	// migration. Finding the migration that takes forty seconds means reading
	// a name off a trace, not correlating an integer against a directory.
	MigrationNameKey = attribute.Key("goga.migration.name")

	// MCPToolNameKey is goga.mcp.tool.name — the name of the MCP tool a
	// goga/mcp span covers, as it was registered with
	// [github.com/gaarutyunov/goga/mcp.AddTool] and as a client names it in a
	// tools/call request.
	//
	// The tool name rather than the JSON-RPC method, because every tool call
	// arrives as the same method — "tools/call" — and an attribute that is the
	// same for every span answers nothing. The tool name is what distinguishes
	// one operation from another, and it is bounded by the number of tools the
	// server registered.
	MCPToolNameKey = attribute.Key("goga.mcp.tool.name")

	// MCPSessionIDKey is goga.mcp.session.id — the id of the MCP session the
	// request arrived on, as the SDK reports it.
	//
	// An MCP client holds one session across many calls, so this is the
	// attribute that groups a conversation's spans together when the trace
	// context did not survive the hop — which, for a client that does not send
	// goga's traceparent in _meta, is every call.
	MCPSessionIDKey = attribute.Key("goga.mcp.session.id")

	// MCPResourceURIKey is goga.mcp.resource.uri — the URI of the resource a
	// resources/read span covers.
	//
	// It is the resource's registered URI, not a per-request one: goga records
	// the URI the handler was registered under, so a server with a bounded set
	// of resources has a bounded set of values here.
	MCPResourceURIKey = attribute.Key("goga.mcp.resource.uri")

	// MCPPromptNameKey is goga.mcp.prompt.name — the name of the prompt a
	// prompts/get span covers. The prompt's arguments are deliberately not
	// recorded: they are caller-supplied text, unbounded in cardinality and
	// routinely the part that should not be stored.
	MCPPromptNameKey = attribute.Key("goga.mcp.prompt.name")

	// MCPTransportKey is goga.mcp.transport — the plain adapter name the MCP
	// server resolved its transport under: "stdio", "http", "sse".
	//
	// It is set on the resolve span rather than on every call. "Which transport
	// did this process actually resolve" is a startup question asked once, and
	// answering it on every tool call would multiply the cardinality of the
	// series that matter without adding an answer.
	MCPTransportKey = attribute.Key("goga.mcp.transport")
)

// The instruments goga's own telemetry records. One histogram and one counter
// cover every goga operation, distinguished by [ModuleKey] and [OperationKey],
// rather than one instrument per module: a fixed instrument set is what lets a
// dashboard be written once and work for every module a project adopts.
const (
	// OperationDurationName is the name of the histogram recording how long a
	// goga operation took.
	OperationDurationName = "goga.operation.duration"
	// OperationDurationUnit is seconds, per the OpenTelemetry convention that
	// durations are recorded in seconds and not milliseconds.
	OperationDurationUnit = "s"
	// OperationDurationDescription documents the histogram for a backend that
	// surfaces instrument descriptions.
	OperationDurationDescription = "Duration of a goga module operation."

	// OperationErrorsName is the name of the counter incremented once per
	// failed goga operation.
	OperationErrorsName = "goga.operation.errors"
	// OperationErrorsUnit is the UCUM annotation for a dimensionless count of
	// errors.
	OperationErrorsUnit = "{error}"
	// OperationErrorsDescription documents the counter for a backend that
	// surfaces instrument descriptions.
	OperationErrorsDescription = "Number of goga module operations that failed."
)

// ServiceName returns the service.name attribute.
func ServiceName(name string) attribute.KeyValue { return ServiceNameKey.String(name) }

// ServiceVersion returns the service.version attribute.
func ServiceVersion(version string) attribute.KeyValue { return ServiceVersionKey.String(version) }

// Module returns the goga.module attribute.
func Module(module string) attribute.KeyValue { return ModuleKey.String(module) }

// Operation returns the goga.operation attribute.
func Operation(op string) attribute.KeyValue { return OperationKey.String(op) }

// ConfigSources returns the goga.config.sources attribute, carrying the
// configuration sources in the fixed house order they were merged in —
// lowest precedence first. See [ConfigSourcesKey]: the order is goga's, not
// the caller's.
func ConfigSources(sources []string) attribute.KeyValue {
	return ConfigSourcesKey.StringSlice(sources)
}

// MigrationVersion returns the goga.migration.version attribute.
//
// The value is an integer and not a string: a version is ordered, and a
// backend that receives it as a string cannot sort or range over it.
func MigrationVersion(version int64) attribute.KeyValue {
	return MigrationVersionKey.Int64(version)
}

// MigrationName returns the goga.migration.name attribute, carrying the
// migration's source path as goose reports it.
func MigrationName(name string) attribute.KeyValue { return MigrationNameKey.String(name) }

// MCPToolName returns the goga.mcp.tool.name attribute.
func MCPToolName(name string) attribute.KeyValue { return MCPToolNameKey.String(name) }

// MCPSessionID returns the goga.mcp.session.id attribute.
func MCPSessionID(id string) attribute.KeyValue { return MCPSessionIDKey.String(id) }

// MCPResourceURI returns the goga.mcp.resource.uri attribute.
func MCPResourceURI(uri string) attribute.KeyValue { return MCPResourceURIKey.String(uri) }

// MCPPromptName returns the goga.mcp.prompt.name attribute.
func MCPPromptName(name string) attribute.KeyValue { return MCPPromptNameKey.String(name) }

// MCPTransport returns the goga.mcp.transport attribute.
func MCPTransport(name string) attribute.KeyValue { return MCPTransportKey.String(name) }

// ErrorType returns the error.type attribute for err, carrying err's concrete
// Go type — "*fs.PathError", "*database.UnknownSchemeError".
//
// The type is used rather than err.Error() deliberately: the message of an
// error routinely embeds a DSN, a row count or a request id, and an attribute
// whose value varies per occurrence multiplies the cardinality of every time
// series it is attached to. The type is the part that identifies the failure
// mode, and it is bounded by the number of error types in the program.
//
// ErrorType(nil) returns error.type="_OTHER", the value the OpenTelemetry
// convention reserves for an error whose type could not be determined. Callers
// record the attribute only on the failure path, so that case should not arise.
func ErrorType(err error) attribute.KeyValue {
	if err == nil {
		return ErrorTypeKey.String("_OTHER")
	}
	return ErrorTypeKey.String(fmt.Sprintf("%T", err))
}
