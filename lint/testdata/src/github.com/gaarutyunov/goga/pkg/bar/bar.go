// Package bar is a violating fixture: goga's own code under pkg/.
package bar // want `gogalayout: package .* lives inside "pkg/"; goga.s own code is laid out flat`

// Helper exists so the fixture is a package with content.
func Helper() string { return "bar" }
