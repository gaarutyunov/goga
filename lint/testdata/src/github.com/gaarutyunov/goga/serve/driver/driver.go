// Package driver is a fixture stub of goga/serve/driver: the sub-package whose
// whole job is to BE the standard library listener. The exemption has to reach
// it as well as its parent, or the one adapter goga ships would be the loudest
// violator of goga's own rule.
package driver

import "net/http"

// Server is the standard library listener.
type Server struct{ srv *http.Server }

// ListenAndServe starts the listener.
func (s *Server) ListenAndServe(addr string, h http.Handler) error {
	s.srv = &http.Server{Addr: addr, Handler: h}
	return s.srv.ListenAndServe()
}
