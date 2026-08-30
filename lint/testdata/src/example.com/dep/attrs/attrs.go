// Package attrs is a clean fixture outside the configured module prefix: a
// dependency writing a literal that goga/semconv happens to declare a constant
// for. goga's conventions are not a dependency's to keep, so the rule must stay
// silent here — the case a rule scoped by directory rather than by import path
// would get wrong.
package attrs

import "go.opentelemetry.io/otel/attribute"

// Emit builds an attribute the way this dependency chooses to.
func Emit() attribute.KeyValue { return attribute.String("service.name", "checkout") }
