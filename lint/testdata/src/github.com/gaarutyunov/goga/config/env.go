package config

// This file is the owner exemption. Reading the environment is what the package
// that owns the environment source does — WithEnv is a source — and a rule
// firing here would be telling the loader not to load. It must produce no
// diagnostic.
import "os"

// Env is the environment source, expressed the way the real one is.
func Env(prefix string) map[string]string {
	if _, ok := os.LookupEnv(prefix + "__DISABLED"); ok {
		return nil
	}

	out := map[string]string{prefix: os.Getenv(prefix)}
	for _, entry := range os.Environ() {
		out[entry] = entry
	}

	return out
}
