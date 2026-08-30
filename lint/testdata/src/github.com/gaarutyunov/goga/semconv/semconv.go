// Package semconv is a fixture stub of goga/semconv, and simultaneously the
// fixture for the one package gogasemconv must never report: the registry
// itself necessarily writes the literals every other package is forbidden to
// write. It carries no `want` comment, so analysistest fails if the rule ever
// starts firing here.
package semconv

import "go.opentelemetry.io/otel/attribute"

// ServiceNameKey is service.name.
const ServiceNameKey = attribute.Key("service.name")

// ModuleKey is goga.module.
const ModuleKey = attribute.Key("goga.module")

// OperationKey is goga.operation.
const OperationKey = attribute.Key("goga.operation")

// ServiceName returns the service.name attribute.
func ServiceName(name string) attribute.KeyValue { return ServiceNameKey.String(name) }

// Module returns the goga.module attribute.
func Module(module string) attribute.KeyValue { return ModuleKey.String(module) }
