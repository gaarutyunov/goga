// Package envread is the violating fixture: an ordinary library package inside
// the module reading the process environment itself, which is the way config
// precedence gets bypassed in practice.
package envread

import "os"

// Endpoint is the plain form of the defect. Whatever the config file or the
// --endpoint flag said was resolved before this line ran, and this line does
// not consult it.
func Endpoint() string {
	return os.Getenv("GOGA__ENDPOINT") // want `reading the environment with os\.Getenv bypasses config\.Load`
}

// Optional is the same bypass with the ok-return. It is in the trigger set
// because leaving it out would make the rule evadable by a one-character edit.
func Optional() (string, bool) {
	return os.LookupEnv("GOGA__OPTIONAL") // want `reading the environment with os\.LookupEnv bypasses config\.Load`
}

// All is the same bypass wholesale.
func All() []string {
	return os.Environ() // want `reading the environment with os\.Environ bypasses config\.Load`
}

// Nested pins that the walk reaches a call that is not the whole statement.
func Nested() int {
	return len(os.Getenv("GOGA__NESTED")) // want `reading the environment with os\.Getenv bypasses config\.Load`
}
