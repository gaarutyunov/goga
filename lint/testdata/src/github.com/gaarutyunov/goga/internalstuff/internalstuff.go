// Package internalstuff is a clean fixture. Its directory name has "internal"
// as a prefix but is not the segment "internal", and the rule matches whole
// path segments. It must produce no diagnostic.
package internalstuff

// Helper exists so the fixture is a package with content.
func Helper() string { return "internalstuff" }
