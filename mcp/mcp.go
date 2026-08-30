// Package mcp wraps the Model Context Protocol SDK so that every tool call,
// resource read and prompt render a project serves is traced, timed, bounded
// and contained, without the project writing a line of telemetry.
//
// # The wrapper is the only door
//
// [Server] holds the SDK server in an unexported field and [New] is its only
// constructor, so no package outside this one can build a [Server] around an
// SDK server of its own. [AddTool] is a free function rather than a method
// because Go methods cannot carry type parameters, and it is the route a tool
// takes onto the server: the span, the per-tool timeout and the panic guard are
// attached there, once, for every project.
//
// The one deliberate way out is [Server.SDK], which hands back the wrapped SDK
// server for the case goga did not anticipate. It is an escape hatch and it is
// named like one; goga/lint's gogamcp rule reports what is done with its
// result.
//
// # Tool failures are in-band
//
// A [ToolFunc] returns an ordinary error and the caller sees a
// [github.com/modelcontextprotocol/go-sdk/mcp.CallToolResult] with IsError set
// and the message as its content — never a JSON-RPC protocol error. That is
// what the MCP specification asks for, and the difference matters to the model
// on the other end: an in-band failure is something it can read and correct,
// while a protocol error looks like a broken server.
//
// A panicking tool is the same thing. The wrapper recovers it, reports it
// in-band, and does so from a deferred function so that the span still ends and
// the timeout context is still released on the way out. Without that, one
// tool's nil dereference takes down a server that was serving every other tool.
//
// # Trace context travels in _meta
//
// MCP defines no header for trace context, so goga's house convention is a W3C
// traceparent in the request's _meta: [Client] injects it and the server
// extracts it. See [github.com/gaarutyunov/goga/mcp.Client] and the package's
// trace.go for why the W3C propagator is pinned here rather than read from the
// OpenTelemetry global.
//
// # Transports are injected
//
// A transport is resolved by name through a [github.com/gaarutyunov/goga/registry.Registry]
// the composition root hands in — see [RegisterTransport] and [WithTransportRegistry].
// The one exception is the default, "stdio", which needs no configuration and
// no wiring at all, so a server built with no options at all still runs.
//
// # Mounting beside HTTP
//
// [Server.Handler] returns an [net/http.Handler] that serves this MCP server
// over streamable HTTP, so a process can expose HTTP and MCP on one port by
// passing it to [github.com/gaarutyunov/goga/serve.New] along with the rest of
// its routes.
package mcp

import (
	"context"
	"errors"
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

// moduleName is the goga module this package reports itself as to
// goga/telemetry.
const moduleName = "mcp"

var (
	// errEmptyName answers WithName and WithVersion called with "".
	errEmptyName = errors.New("goga/mcp: server name must not be empty")

	// errEmptyVersion answers WithVersion called with "".
	errEmptyVersion = errors.New("goga/mcp: server version must not be empty")

	// errEmptyEndpoint answers WithEndpoint called with "".
	errEmptyEndpoint = errors.New("goga/mcp: endpoint must not be empty")

	// errNilAuthenticator answers WithAuthenticator called with nil. There is
	// no "authenticate with nothing" setting: a program that wants no
	// authenticator does not pass the option.
	errNilAuthenticator = errors.New("goga/mcp: authenticator must not be nil")

	// errNilTelemetry answers WithTelemetry called with nil. WithTelemetry
	// REPLACES the instrumentation; it cannot remove it (design D6).
	errNilTelemetry = errors.New("goga/mcp: instrumentation must not be nil")

	// errNilRegistry answers WithTransportRegistry and RegisterTransport called
	// with a nil registry.
	errNilRegistry = errors.New("goga/mcp: transport registry must not be nil")
)

// Settings is the accessor interface a transport reads the MODULE's resolved
// settings through.
//
// # It carries the union of two declarations, on purpose
//
// The design document declared a Settings interface twice — once with
// Endpoint, once with ToolTimeout and ServerName — and described both as "this
// module's accessor interface". Two declarations of one name do not compile,
// and neither of them alone serves the two readers that exist: a transport
// needs the endpoint it binds and the server name it advertises, and the tool
// wrapper needs the timeout. So the two were merged into the one interface
// here, which is the union. Please do not "restore" the other declaration.
//
// What belongs in it is exactly this: values the caller gave the MODULE that
// something outside the module reads. It is not a transport's own settings
// type, which the registry decodes from configuration and which stays
// unexported in the transport's own package (design D14). The module's
// settings struct is unexported and implements this.
type Settings interface {
	// ServerName is the name the server advertises to clients, as given to
	// [WithName].
	ServerName() string

	// ToolTimeout bounds one tool call, as given to [WithToolTimeout].
	ToolTimeout() time.Duration

	// Endpoint is the address a listening transport binds, as given to
	// [WithEndpoint]. It is empty when the caller set none, which is the usual
	// case for stdio; a transport that needs one reports the omission itself,
	// naming its own package.
	Endpoint() string
}

// Authenticator authorises an incoming MCP request before it reaches a tool, a
// resource or a prompt.
//
// It sits on the SDK's receiving-middleware seam rather than on an HTTP
// handler, so one authenticator covers every transport — including stdio, where
// there is no request to read a header from. The context it returns is the one
// the rest of the call runs under, which is how an authenticator passes an
// identity down to a tool.
//
// A non-nil error is reported as a protocol error and not as an in-band tool
// failure. That is the one case where the distinction runs the other way from
// [ToolFunc]: a call that was never authorised did not fail, it did not happen.
type Authenticator interface {
	// Authenticate is called for every incoming request, with the JSON-RPC
	// method name and the request itself.
	Authenticate(ctx context.Context, method string, req sdkmcp.Request) (context.Context, error)
}

// Server is the portable type: an MCP server whose every operation is
// instrumented.
//
// Every field is unexported and [New] is the only constructor, so there is no
// way to obtain a Server holding an SDK server that goga did not wrap. That is
// what makes the instrumentation in [AddTool], [AddResource] and [AddPrompt]
// unavoidable rather than conventional.
type Server struct {
	// srv is the wrapped SDK server. Reachable from outside this package only
	// through [Server.SDK].
	srv *sdkmcp.Server

	// instr is the module's telemetry handle. It resolves through the
	// OpenTelemetry globals on every use, so a Server built before
	// telemetry.Setup starts emitting once Setup runs.
	instr *telemetry.Instrumentation

	// s is the resolved settings. AddTool reads the tool timeout from it and a
	// transport reads the endpoint and the server name.
	s *settings
}

// New builds an MCP server.
//
// It takes no context because it performs no I/O: nothing is bound and nothing
// is dialled until [Server.Run], which is where the transport is resolved and
// where a context is needed.
func New(opts ...Option) (*Server, error) {
	s, err := goga.Apply(defaults(), opts...)
	if err != nil {
		return nil, err
	}

	srv := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: s.name, Version: s.version},
		s.serverOptions(),
	)
	if s.auth != nil {
		srv.AddReceivingMiddleware(authMiddleware(s.auth))
	}

	return &Server{srv: srv, instr: s.instr, s: &s}, nil
}

// authMiddleware turns an [Authenticator] into the SDK's receiving middleware.
//
// It is installed once, in [New], so it covers tools, resources, prompts and
// every other incoming method rather than only the surfaces goga wraps.
func authMiddleware(a Authenticator) sdkmcp.Middleware {
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
			ctx, err := a.Authenticate(ctx, method, req)
			if err != nil {
				return nil, err
			}
			return next(ctx, method, req)
		}
	}
}

// Run resolves the configured transport and serves on it until ctx is
// cancelled.
//
// Like [github.com/gaarutyunov/goga/serve.Server.Run] it installs no signal
// handling of its own. A process serving HTTP and MCP together gets one signal
// handler, and it belongs to the composition root; three surfaces installing
// their own is how a shutdown ends up racing itself.
func (s *Server) Run(ctx context.Context) error {
	tr, err := s.resolveTransport(ctx)
	if err != nil {
		return err
	}
	return tr.Serve(ctx, s.srv)
}

// Handler returns an [net/http.Handler] serving this server over streamable
// HTTP, so that one process can expose HTTP and MCP on a single port:
//
//	mux.Handle("/mcp", mcpSrv.Handler())
//	srv, err := serve.New(ctx, mux, serve.WithAddr(":8080"))
//
// It is deliberately independent of [WithTransport]. The transport option
// selects how [Server.Run] serves when MCP is the process's own surface;
// Handler is for the case where it is mounted inside somebody else's, and there
// the listener, its timeouts and its drain belong to goga/serve. Streamable
// HTTP is the transport a mounted server speaks, because it is the one the
// current specification defines for exactly this arrangement.
func (s *Server) Handler() http.Handler {
	return sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return s.srv },
		&sdkmcp.StreamableHTTPOptions{Logger: s.instr.Logger()},
	)
}

// SDK returns the wrapped SDK server.
//
// It is the escape hatch for what goga did not anticipate, and using it costs
// the guarantees the wrapper provides: a tool registered through
// [github.com/modelcontextprotocol/go-sdk/mcp.AddTool] on the returned server
// has no span, no timeout and no panic guard. Prefer [AddTool], [AddResource]
// and [AddPrompt]; reach for this when the SDK offers something goga does not
// wrap yet, and say so where you call it.
func (s *Server) SDK() *sdkmcp.Server { return s.srv }
