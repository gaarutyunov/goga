package envclean

// This file binds the identifier "os" to a package that is NOT the standard
// library's, and then calls Getenv on it. It is the fixture for the rule's most
// tempting shortcut: matching the selector "os.Getenv" by name. Doing that would
// report these lines. The rule resolves the identifier through the file's own
// import block instead, so it must stay silent — and because the binding is
// per-file, the real os package next door is unaffected.
//
// The real os is imported here TOO, under an alias, and that is load-bearing
// rather than decoration. The analyzer skips a file that does not import os at
// all, so a lookalike file importing only the lookalike would be excluded
// before the selector is ever examined — and would then prove the skip rather
// than the resolution it is written to prove. A mutation that trusted the bare
// identifier "os" in addition to the import block passed against exactly that
// version of this file. With the real package present the walk runs, and the
// two identifiers have to be told apart on their merits.
import (
	os "example.com/oslike"
	realos "os"
)

// NotTheOsPackage reads from a table, not from the process environment.
func NotTheOsPackage() (string, bool) {
	_ = os.Environ()

	return os.LookupEnv(os.Getenv("GOGA__LOOKALIKE"))
}

// TheRealOsIsHereToo touches the standard library in a way the rule does not
// report, so that the file imports it without violating anything.
func TheRealOsIsHereToo() []string { return realos.Args }
