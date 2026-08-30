// Package configutil is the lookalike that decides how the owner exemption is
// written. Its name merely STARTS WITH the owner's, so a prefix match on the
// bare string would silence it. It is ordinary project code and must be
// reported.
package configutil

import "os"

// Endpoint reads the environment from a helper that is not goga/config.
func Endpoint() string {
	return os.Getenv("GOGA__ENDPOINT") // want `reading the environment with os\.Getenv bypasses config\.Load`
}
