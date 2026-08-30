// Package clean is the fixture that has to stay silent. Every declaration in it
// is a near miss: something the rule could plausibly fire on and must not,
// because there is either no defect or no correct alternative to suggest.
//
// analysistest fails on an unexpected diagnostic as well as on a missing one,
// so this file is what proves gogasemconv is a rule rather than a blanket ban
// on string literals near telemetry.
package clean

import (
	"go.opentelemetry.io/otel/attribute"

	"github.com/gaarutyunov/goga/semconv"
)

// Correct is the shape the rule is asking for: the constant, and the helper.
func Correct() []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.ServiceName("checkout"),
		semconv.Module("serve"),
		semconv.OperationKey.String("serve.Shutdown"),
	}
}

// Uncovered uses keys goga/semconv does not declare. There is no constant to
// suggest, so reporting these would be noise the reader cannot act on.
func Uncovered() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("http.request.method", "GET"),
		attribute.Int64("http.response.status_code", 200),
		attribute.Key("db.system.name").String("postgresql"),
	}
}

// SubstringsAreNotKeys is the fixture that catches a rule matching loosely
// instead of exactly, in both directions: a key that CONTAINS one semconv
// declares, and a key that IS CONTAINED IN one. Neither is the key semconv
// covers, so neither has a constant to suggest — and a rule that reported them
// would be telling the reader to replace "goga.module.version" with
// semconv.Module, which is a different attribute.
func SubstringsAreNotKeys() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("goga.module.version", "1.4.0"),
		attribute.String("service.name.prefix", "checkout"),
		attribute.String("name", "checkout"),
		attribute.String("error", "boom"),
	}
}

// ValuesAreNotKeys pins the reason attributeKeyArg is a map of positions rather
// than a set of function names: only the first argument is a key, and a VALUE
// that happens to read like one is not a violation.
func ValuesAreNotKeys() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("http.route", "service.name"),
		attribute.Key("http.route").String("goga.module"),
	}
}

// ComputedKey passes a key that is not a literal at the call site. It is
// already a named constant, which is what the rule wants; there is nothing to
// report even though the string it resolves to is one semconv covers.
func ComputedKey(key string) attribute.KeyValue {
	const moduleKey = "goga.module"
	return attribute.String(moduleKey, "serve")
}
