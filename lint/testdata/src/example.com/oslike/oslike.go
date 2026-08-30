// Package oslike exports Getenv, LookupEnv and Environ with the same shapes as
// the os package's, from a different import path. It exists so the clean
// fixture can prove gogaconfig keys on the import path and not on the
// identifier or the function name.
package oslike

// Getenv answers from the package's own table rather than from the process.
func Getenv(string) string { return "" }

// LookupEnv answers from the package's own table rather than from the process.
func LookupEnv(string) (string, bool) { return "", false }

// Environ returns the package's own table rather than the process environment.
func Environ() []string { return nil }
