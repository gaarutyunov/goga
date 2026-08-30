// Package envdep is a clean fixture outside the configured module prefix: a
// dependency reading the environment the way it chooses to. goga's conventions
// are not a dependency's to keep, so the rule must stay silent here — the case
// a rule scoped by directory rather than by import path would get wrong.
package envdep

import "os"

// Endpoint reads the dependency's own variable on the dependency's own terms.
func Endpoint() string { return os.Getenv("DEP_ENDPOINT") }
