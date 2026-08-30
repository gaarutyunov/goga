// Package serveutil is the lookalike whose NAME starts with the owner
// package's. It is ordinary project code and the rule must fire here, which is
// what separates "the package that owns the listener" from "any package whose
// name mentions serving".
package serveutil

import "net/http"

// Listen is a bypass like any other.
func Listen(h http.Handler) error {
	return http.ListenAndServe(":8080", h) // want `serving HTTP through http\.ListenAndServe bypasses serve\.New`
}
