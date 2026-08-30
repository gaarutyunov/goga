// Package httptransport is the streamable-HTTP transport adapter for
// goga/mcp: the transport a remotely hosted MCP server speaks.
//
// It is attached to a registry by the composition root, never by an init():
//
//	reg := registry.New(decode)
//	if err := httptransport.Provide(reg); err != nil { … }
//	srv, err := mcp.New(mcp.WithTransportRegistry(reg), mcp.WithTransport("http"),
//		mcp.WithEndpoint(":8080"))
//
// # It carries no instrumentation
//
// Nothing in this package opens a span, and that is the design rather than an
// omission (design D6, D7). The span belongs to the portable type — a tool call
// is traced by goga/mcp's wrapper before this transport is reached, and the
// listener this transport runs on is goga/serve's, which instruments the HTTP
// hop itself. An adapter that instrumented either would produce two spans for
// one operation.
//
// # The listener is goga/serve's
//
// A hand-built [net/http.Server] leaves every timeout at zero, which net/http
// documents as "no timeout", and cannot be shut down; goga/serve exists so that
// no package in the house writes one. So this transport builds the SDK's
// handler and hands it to [github.com/gaarutyunov/goga/serve.New], which is
// also where its drain comes from.
package httptransport

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
const Name = "http"

// defaultWriteTimeout bounds writing one response.
//
// It is thirty minutes rather than goga/serve's thirty seconds, and the reason
// is the protocol: a streamable-HTTP response is a server-sent-event stream
// that stays open for the whole of a model's turn, and net/http's write timeout
// is a deadline on the connection from the start of the request rather than on
// each write. Thirty seconds would cut every stream that outlived it. It is
// still a bound, because an unbounded one is how a stalled client holds a
// connection for the life of the process.
const defaultWriteTimeout = 30 * time.Minute

// settings is this adapter's own configuration, decoded by the registry from
// the subtree a config file gives it. It is unexported and never leaves the
// package: an adapter's settings type is not part of anybody's API (design
// D14).
type settings struct {
	// Endpoint is the address to bind. Empty means "use the endpoint the
	// caller gave goga/mcp", which is what makes this transport work with no
	// configuration file at all.
	Endpoint string `koanf:"endpoint"`

	// WriteTimeout bounds writing one response. Zero means
	// [defaultWriteTimeout].
	WriteTimeout time.Duration `koanf:"write_timeout"`

	// Stateless serves each request with a temporary session and no
	// Mcp-Session-Id header, which is what a horizontally scaled deployment
	// behind a load balancer needs.
	Stateless bool `koanf:"stateless"`

	// JSONResponse answers with application/json instead of an event stream,
	// for a client that cannot read one.
	JSONResponse bool `koanf:"json_response"`
}

// Provide registers this transport on r under [Name].
//
// It returns the registry's *[registry.DuplicateNameError] (matching
// [registry.ErrDuplicateName]) if the name is taken.
func Provide(r *registry.Registry) error {
	return mcp.RegisterTransport(r, Name, newTransport)
}

// newTransport is the constructor the registry calls. It resolves the endpoint
// and validates it here, at construction, so that a missing address is a
// startup error naming this package rather than a listener failing later.
func newTransport(_ context.Context, ms mcp.Settings, as settings) (mcp.Transport, error) {
	endpoint := as.Endpoint
	if endpoint == "" {
		endpoint = ms.Endpoint()
	}
	if endpoint == "" {
		return nil, errors.New("goga/mcp/httptransport: no endpoint — set it in this transport's settings, or pass mcp.WithEndpoint")
	}

	writeTimeout := as.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = defaultWriteTimeout
	}
	if writeTimeout < 0 {
		return nil, fmt.Errorf("goga/mcp/httptransport: write timeout must be >= 0, got %s", as.WriteTimeout)
	}

	return &transport{endpoint: endpoint, writeTimeout: writeTimeout, opts: &sdkmcp.StreamableHTTPOptions{
		Stateless:    as.Stateless,
		JSONResponse: as.JSONResponse,
	}}, nil
}

// transport serves one MCP server over streamable HTTP.
type transport struct {
	endpoint     string
	writeTimeout time.Duration
	opts         *sdkmcp.StreamableHTTPOptions
}

// Serve implements [mcp.Transport]. It runs until ctx is cancelled, then drains
// in-flight requests the way goga/serve does.
func (t *transport) Serve(ctx context.Context, srv *sdkmcp.Server) error {
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, t.opts)

	s, err := serve.New(ctx, handler,
		serve.WithAddr(t.endpoint),
		serve.WithWriteTimeout(t.writeTimeout))
	if err != nil {
		return fmt.Errorf("goga/mcp/httptransport: building the server: %w", err)
	}
	if err := s.Run(ctx); err != nil {
		return fmt.Errorf("goga/mcp/httptransport: serving on %s: %w", t.endpoint, err)
	}
	return nil
}
