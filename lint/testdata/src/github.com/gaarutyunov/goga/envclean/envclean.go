// Package envclean is the fixture that has to stay silent. Every declaration in
// it is a near miss: something the rule could plausibly fire on and must not.
//
// analysistest fails on an unexpected diagnostic as well as on a missing one,
// so this file is what proves gogaconfig is a rule about reading the
// environment rather than a blanket ban on naming os.
package envclean

import (
	"os"

	"github.com/gaarutyunov/goga/config"
)

// Correct is the shape the rule is asking for: the value comes from the struct
// goga/config filled, and the config file and the flags could both have
// overridden it.
func Correct(prefix string) string {
	return config.Env(prefix)[prefix]
}

// OtherOsFunctions is the fixture that catches a rule widened to any os call.
// None of these reads the environment, and reporting them would leave a package
// no way to touch the filesystem.
func OtherOsFunctions(path string) (*os.File, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	_ = os.TempDir()

	return os.Open(path)
}

// Writes pins that Setenv and Unsetenv are deliberately outside the trigger
// set. They write, and a write is not a source Load could have taken
// precedence over.
func Writes() {
	_ = os.Setenv("GOGA__WRITTEN", "1")
	_ = os.Unsetenv("GOGA__WRITTEN")
}

// Expansion pins that os.ExpandEnv is deliberately outside the trigger set: its
// argument here came FROM the configuration, so expanding it is not a bypass —
// and that is the common use, which is why reporting it would fire on correct
// code more often than on the defect.
func Expansion(loadedPath string) string {
	return os.ExpandEnv(loadedPath)
}

// Values pins that a reference to the function is not a call. Only a call reads
// anything.
func Values() func(string) string {
	_ = os.Args

	return os.Getenv
}

// Local declares functions with the trigger names on the package itself, which
// is what catches a rule matching the bare selector name instead of the package
// it belongs to.
func Local() string {
	return Getenv("GOGA__LOCAL") + shell{}.Getenv("GOGA__METHOD")
}

// Getenv is this package's own function, not os's.
func Getenv(string) string { return "" }

type shell struct{}

// Getenv is a method, and a method call is a selector on a value rather than on
// a package.
func (shell) Getenv(string) string { return "" }
