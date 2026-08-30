package serve

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/serve/driver"
)

// Addr is a listen address, in the form [net.Listen] takes: "host:port", or
// ":8080" for every interface.
//
// It is a named type rather than a bare string so that a dependency-injection
// graph can supply it unambiguously. A wire provider set that also carries a
// database DSN and a service name would otherwise have three providers of
// string and no way to tell them apart.
type Addr string

// check is one named health or readiness probe.
type check struct {
	name string
	fn   func(ctx context.Context) error
}

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
type settings struct {
	addr    Addr
	opsAddr Addr

	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	shutdownGrace     time.Duration

	certFile string
	keyFile  string

	health     []check
	readiness  []check
	middleware []func(http.Handler) http.Handler

	drv driver.Server
}

// defaultAddr is the address a [Server] built with no [WithAddr] listens on.
const defaultAddr Addr = ":8080"

// The default timeouts. Every one of them is set: checklist item 2.1 is that a
// goga server never leaves a timeout unbounded, because an unbounded read
// header timeout turns one idle connection into a goroutine held for the life
// of the process, and that is the shape of the cheapest denial of service there
// is. The values are deliberately unremarkable — long enough not to cut off a
// slow mobile client, short enough to bound the damage — and every one of them
// is overridable.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 30 * time.Second

	// defaultShutdownGrace bounds the drain. Fifteen seconds is inside the
	// thirty a Kubernetes pod gets by default between SIGTERM and SIGKILL, so
	// the drain finishes — or is abandoned, in the bounded way — while the
	// process is still alive to report it.
	defaultShutdownGrace = 15 * time.Second
)

// defaults returns the settings a [New] with no options runs with.
func defaults() settings {
	return settings{
		addr:              defaultAddr,
		readHeaderTimeout: defaultReadHeaderTimeout,
		readTimeout:       defaultReadTimeout,
		writeTimeout:      defaultWriteTimeout,
		shutdownGrace:     defaultShutdownGrace,
	}
}

// driverOptions is the copy across the port boundary. The two structs
// deliberately do not alias: see [driver.Options].
func (s *settings) driverOptions() driver.Options {
	return driver.Options{
		ReadHeaderTimeout: s.readHeaderTimeout,
		ReadTimeout:       s.readTimeout,
		WriteTimeout:      s.writeTimeout,
	}
}

// Option configures [New]. It is an exported alias over an unexported settings
// type, so a caller can hold and pass a serve.Option and cannot write the
// struct it mutates.
type Option = goga.Option[settings]

// WithAddr sets the address the application listener binds.
//
// The default is ":8080".
func WithAddr(addr string) Option {
	return func(s *settings) error {
		if addr == "" {
			return errEmptyAddr
		}
		s.addr = Addr(addr)
		return nil
	}
}

// WithOpsAddr moves the operational endpoints — /livez, /readyz, /healthz and
// /metrics — onto a listener of their own.
//
// By default they share the application's port and are served by a separate mux
// on it, which is what most deployments want: one port to expose, and probes
// that still never reach the application's middleware or its traces. Give them
// their own address when the application port is public and the probes must not
// be, which is the case a shared port cannot serve.
//
// A second address needs a second listener, and goga cannot construct a second
// instance of a listener a caller supplied with [WithDriver]. Combining the two
// is therefore rejected by [New] rather than silently ignored.
func WithOpsAddr(addr string) Option {
	return func(s *settings) error {
		if addr == "" {
			return errEmptyAddr
		}
		s.opsAddr = Addr(addr)
		return nil
	}
}

// WithReadHeaderTimeout bounds how long a connection may take to send its
// request headers.
func WithReadHeaderTimeout(d time.Duration) Option {
	return positiveDuration("read header timeout", d, func(s *settings) *time.Duration {
		return &s.readHeaderTimeout
	})
}

// WithReadTimeout bounds reading the whole request, headers and body.
func WithReadTimeout(d time.Duration) Option {
	return positiveDuration("read timeout", d, func(s *settings) *time.Duration {
		return &s.readTimeout
	})
}

// WithWriteTimeout bounds writing the response.
func WithWriteTimeout(d time.Duration) Option {
	return positiveDuration("write timeout", d, func(s *settings) *time.Duration {
		return &s.writeTimeout
	})
}

// WithShutdownGrace bounds the drain [Server.Run] performs when its context is
// cancelled. In-flight requests get this long to finish; after it, Run returns
// with the deadline error rather than waiting on a connection that will never
// close.
func WithShutdownGrace(d time.Duration) Option {
	return positiveDuration("shutdown grace", d, func(s *settings) *time.Duration {
		return &s.shutdownGrace
	})
}

// positiveDuration builds the option shared by every duration setting: reject a
// non-positive value at the call site that supplied it, rather than at first
// use somewhere deeper in the program. Zero is rejected along with negatives,
// because zero means "no bound" to net/http and no goga timeout is unbounded.
func positiveDuration(name string, d time.Duration, field func(*settings) *time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf("goga/serve: %s must be > 0, got %s", name, d)
		}
		*field(s) = d
		return nil
	}
}

// WithTLS serves the application over TLS from the given certificate and key
// files.
//
// The configured listener must implement [driver.TLSServer]; one that does not
// is rejected by [New], not at first request. The in-tree listener implements
// it.
func WithTLS(certFile, keyFile string) Option {
	return func(s *settings) error {
		if certFile == "" || keyFile == "" {
			return fmt.Errorf(
				"goga/serve: TLS needs both a certificate and a key file, got %q and %q",
				certFile, keyFile)
		}
		s.certFile, s.keyFile = certFile, keyFile
		return nil
	}
}

// WithHealthCheck registers a named liveness check.
//
// Health checks answer /livez, and /healthz which is its legacy alias. They
// report whether the process itself is still viable, so a failing one is a
// request to restart it: register a check here only when a restart is the right
// remedy. A dependency that may be down without the process being broken —
// a database, a queue, a migration that has not run yet — is a readiness check.
// See [WithReadinessCheck].
//
// The function shape is fixed at func(context.Context) error so that anything
// with that shape is already a check. goga/migrate's Pending becomes a
// supported readiness input without either module learning about the other.
func WithHealthCheck(name string, fn func(ctx context.Context) error) Option {
	return addCheck("health check", name, fn, func(s *settings) *[]check { return &s.health })
}

// WithReadinessCheck registers a named readiness check.
//
// Readiness checks answer /readyz: whether this instance should be sent
// traffic right now. A failing one takes the instance out of the load
// balancer's rotation and leaves the process running, which is the correct
// response to a dependency that is temporarily unavailable.
func WithReadinessCheck(name string, fn func(ctx context.Context) error) Option {
	return addCheck("readiness check", name, fn, func(s *settings) *[]check { return &s.readiness })
}

// addCheck is the body [WithHealthCheck] and [WithReadinessCheck] share.
// Duplicate names are rejected: two checks reporting under one name make the
// probe output ambiguous about which dependency is down.
func addCheck(kind, name string, fn func(ctx context.Context) error, field func(*settings) *[]check) Option {
	return func(s *settings) error {
		if name == "" {
			return fmt.Errorf("goga/serve: %s name must not be empty", kind)
		}
		if fn == nil {
			return fmt.Errorf("goga/serve: %s %q must not be nil", kind, name)
		}
		list := field(s)
		for _, c := range *list {
			if c.name == name {
				return fmt.Errorf("goga/serve: duplicate %s name %q", kind, name)
			}
		}
		*list = append(*list, check{name: name, fn: fn})
		return nil
	}
}

// WithMiddleware appends middleware around the application handler, outermost
// first: the first middleware passed sees a request before the second does.
//
// Middleware wraps the application handler and is itself wrapped by goga's
// OpenTelemetry instrumentation, so the time a middleware spends is inside the
// server span rather than beside it. It never sees a request to an operational
// endpoint: those are dispatched before the traced handler is reached, which is
// checklist item 2.3 and is not configurable.
func WithMiddleware(mw ...func(http.Handler) http.Handler) Option {
	return func(s *settings) error {
		for i, m := range mw {
			if m == nil {
				return fmt.Errorf("goga/serve: middleware %d must not be nil", i)
			}
		}
		s.middleware = append(s.middleware, mw...)
		return nil
	}
}

// WithDriver supplies the listener.
//
// The default is the standard library's *net/http.Server, and it is the only
// listener goga ships: h2c and a unix socket are the plausible second and
// third, and they arrive when a project needs one. There is deliberately no
// name-keyed registry of listeners and no URL scheme to select one — a table
// with a single entry is a lookup that can only fail — so a project that has
// its own passes it here directly.
func WithDriver(d driver.Server) Option {
	return func(s *settings) error {
		if d == nil {
			return errNilDriver
		}
		s.drv = d
		return nil
	}
}
