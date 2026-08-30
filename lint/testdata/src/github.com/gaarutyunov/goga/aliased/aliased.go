// Package aliased is a violating fixture that imports the attribute package
// under an alias. The rule matches the import PATH and then whatever identifier
// the file bound it to, so an alias must not be an escape hatch.
package aliased

import attrs "go.opentelemetry.io/otel/attribute"

// Emit builds one attribute through the aliased package name.
func Emit() attrs.KeyValue {
	return attrs.String("service.name", "checkout") // want `attribute key "service.name" .* use semconv\.ServiceName\(…\) instead`
}
