package bypass

import (
	"net/http"
	"testing"
)

// TestListener is the fixture for the deliberate decision not to exempt
// _test.go files. A test that reaches for a real listener has bypassed
// serve.New exactly as production code would, and httptest — which this rule
// never touches — is the answer for every test that only needs a server to
// talk to. Exempting test files would hide the difference.
func TestListener(t *testing.T) {
	_ = &http.Server{} // want `constructing an http\.Server directly bypasses serve\.New`
}
