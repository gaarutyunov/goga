// Package attribute is a fixture stub of go.opentelemetry.io/otel/attribute.
//
// analysistest loads its fixtures in GOPATH mode with GO111MODULE=off, so an
// import inside testdata/src can only resolve to another package inside
// testdata/src — the real module graph is not available to it. This stub
// exists so the fixtures can spell the import path the analyzer matches on and
// still type-check; it declares only the surface the fixtures call, and it is
// not a model of upstream.
package attribute

// Key is an attribute key.
type Key string

// String returns the KeyValue pairing k with a string value.
func (k Key) String(v string) KeyValue { return KeyValue{Key: k, Value: v} }

// KeyValue is a key and its value.
type KeyValue struct {
	Key   Key
	Value any
}

// String returns the KeyValue for a string-valued attribute.
func String(k, v string) KeyValue { return KeyValue{Key: Key(k), Value: v} }

// Int64 returns the KeyValue for an int64-valued attribute.
func Int64(k string, v int64) KeyValue { return KeyValue{Key: Key(k), Value: v} }

// Bool returns the KeyValue for a bool-valued attribute.
func Bool(k string, v bool) KeyValue { return KeyValue{Key: Key(k), Value: v} }
