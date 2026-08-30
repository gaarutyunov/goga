// Package httpsrv is a clean fixture outside the configured module prefix: a
// dependency listening the way it chooses to. goga's conventions are not a
// dependency's to keep, so the rule must stay silent here — the case a rule
// scoped by directory rather than by import path would get wrong.
package httpsrv

import "net/http"

// Serve listens on the dependency's own terms.
func Serve(h http.Handler) error { return http.ListenAndServe(":8080", h) }
