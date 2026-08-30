package envread

// The import is aliased, so a rule matching the selector "os.Getenv" by name
// would miss this line entirely. Resolving the identifier through the file's
// own import block is what catches it.
import stdos "os"

// Aliased reads the environment through an aliased import.
func Aliased() string {
	return stdos.Getenv("GOGA__ALIASED") // want `reading the environment with os\.Getenv bypasses config\.Load`
}
