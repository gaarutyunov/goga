// Package serve here is the other lookalike, and the sharper one: its last path
// segment IS "serve", but it sits at goga/httpx/serve rather than at goga/serve.
// The exemption belongs to a position in the tree, not to a segment name — a
// rule that matched the segment anywhere would go quiet in every package a
// project chose to call serve.
package serve

import "net/http"

// Bare builds the listener by hand, which this package has no licence to do.
func Bare(h http.Handler) *http.Server {
	return &http.Server{Handler: h} // want `constructing an http\.Server directly bypasses serve\.New`
}
