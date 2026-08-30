package mcp

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
)

// ToolFunc is what a project writes: no SDK types, no telemetry, no timeout,
// no panic guard.
//
// In and Out are ordinary structs and the SDK derives their JSON schema, so the
// tool's contract is its Go signature. A returned error is a TOOL failure and
// reaches the caller in-band; see [AddTool].
type ToolFunc[In, Out any] func(ctx context.Context, in In) (Out, error)

// toolSettings is one tool's registration settings, folded from [ToolOption]s
// over the server's own.
type toolSettings struct {
	title       string
	annotations *sdkmcp.ToolAnnotations
	timeout     time.Duration
}

// ToolOption configures one tool at [AddTool]. Like [Option] it is an exported
// alias over an unexported settings type.
type ToolOption = goga.Option[toolSettings]

// WithToolTitle sets the tool's display title, which a client shows in place of
// its name where it has one.
func WithToolTitle(title string) ToolOption {
	return func(s *toolSettings) error {
		if title == "" {
			return fmt.Errorf("goga/mcp: tool title must not be empty")
		}
		s.title = title
		return nil
	}
}

// WithToolAnnotations attaches the MCP annotations that tell a client how the
// tool behaves — read-only, destructive, idempotent, open-world.
//
// They are hints to the model and its host, not enforcement: nothing in goga
// stops a tool annotated read-only from writing. They are worth setting anyway,
// because a host that asks for confirmation before a destructive call can only
// do so if the tool said it was one.
func WithToolAnnotations(a *sdkmcp.ToolAnnotations) ToolOption {
	return func(s *toolSettings) error {
		if a == nil {
			return fmt.Errorf("goga/mcp: tool annotations must not be nil")
		}
		s.annotations = a
		return nil
	}
}

// WithToolCallTimeout overrides the server's [WithToolTimeout] for this one
// tool, for the tool that legitimately takes longer — or must take less — than
// the rest.
//
// There is no value that removes the bound.
func WithToolCallTimeout(d time.Duration) ToolOption {
	return func(s *toolSettings) error {
		if d <= 0 {
			return fmt.Errorf("goga/mcp: tool call timeout must be > 0, got %s", d)
		}
		s.timeout = d
		return nil
	}
}

// AddTool registers fn on s under name.
//
// It is a free generic function because Go methods cannot carry type
// parameters, and it is the route a tool takes onto the wrapped SDK server.
// Everything the owner's telemetry rule asks for is attached here, once, for
// every project that ever writes a tool:
//
//   - the caller's trace context is restored from the request's _meta;
//   - a span named goga.mcp.tool is opened, carrying the tool name, the
//     session id, the duration and, on failure, error.type;
//   - the call runs under a timeout;
//   - a panicking tool is recovered and reported rather than taking the
//     process with it, from a deferred function, so the span still ends and the
//     timeout context is still released on that path.
//
// # A tool failure is in-band
//
// An error from fn becomes a
// [github.com/modelcontextprotocol/go-sdk/mcp.CallToolResult] with IsError set
// and the message as its text content, never a JSON-RPC protocol error. That is
// what the specification asks for, and it is the difference between a model
// that can read the failure and correct itself and a host that thinks the
// server is broken.
//
// The conversion is guaranteed by construction: the wrapper hands the SDK a
// plain error, wrapped in this package's own type, so it can never be the
// structured [github.com/modelcontextprotocol/go-sdk/jsonrpc.Error] the SDK
// would forward as a protocol error. The SDK then packs it in-band. The
// alternative — building the result here and returning it with a zero Out — was
// measured and rejected: the SDK marshals the returned Out into
// StructuredContent and validates it against the tool's output schema, and a
// zero Out with a slice field marshals to null, fails that validation, and
// turns the tool failure into exactly the protocol error this rule forbids.
//
// AddTool panics if the tool's name or its In/Out types cannot produce a valid
// schema, because the SDK does. That is a programming error discovered at
// startup with nothing running yet, which is the one place a panic is the right
// report.
func AddTool[In, Out any](s *Server, name, desc string, fn ToolFunc[In, Out], opts ...ToolOption) {
	ts, err := goga.Apply(toolSettings{timeout: s.s.toolTimeout}, opts...)
	if err != nil {
		panic(fmt.Sprintf("goga/mcp: tool %q: %v", name, err))
	}

	tool := &sdkmcp.Tool{
		Name:        name,
		Description: desc,
		Title:       ts.title,
		Annotations: ts.annotations,
	}

	sdkmcp.AddTool(s.srv, tool,
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in In) (_ *sdkmcp.CallToolResult, out Out, err error) {
			// MCP defines no trace-context header, so goga's house convention
			// is traceparent in the request _meta; restore it if the caller
			// sent one.
			ctx = extractTraceContext(ctx, req.Params.Meta)
			ctx, end := s.instr.Start(ctx, "tool",
				semconv.MCPToolName(name), semconv.MCPSessionID(sessionID(req)))

			ctx, cancel := context.WithTimeout(ctx, ts.timeout)
			defer cancel()

			// Deferred, so that a panicking tool still ends its span and still
			// releases its timeout context. Without this, one tool's nil
			// dereference takes down a server that was serving every other
			// tool — and this wrapper is the only place that can be fixed once
			// for every project.
			defer func() {
				if p := recover(); p != nil {
					panicked := &PanicError{Operation: "tool", Name: name, Value: p, Stack: debug.Stack()}
					err = panicked
					s.instr.Logger().ErrorContext(ctx, "tool panicked",
						"tool", name, "panic", fmt.Sprint(p), "stack", string(panicked.Stack))
				}
				end(err)
			}()

			out, err = fn(ctx, in)
			if err != nil {
				// The SDK discards out entirely once the handler reports an
				// error, so the value returned alongside it is never seen.
				err = &ToolError{Tool: name, Err: err}
			}
			return nil, out, err
		})
}

// sessionID reports the id of the session a request arrived on, or "" when
// there is none.
//
// A request can reach a handler with no session — a stateless streamable-HTTP
// server has none to report, and neither does a direct call in a test — so the
// nil check is the ordinary path and not a defensive one.
func sessionID(req *sdkmcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	return req.Session.ID()
}

// ToolError wraps the error a [ToolFunc] returned.
//
// It names the tool, which the error alone does not: a project's error message
// says what went wrong, and a caller reading a server's log wants to know which
// of forty tools it went wrong in. It also guarantees the shape the in-band
// conversion depends on — a plain error, never a structured JSON-RPC one — so a
// tool failure cannot be reported to the client as a protocol error.
type ToolError struct {
	// Tool is the name the tool was registered under.
	Tool string
	// Err is the error the tool returned.
	Err error
}

// Error implements error.
func (e *ToolError) Error() string { return fmt.Sprintf("goga/mcp: tool %q: %v", e.Tool, e.Err) }

// Unwrap returns the tool's own error, so that a caller inside the server
// process can still branch on it with errors.Is and errors.As.
func (e *ToolError) Unwrap() error { return e.Err }

// PanicError reports that a handler panicked and was contained.
//
// It is an error and not a re-panic because the alternative is a dead server:
// the panic is reported to the caller like any other failure, and the other
// thirty-nine tools keep serving. The stack is captured at the point of
// recovery, where it is still the panicking goroutine's, and logged; it is
// carried on the value rather than in the message so that a log pipeline can
// keep the two apart.
type PanicError struct {
	// Operation is the kind of handler that panicked: "tool", "resource" or
	// "prompt". It matches the span name's last segment.
	Operation string
	// Name is what the handler was registered under — a tool or prompt name,
	// or a resource URI.
	Name string
	// Value is what the handler panicked with.
	Value any
	// Stack is the stack of the recovering goroutine, as [runtime/debug.Stack]
	// renders it.
	Stack []byte
}

// Error implements error. It does NOT include the stack: the message is what
// reaches the model on the other end of the call, and a stack trace there is
// noise at best and an information leak at worst.
func (e *PanicError) Error() string {
	return fmt.Sprintf("goga/mcp: %s %q panicked: %v", e.Operation, e.Name, e.Value)
}
