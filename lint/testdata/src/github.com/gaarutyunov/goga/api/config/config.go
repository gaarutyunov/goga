// Package config is the second lookalike: its LAST SEGMENT is "config", but its
// position in the tree is not goga/config. A segment match anywhere in the path
// would silence it. Exempting a package because of its name is how a rule
// quietly stops covering the code it was written for, so this one is reported.
package config

import "os"

// Endpoint reads the environment from a package that merely shares a name with
// the owner.
func Endpoint() string {
	return os.Getenv("GOGA__ENDPOINT") // want `reading the environment with os\.Getenv bypasses config\.Load`
}
