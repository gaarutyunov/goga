// Package serve runs an application's HTTP handler with the operational
// behaviour every service needs and no project gets right twice: bounded
// timeouts, a bounded graceful drain, probe and metrics endpoints that are
// never part of a request trace, and OpenTelemetry instrumentation applied
// exactly once.
//
// # The port is http.Handler
//
// [New] takes an [net/http.Handler]. That is the whole port. A *gin.Engine, a
// *chi.Mux, an *http.ServeMux and oapi-codegen's generated server all satisfy
// it unchanged, so none of them needs a goga adapter, a pattern translation or
// a routing DSL to sit behind.
//
// An earlier design had a Router port with an adapter per router. Its own
// evidence killed it: the same Use call applies to everything on a mux, applies
// only to later routes on gin, and panics on chi — three behaviours a port
// cannot promise at once. What remains is the listener, and only the listener:
// see [github.com/gaarutyunov/goga/serve/driver].
//
// # It installs no signal handling
//
// [Server.Run] takes a context and returns when it is cancelled. It does not
// call [os/signal.NotifyContext], and no goga surface does.
//
// The reason is a defect, not a preference. A process that serves HTTP and MCP
// together — a case goga exists to support — would otherwise install three
// signal handlers whose shutdowns race, with no ordering between draining
// connections, closing the database pool and flushing telemetry. The
// requirement that every surface stops together is then unimplementable. One
// process gets one handler, it belongs to the composition root, and it ships
// with goga/cli. Until a project adopts that, it keeps the signal handling it
// already has and passes the context down.
//
// # Operational endpoints are never traced
//
// /livez, /readyz, /healthz and /metrics are served by a mux that sits outside
// the OpenTelemetry wrapper, and there is no option that moves them inside.
// A liveness probe every second is not a request the service received, and a
// backend that has to be taught to drop them has already paid for storing them.
// Encoding that here is the point: it is a property another project discovered
// by hand, after the fact, in its traces.
//
// The same holds for [Server.Ops]: anything registered on the returned mux is
// dispatched before the instrumented handler is reached, so a pprof endpoint
// added there does not appear in the trace either.
//
// # Instrumentation belongs to serve, not to a listener
//
// [New] wraps the application handler in otelhttp exactly once. No listener
// instruments anything, which is what makes every listener instrumented
// identically and makes it impossible for the author of the next one to forget.
//
// # As
//
// [Server.As] reaches the listener underneath. The in-tree listener supports
// one target, **net/http.Server, which is how a caller sets a field goga does
// not expose — ConnState, ErrorLog, TLSConfig. Do it before [Server.Run]:
// net/http reads those fields as it serves and does not guard them.
//
// As returning false is not an error. A caller that treats it as one has
// written listener-locked code without saying so; the intended shape is to skip
// the tweak and carry on, so that the same program still runs against a test
// listener.
package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/serve/driver"
	"github.com/gaarutyunov/goga/telemetry"
)

// moduleName is the goga module this package reports itself as to
// goga/telemetry.
const moduleName = "serve"

var (
	// errNilHandler answers a New called without an application handler.
	errNilHandler = errors.New("goga/serve: handler must not be nil")

	// errEmptyAddr answers WithAddr and WithOpsAddr called with "".
	errEmptyAddr = errors.New("goga/serve: address must not be empty")

	// errNilDriver answers WithDriver called with a nil listener.
	errNilDriver = errors.New("goga/serve: driver must not be nil")

	// errOpsAddrWithCustomDriver answers the one combination of options goga
	// cannot honour: a separate operational address needs a second listener,
	// and goga cannot construct a second instance of a listener the caller
	// supplied ready-made.
	errOpsAddrWithCustomDriver = errors.New(
		"goga/serve: WithOpsAddr and WithDriver cannot be combined — a second " +
			"address needs a second listener, which goga cannot build from a " +
			"driver it was handed; serve the operational endpoints on the " +
			"application port, or run a second listener over Server.Ops yourself")
)

// UnsupportedTLSError reports that the configured listener cannot serve TLS.
//
// It is a type rather than a sentinel because a caller may reasonably branch on
// it: a program that would serve TLS if it could, and plain HTTP otherwise,
// needs to tell this failure apart from a bad certificate path.
type UnsupportedTLSError struct {
	// Driver is the Go type of the listener that was configured.
	Driver string
}

// Error implements error, naming the listener that lacks the capability.
func (e *UnsupportedTLSError) Error() string {
	return "listener " + e.Driver + " does not implement driver.TLSServer"
}

// Is reports whether target is an *UnsupportedTLSError, so that a caller can
// branch with errors.Is as well as errors.As.
func (e *UnsupportedTLSError) Is(target error) bool {
	_, ok := target.(*UnsupportedTLSError)
	return ok
}

// Server runs an application handler on one or two listeners.
//
// There is deliberately no *net/http.Server field here. The listener listens;
// the timeouts are carried to it in [driver.Options] at construction, and the
// in-tree listener owns its *http.Server internally. A design that kept both
// left it ambiguous which of the two actually bound the socket and where the
// timeouts had been applied.
type Server struct {
	// app is the application handler after middleware and the OpenTelemetry
	// wrapper: everything a request reaches once the operational mux has
	// declined it.
	app http.Handler

	// ops holds /livez, /readyz, /healthz and /metrics, and whatever else a
	// caller registers through Ops. Nothing on it is ever traced.
	ops *http.ServeMux

	// root is what the application listener is handed. With the operational
	// endpoints on the application port it dispatches between ops and app; with
	// them on their own port it is app.
	root http.Handler

	d    driver.Server
	addr Addr

	// opsDrv and opsAddr are set only when WithOpsAddr asked for a listener of
	// their own.
	opsDrv  driver.Server
	opsAddr Addr

	certFile string
	keyFile  string

	shutdownGrace time.Duration
	instr         *telemetry.Instrumentation
}

// New builds a server around an application handler.
//
// It applies the options, wraps h in the caller's middleware and then in
// OpenTelemetry instrumentation — once, here, so that no listener has to — and
// builds the operational mux beside it. Nothing binds a socket until
// [Server.Run].
//
// ctx is used for the construction span; it is not retained.
func New(ctx context.Context, h http.Handler, opts ...Option) (_ *Server, err error) {
	instr := telemetry.For(moduleName)
	_, end := instr.Start(ctx, "New")
	defer func() { end(err) }()

	if h == nil {
		return nil, errNilHandler
	}

	set, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, fmt.Errorf("goga/serve: new: %w", err)
	}

	// The operational endpoints get a listener of their own only when they were
	// given an address that is not the application's.
	separateOps := set.opsAddr != "" && set.opsAddr != set.addr

	drv := set.drv
	switch {
	case drv == nil:
		drv = newStdListener(set.driverOptions())
	case separateOps:
		return nil, errOpsAddrWithCustomDriver
	}

	if set.certFile != "" {
		if _, ok := drv.(driver.TLSServer); !ok {
			return nil, fmt.Errorf("goga/serve: new: %w",
				&UnsupportedTLSError{Driver: fmt.Sprintf("%T", drv)})
		}
	}

	traced := otelhttp.NewHandler(applyMiddleware(h, set.middleware), "",
		otelhttp.WithSpanNameFormatter(routePattern))

	s := &Server{
		app:           traced,
		ops:           newOpsMux(set.health, set.readiness),
		d:             drv,
		addr:          set.addr,
		certFile:      set.certFile,
		keyFile:       set.keyFile,
		shutdownGrace: set.shutdownGrace,
		instr:         instr,
	}

	if separateOps {
		s.root = traced
		s.opsAddr = set.opsAddr
		s.opsDrv = newStdListener(set.driverOptions())
	} else {
		s.root = rootHandler{app: traced, ops: s.ops}
	}

	return s, nil
}

// listener is one bound address and what is served on it.
type listener struct {
	drv  driver.Server
	addr Addr
	h    http.Handler
	tls  bool
}

// listeners returns the application listener, and the operational one when it
// was given an address of its own.
func (s *Server) listeners() []listener {
	ls := []listener{{drv: s.d, addr: s.addr, h: s.root, tls: s.certFile != ""}}
	if s.opsDrv != nil {
		ls = append(ls, listener{drv: s.opsDrv, addr: s.opsAddr, h: s.ops})
	}
	return ls
}

// serve binds one listener, over TLS where the caller configured it. The type
// assertion cannot fail: New rejected a non-TLS listener before this point.
func (s *Server) serve(l listener) error {
	if l.tls {
		ts, ok := l.drv.(driver.TLSServer)
		if !ok {
			return fmt.Errorf("goga/serve: %w",
				&UnsupportedTLSError{Driver: fmt.Sprintf("%T", l.drv)})
		}
		return ts.ListenAndServeTLS(string(l.addr), s.certFile, s.keyFile, l.h)
	}
	return l.drv.ListenAndServe(string(l.addr), l.h)
}

// Run serves until ctx is cancelled or a listener fails, then drains.
//
// It installs no signal handling: see the package documentation. Cancelling ctx
// starts a drain bounded by [WithShutdownGrace] — in-flight requests get that
// long to finish, and after it Run returns the deadline error rather than
// waiting on a connection that will never close. An orderly stop reports
// [net/http.ErrServerClosed] from the listener, which is success and is
// reported as a nil error.
func (s *Server) Run(ctx context.Context) error {
	ls := s.listeners()

	// Buffered by the number of listeners, so that every goroutine can report
	// and exit even when this function has already returned down another path.
	errc := make(chan error, len(ls))
	for _, l := range ls {
		go func() { errc <- s.serve(l) }()
	}

	select {
	case err := <-errc:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("goga/serve: run: %w", err)
	case <-ctx.Done():
		return s.drain(ctx, ls)
	}
}

// drain shuts every listener down under one bounded deadline.
//
// The deadline is taken from a context detached from ctx: ctx is already
// cancelled by the time drain is called, so a child of it would expire before
// the first connection had a chance to finish.
func (s *Server) drain(ctx context.Context, ls []listener) (err error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownGrace)
	defer cancel()

	sctx, end := s.instr.Start(sctx, "shutdown")
	defer func() { end(err) }()

	var errs []error
	for _, l := range ls {
		if e := l.drv.Shutdown(sctx); e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("goga/serve: shutdown: %w", errors.Join(errs...))
	}
	return nil
}

// Ops returns the mux the operational endpoints are registered on.
//
// Use it to add an endpoint that belongs beside the probes rather than in the
// application — /debug/pprof, a build-info handler — or to replace one of
// goga's. Everything on this mux is dispatched before the instrumented handler
// is reached, so none of it appears in a request trace.
func (s *Server) Ops() *http.ServeMux { return s.ops }

// As converts i to a listener-specific type, reporting whether it could.
//
// It is a runtime assertion: the compiler does not know the dynamic type behind
// the port, so this is the honest shape rather than a generic one that would
// merely look static and fail at run time anyway.
//
// Returning false is not an error. A caller that cannot reach the type it hoped
// for skips the adjustment and carries on, which is what keeps the same program
// running against a listener that does not have it. See the package
// documentation for what the in-tree listener supports.
func (s *Server) As(i any) bool {
	if i == nil {
		return false
	}
	a, ok := s.d.(interface{ As(i any) bool })
	if !ok {
		return false
	}
	return a.As(i)
}
