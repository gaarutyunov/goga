// Package baz is a clean fixture: a *dependency's* internal package. It is
// outside the configured module prefix, so gogalayout must stay silent — a
// dependency's layout is not goga's to enforce. This is the case that a rule
// keyed on the on-disk directory rather than the import path would get wrong.
package baz

// Helper exists so the fixture is a package with content.
func Helper() string { return "baz" }
