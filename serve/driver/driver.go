// Package driver defines the one seam goga/serve dispatches through: the
// listener.
//
// # There is no Router port here, on purpose
//
// An earlier design had a Router port with muxrouter, chirouter and ginrouter
// adapters. It was dropped, and its own evidence is what dropped it: gin, chi
// and *http.ServeMux are already [net/http.Handler]s, so they need no goga
// adapter and no pattern translation, and the one behaviour a routing DSL would
// have had to paper over is not paperable. The same Use call applies to
// everything already registered on an *http.ServeMux wrapper, applies only to
// later routes on gin, and panics on chi. A port cannot promise a behaviour its
// implementations disagree about.
//
// So the port goga/serve exposes to an application is http.Handler itself, and
// the seam that remains is the listener: the thing that binds an address and
// dispatches into that handler. [Server] is that seam, and neither of its two
// methods knows what a route is.
//
// # This package is exempt from the v1 freeze
//
// goga freezes its portable types at v1. It does not freeze its driver
// packages: they are the extension point, and they evolve by two channels.
//
//  1. [Options] gains fields. New fields are additive, and an adapter may
//     ignore any field it does not support.
//  2. New capabilities arrive as new *optional* interfaces — [TLSServer] is the
//     first — which goga/serve type-asserts. A capability never arrives as a
//     new method on [Server], because that would force every adapter that does
//     not have the capability to grow a stub returning an error.
//
// Adding a method to an existing interface in this package is a breaking change
// and needs a major version. The promise is stated here rather than in a
// release note because this is where the reader who needs it is looking.
package driver

import (
	"context"
	"net/http"
	"time"
)

// Server dispatches requests to an [net/http.Handler].
//
// ListenAndServe binds addr and serves h until [Server.Shutdown] is called; it
// returns [net/http.ErrServerClosed] on an orderly stop, as the standard
// library does, and goga/serve treats that value as success.
//
// Shutdown stops accepting connections and waits for the in-flight ones to
// finish, returning ctx.Err() if the context expires first. goga/serve always
// passes a context with a deadline, so an implementation that blocks forever on
// a wedged connection cannot hang the process.
type Server interface {
	ListenAndServe(addr string, h http.Handler) error
	Shutdown(ctx context.Context) error
}

// TLSServer is the optional interface for a listener that can serve TLS.
//
// It is a separate interface rather than a third method on [Server] so that a
// listener which does not serve TLS — an in-process test listener, a unix
// socket — does not have to grow a stub for it. goga/serve type-asserts this
// interface when the caller configured a certificate, and refuses at
// construction time if the configured listener does not implement it.
type TLSServer interface {
	ListenAndServeTLS(addr, certFile, keyFile string, h http.Handler) error
}

// Options is what goga/serve hands a listener at construction. It is exported
// by necessity: an adapter in another package has to name it in the signature
// of its own constructor, and goga/serve/servetest constructs one.
//
// Constructing an Options buys an application nothing. No goga entry point
// accepts one, and the only way to obtain a *serve.Server is serve.New, which
// instruments. This is the vocabulary of a boundary the application never
// reaches, not a second way in.
//
// The fields deliberately duplicate serve's own unexported settings rather than
// aliasing or embedding them: each type's documentation addresses its own
// audience, and the two are allowed to diverge.
//
// New fields are additive. An adapter may ignore any field it does not support;
// zero means "not configured by the caller", and an adapter that has a sensible
// bound of its own should apply it rather than leave the value unbounded.
type Options struct {
	// ReadHeaderTimeout bounds how long a connection may take to send its
	// request headers. It is the timeout that matters most: without it a single
	// idle connection holds a goroutine for the life of the process.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds reading the entire request, headers and body.
	ReadTimeout time.Duration

	// WriteTimeout bounds writing the response, measured from the end of the
	// request headers.
	WriteTimeout time.Duration
}
