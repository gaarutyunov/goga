package clean

// This file binds the identifier "attribute" to a package that is NOT
// go.opentelemetry.io/otel/attribute, and then calls String(key, value) on it.
// It is the fixture for the rule's most tempting shortcut: matching the
// selector "attribute.String" by name. Doing that would report this line. The
// rule resolves the identifier through the file's own import block instead, so
// it must stay silent — and because the binding is per-file, the real
// attribute package next door is unaffected.
import attribute "example.com/lookalike"

// NotTheAttributePackage builds a string, not an OpenTelemetry attribute.
func NotTheAttributePackage() string {
	return attribute.String("service.name", "checkout")
}
