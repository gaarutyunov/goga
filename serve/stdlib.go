package serve

import (
	"context"
	"net/http"
	"sync"

	"github.com/gaarutyunov/goga/serve/driver"
)

// stdListener is goga's in-tree listener: the standard library's
// *net/http.Server, and nothing else.
//
// It is unexported and lives in this package rather than in one of its own
// because there is exactly one of it. An adapter package exists so that a
// second implementation can be added without touching the first; a package
// holding the only implementation of a two-method interface is a directory
// standing in for a decision that has not been made. A project that needs h2c,
// a unix socket or its own TLS configuration writes a [driver.Server] and
// passes it to [WithDriver], which is the seam this type proves works.
//
// It implements [driver.TLSServer] as well as [driver.Server].
type stdListener struct {
	// mu guards the fields net/http reads once serving has begun. The
	// *http.Server itself is built in the constructor rather than at
	// ListenAndServe so that As can reach it before Run — which is the point at
	// which reaching it is useful.
	mu  sync.Mutex
	srv *http.Server
}

// newStdListener builds the in-tree listener with the timeouts goga/serve
// resolved from its options. Every one of the three is set; see [driver.Options].
func newStdListener(o driver.Options) *stdListener {
	return &stdListener{srv: &http.Server{
		ReadHeaderTimeout: o.ReadHeaderTimeout,
		ReadTimeout:       o.ReadTimeout,
		WriteTimeout:      o.WriteTimeout,
	}}
}

// prepare installs the address and handler and hands back the server to serve
// on, under the lock that As also takes.
func (l *stdListener) prepare(addr string, h http.Handler) *http.Server {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.srv.Addr = addr
	l.srv.Handler = h
	return l.srv
}

// ListenAndServe implements [driver.Server].
func (l *stdListener) ListenAndServe(addr string, h http.Handler) error {
	return l.prepare(addr, h).ListenAndServe()
}

// ListenAndServeTLS implements [driver.TLSServer].
func (l *stdListener) ListenAndServeTLS(addr, certFile, keyFile string, h http.Handler) error {
	return l.prepare(addr, h).ListenAndServeTLS(certFile, keyFile)
}

// Shutdown implements [driver.Server]. Shutting down a listener that never
// started is not an error.
func (l *stdListener) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	srv := l.srv
	l.mu.Unlock()
	return srv.Shutdown(ctx)
}

// As reaches the underlying *net/http.Server.
//
// It is a runtime assertion and it returns false — never an error — for any
// other target, so a caller that asked for something this listener does not
// have skips the tweak and still runs.
func (l *stdListener) As(i any) bool {
	p, ok := i.(**http.Server)
	if !ok {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	*p = l.srv
	return true
}
