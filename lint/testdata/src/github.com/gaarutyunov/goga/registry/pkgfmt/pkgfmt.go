// Package pkgfmt is a clean fixture: a nested package whose name starts with
// "pkg" but is not the segment "pkg". It must produce no diagnostic, and it
// also shows the rule is not "no nesting" — it is "no pkg/ and no internal/".
package pkgfmt

// Helper exists so the fixture is a package with content.
func Helper() string { return "pkgfmt" }
