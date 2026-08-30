// Package ssetransport is the HTTP+SSE transport adapter for goga/mcp: the
// transport the 2024-11-05 revision of the specification defined, kept for the
// clients that still speak it.
//
// New deployments should use goga/mcp/httptransport. This one exists because
// "the client we have to work with is older than the spec revision we like" is
// a deployment fact rather than a design choice, and a framework that cannot
// serve it sends the project back to a hand-rolled server.
//
// It is attached to a registry by the composition root, never by an init():
//
//	reg := registry.New(decode)
//	if err := ssetransport.Provide(reg); err != nil { … }
//	srv, err := mcp.New(mcp.WithTransportRegistry(reg), mcp.WithTransport("sse"),
//		mcp.WithEndpoint(":8080"))
//
// Like goga/mcp/httptransport it carries no instrumentation of its own and runs
// on goga/serve's listener; the reasons are the same and are written out there.
package ssetransport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga/mcp"
	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/serve"
)

// Name is the plain adapter name this transport registers under, and the value
// that appears in a configuration file.
const Name = "sse"

// defaultWriteTimeout bounds writing one response.
//
// An SSE stream is open for as long as the session lasts, and net/http's write
// timeout is a deadline on the connection from the start of the request, so
// this has to exceed the longest session rather than the longest response.
// Thirty minutes is the same value goga/mcp/httptransport uses, for the same
// reason, and it is still a bound.
const defaultWriteTimeout = 30 * time.Minute

// settings is this adapter's own configuration, decoded by the registry from
// the subtree a config file gives it. It is unexported and never leaves the
// package (design D14).
type settings struct {
	// Endpoint is the address to bind. Empty means "use the endpoint the
	// caller gave goga/mcp".
	Endpoint string `koanf:"endpoint"`

	// WriteTimeout bounds writing one response. Zero means
	// [defaultWriteTimeout].
	WriteTimeout time.Duration `koanf:"write_timeout"`

	// DisableLocalhostProtection turns off the SDK's DNS-rebinding guard, which
	// rejects a request arriving on a loopback address whose Host header names
	// something else. Leave it off unless you know why you are turning it on.
	DisableLocalhostProtection bool `koanf:"disable_localhost_protection"`
}

// Provide registers this transport on r under [Name].
//
// It returns the registry's *[registry.DuplicateNameError] (matching
// [registry.ErrDuplicateName]) if the name is taken.
func Provide(r *registry.Registry) error {
	return mcp.RegisterTransport(r, Name, newTransport)
}

// newTransport is the constructor the registry calls.
func newTransport(_ context.Context, ms mcp.Settings, as settings) (mcp.Transport, error) {
	endpoint := as.Endpoint
	if endpoint == "" {
		endpoint = ms.Endpoint()
	}
	if endpoint == "" {
		return nil, errors.New("goga/mcp/ssetransport: no endpoint — set it in this transport's settings, or pass mcp.WithEndpoint")
	}

	writeTimeout := as.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = defaultWriteTimeout
	}
	if writeTimeout < 0 {
		return nil, fmt.Errorf("goga/mcp/ssetransport: write timeout must be >= 0, got %s", as.WriteTimeout)
	}

	return &transport{endpoint: endpoint, writeTimeout: writeTimeout, opts: &sdkmcp.SSEOptions{
		DisableLocalhostProtection: as.DisableLocalhostProtection,
	}}, nil
}

// transport serves one MCP server over HTTP+SSE.
type transport struct {
	endpoint     string
	writeTimeout time.Duration
	opts         *sdkmcp.SSEOptions
}

// Serve implements [mcp.Transport]. It runs until ctx is cancelled, then drains
// in-flight requests the way goga/serve does.
func (t *transport) Serve(ctx context.Context, srv *sdkmcp.Server) error {
	handler := sdkmcp.NewSSEHandler(func(*http.Request) *sdkmcp.Server { return srv }, t.opts)

	s, err := serve.New(ctx, handler,
		serve.WithAddr(t.endpoint),
		serve.WithWriteTimeout(t.writeTimeout))
	if err != nil {
		return fmt.Errorf("goga/mcp/ssetransport: building the server: %w", err)
	}
	if err := s.Run(ctx); err != nil {
		return fmt.Errorf("goga/mcp/ssetransport: serving on %s: %w", t.endpoint, err)
	}
	return nil
}
