// Package foo is a violating fixture: it is goga's own code under internal/,
// which the flat layout forbids.
package foo // want `gogalayout: package .* lives inside "internal/"; goga.s own code is laid out flat`

// Helper exists so the fixture is a package with content, not just a clause.
func Helper() string { return "foo" }
