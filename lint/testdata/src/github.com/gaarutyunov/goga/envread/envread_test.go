package envread

import (
	"os"
	"testing"
)

// TestEndpoint is the fixture for the deliberate decision TO exempt _test.go
// files, in the same package whose non-test files are reported. A test binary
// has no config file and no flags, so there is no precedence for this read to
// take precedence over; reading the ambient environment to decide whether to
// skip an integration test is the correct way to write one, and goga has
// nothing else to suggest. It must produce no diagnostic.
func TestEndpoint(t *testing.T) {
	if os.Getenv("GOGA_INTEGRATION") == "" {
		t.Skip("integration endpoint not configured")
	}
	if _, ok := os.LookupEnv("GOGA_INTEGRATION_TOKEN"); !ok {
		t.Skip("integration token not configured")
	}
	_ = os.Environ()
}
