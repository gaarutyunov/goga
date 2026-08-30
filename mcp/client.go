package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
	"github.com/gaarutyunov/goga/telemetry"
)

var (
	// errNilClientTransport answers a Connect with no transport. There is no
	// default: a client has to be told what to connect to, and guessing stdio
	// would silently talk to the process's own stdin.
	errNilClientTransport = errors.New("goga/mcp: client transport must not be nil — pass mcp.WithClientTransport")

	// errEmptyClientName answers WithClientName called with "".
	errEmptyClientName = errors.New("goga/mcp: client name must not be empty")

	// errEmptyClientVersion answers WithClientVersion called with "".
	errEmptyClientVersion = errors.New("goga/mcp: client version must not be empty")
)

// clientSettings is unexported for the same reason [settings] is: the only way
// a populated one exists is [goga.Apply] over [clientDefaults].
type clientSettings struct {
	name    string
	version string

	transport sdkmcp.Transport

	callTimeout time.Duration
	instr       *telemetry.Instrumentation
}

// clientDefaults returns the settings a [Connect] with no options runs with.
func clientDefaults() clientSettings {
	return clientSettings{
		name:        defaultName,
		version:     defaultVersion,
		callTimeout: defaultToolTimeout,
		instr:       telemetry.For(moduleName),
	}
}

// ClientOption configures [Connect]. Like [Option] it is an exported alias over
// an unexported settings type.
type ClientOption = goga.Option[clientSettings]

// WithClientName sets the client name advertised to the server.
func WithClientName(name string) ClientOption {
	return func(s *clientSettings) error {
		if name == "" {
			return errEmptyClientName
		}
		s.name = name
		return nil
	}
}

// WithClientVersion sets the client version advertised to the server.
func WithClientVersion(v string) ClientOption {
	return func(s *clientSettings) error {
		if v == "" {
			return errEmptyClientVersion
		}
		s.version = v
		return nil
	}
}

// WithClientTransport sets what the client connects over — an SDK transport
// such as CommandTransport for a locally launched server, or
// StreamableClientTransport for one over HTTP.
//
// This is the SDK's transport type and not goga's [Transport]. The two are not
// the same port: goga's serves a server, this dials one, and there is no
// registry on this side because a client's peer is an address it was given, not
// an adapter it selects.
func WithClientTransport(t sdkmcp.Transport) ClientOption {
	return func(s *clientSettings) error {
		if t == nil {
			return errNilClientTransport
		}
		s.transport = t
		return nil
	}
}

// WithClientCallTimeout bounds one call the client makes.
//
// It is the client-side mirror of [WithToolTimeout], and it is a separate bound
// on purpose: a caller cannot know the server's, and a call that hangs holds
// the caller's goroutine whatever the server thinks.
func WithClientCallTimeout(d time.Duration) ClientOption {
	return func(s *clientSettings) error {
		if d <= 0 {
			return fmt.Errorf("goga/mcp: client call timeout must be > 0, got %s", d)
		}
		s.callTimeout = d
		return nil
	}
}

// WithClientTelemetry REPLACES the client's instrumentation handle. As on the
// server there is no way to remove it (design D6).
func WithClientTelemetry(i *telemetry.Instrumentation) ClientOption {
	return func(s *clientSettings) error {
		if i == nil {
			return errNilTelemetry
		}
		s.instr = i
		return nil
	}
}

// Client is the consumer side, instrumented symmetrically with [Server]: a span
// per call, and goga's traceparent injected into every request's _meta so that
// a goga server on the other end continues the caller's trace rather than
// starting a new one.
//
// The injection is the half that makes [Server]'s extraction worth anything.
// MCP defines no trace-context header, so the convention only works if both
// ends agree — which is why the client ships in the same milestone as the
// server and not a later one.
type Client struct {
	// session is the connected session. Every call goes through it.
	session *sdkmcp.ClientSession

	// instr is the module's telemetry handle, resolved through the
	// OpenTelemetry globals on every use.
	instr *telemetry.Instrumentation

	// callTimeout bounds one call.
	callTimeout time.Duration
}

// Connect dials a server over the configured transport and completes the MCP
// initialisation handshake.
//
// The returned client owns the session; call [Client.Close] to end it.
func Connect(ctx context.Context, opts ...ClientOption) (_ *Client, err error) {
	s, err := goga.Apply(clientDefaults(), opts...)
	if err != nil {
		return nil, err
	}
	if s.transport == nil {
		return nil, errNilClientTransport
	}

	ctx, end := s.instr.Start(ctx, "client.connect")
	defer func() { end(err) }()

	c := sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: s.name, Version: s.version},
		&sdkmcp.ClientOptions{Logger: s.instr.Logger()},
	)

	session, err := c.Connect(ctx, s.transport, nil)
	if err != nil {
		return nil, fmt.Errorf("goga/mcp: connecting: %w", err)
	}

	return &Client{session: session, instr: s.instr, callTimeout: s.callTimeout}, nil
}

// CallTool calls a tool on the connected server.
//
// A result with IsError set is NOT reported as a Go error: that is the
// specification's in-band tool failure, it is the answer the call produced, and
// a caller reads it from the result. A non-nil error here means the call itself
// did not complete.
func (c *Client) CallTool(ctx context.Context, name string, args any) (_ *sdkmcp.CallToolResult, err error) {
	ctx, end := c.instr.Start(ctx, "client.tool", semconv.MCPToolName(name))
	defer func() { end(err) }()

	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	params := &sdkmcp.CallToolParams{Name: name, Arguments: args}
	params.Meta = injectTraceContext(ctx)

	res, err := c.session.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("goga/mcp: calling tool %q: %w", name, err)
	}
	return res, nil
}

// ReadResource reads a resource from the connected server.
func (c *Client) ReadResource(ctx context.Context, uri string) (_ *sdkmcp.ReadResourceResult, err error) {
	ctx, end := c.instr.Start(ctx, "client.resource", semconv.MCPResourceURI(uri))
	defer func() { end(err) }()

	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	params := &sdkmcp.ReadResourceParams{URI: uri}
	params.Meta = injectTraceContext(ctx)

	res, err := c.session.ReadResource(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("goga/mcp: reading resource %q: %w", uri, err)
	}
	return res, nil
}

// GetPrompt renders a prompt on the connected server.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (_ *sdkmcp.GetPromptResult, err error) {
	ctx, end := c.instr.Start(ctx, "client.prompt", semconv.MCPPromptName(name))
	defer func() { end(err) }()

	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	params := &sdkmcp.GetPromptParams{Name: name, Arguments: args}
	params.Meta = injectTraceContext(ctx)

	res, err := c.session.GetPrompt(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("goga/mcp: getting prompt %q: %w", name, err)
	}
	return res, nil
}

// Close ends the session.
func (c *Client) Close() error {
	if err := c.session.Close(); err != nil {
		return fmt.Errorf("goga/mcp: closing client session: %w", err)
	}
	return nil
}

// SDK returns the wrapped SDK session.
//
// It is [Server.SDK]'s counterpart and carries the same warning: a call made
// through it has no span and no traceparent, so a server on the other end
// starts a new trace instead of continuing this one.
func (c *Client) SDK() *sdkmcp.ClientSession { return c.session }
