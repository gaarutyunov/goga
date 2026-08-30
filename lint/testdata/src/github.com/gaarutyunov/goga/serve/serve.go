// Package serve is a fixture stub of goga/serve, and simultaneously the fixture
// for the package gogaserve must never report: the portable type necessarily
// constructs the *http.Server every other package is forbidden to construct.
// It carries no `want` comment, so analysistest fails if the rule ever starts
// firing here.
//
// It declares only the surface the clean fixtures call, and it is not a model
// of the real package.
package serve

import "net/http"

// Server is the portable type.
type Server struct{ inner *http.Server }

// New wraps the application's handler. The port is http.Handler, so a router
// of any kind satisfies it unchanged.
func New(h http.Handler) *Server {
	return &Server{inner: &http.Server{Handler: h}}
}

// Run serves until the listener stops.
func (s *Server) Run() error { return s.inner.ListenAndServe() }

// As reaches the underlying listener. Returning false is not an error.
func (s *Server) As(i any) bool {
	target, ok := i.(**http.Server)
	if !ok {
		return false
	}
	*target = s.inner
	return true
}
