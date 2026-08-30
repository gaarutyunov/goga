// Package emitter is the violating fixture: goga's own code building attributes
// from string literals that goga/semconv already declares constants for.
package emitter

import (
	"go.opentelemetry.io/otel/attribute"
)

// Resource writes every key semconv covers as a literal, once per constructor
// shape the rule recognises.
func Resource() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("service.name", "checkout"),             // want `attribute key "service.name" has a constant in goga/semconv; use semconv\.ServiceName\(…\) instead of a string literal`
		attribute.String("service.version", "1.4.0"),             // want `attribute key "service.version" .* use semconv\.ServiceVersion\(…\) instead`
		attribute.String("error.type", "\\*fs.PathError"),        // want `attribute key "error.type" .* use semconv\.ErrorType\(…\) instead`
		attribute.String("goga.module", "serve"),                 // want `attribute key "goga.module" .* use semconv\.Module\(…\) instead`
		attribute.Key("goga.operation").String("serve.Shutdown"), // want `attribute key "goga.operation" .* use semconv\.Operation\(…\) instead`
	}
}

// Typed proves the rule is about the key argument and not about the attribute's
// Go type: a non-string constructor is just as much a violation.
func Typed() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Int64("goga.module", 1),  // want `attribute key "goga.module" .* use semconv\.Module\(…\) instead`
		attribute.Bool("error.type", true), // want `attribute key "error.type" .* use semconv\.ErrorType\(…\) instead`
	}
}
