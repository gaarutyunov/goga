// Package bypass is the violating fixture: goga's own code serving HTTP
// without going through serve.New, in each of the shapes the rule recognises.
package bypass

import (
	"net/http"
	"time"
)

// Pointer is the common shape — the one that looks most like careful code,
// which is why the rule has to fire on it. Setting some of the timeouts is not
// setting them: every field left out is still zero, and zero is no timeout.
func Pointer(h http.Handler) *http.Server {
	return &http.Server{ // want `constructing an http\.Server directly bypasses serve\.New, losing the bounded timeouts and the graceful drain; pass your handler to serve\.New instead`
		Addr:              ":8080",
		Handler:           h,
		ReadHeaderTimeout: time.Second,
	}
}

// Value proves the rule is about the composite literal and not about the
// pointer: a value literal loses exactly the same properties.
func Value(h http.Handler) http.Server {
	return http.Server{Addr: ":8080", Handler: h} // want `constructing an http\.Server directly bypasses serve\.New`
}

// Run is the worse half of the rule. There is no *http.Server to shut down,
// because ListenAndServe builds one internally and returns only the error.
func Run(h http.Handler) error {
	return http.ListenAndServe(":8080", h) // want `serving HTTP through http\.ListenAndServe bypasses serve\.New, losing the bounded timeouts and the graceful drain; pass your handler to serve\.New instead`
}

// RunTLS is the same defect over TLS, and it is reported separately so the
// diagnostic names the function the reader actually wrote.
func RunTLS(h http.Handler) error {
	return http.ListenAndServeTLS(":8443", "cert.pem", "key.pem", h) // want `serving HTTP through http\.ListenAndServeTLS bypasses serve\.New`
}
