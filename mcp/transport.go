package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/semconv"
)

// Transport is this module's port: one way of carrying MCP traffic for a
// server.
//
// It is deliberately a single method. Everything cross-cutting — the span, the
// timeout, the panic guard, the trace context, the authenticator — is attached
// by the wrapper before a transport ever sees a request, so a new transport is
// instrumented the day it is written and its author does nothing to make that
// true (design D6, D7).
//
// A transport is handed the SDK server rather than goga's [Server] because that
// is what the SDK's own entry points take: Run wants a
// [github.com/modelcontextprotocol/go-sdk/mcp.Transport] and the HTTP handlers
// want a function returning the server. This does not widen the route to the
// wrapped server: an adapter is code the composition root installs on purpose,
// the same category as [Server.SDK], and a tool registered on it is a tool the
// composition root wrote rather than one the wrapper let through.
type Transport interface {
	// Serve carries traffic for srv until ctx is cancelled. It returns nil on
	// a clean shutdown, so a cancelled context is not an error.
	Serve(ctx context.Context, srv *sdkmcp.Server) error
}

// transportFactory is what this module actually stores in the registry: a
// constructor that still needs the module's [Settings].
//
// The registry fixes one constructor shape, func(context.Context, S) (P,
// error), and it has to, or type inference would break. [RegisterTransport]'s
// shape has a third parameter, so the registered port is this function type and
// not [Transport] itself: registering yields the factory, and the module calls
// it with its own settings at resolve time. The outer constructor closes over
// the decoded adapter settings; the inner one does the work, with the context
// that is live when the server actually runs.
type transportFactory func(ctx context.Context, ms Settings) (Transport, error)

// RegisterTransport contributes one transport adapter to a registry, under a
// plain name a configuration file can carry: "stdio", "http", "sse".
//
// S is the adapter's own settings type, inferred from ctor, so an adapter keeps
// that type unexported and is still registered from another package. The
// registry decodes a configuration subtree into it — see
// [WithTransportSettings] — before ctor runs.
//
// ctor is given both halves of what a transport needs: ms, the MODULE's
// resolved settings (the endpoint, the server name, the tool timeout), and as,
// the adapter's own. Its context is the one live when [Server.Run] resolves the
// transport, which matters because a listening transport binds a socket there.
//
// It is implemented over [registry.Registry.Register] — the storage, the
// duplicate-name check, the decode and the port check are the registry's, and
// this signature is what an adapter author and a composition root touch
// (design D8). [registry.Registry.Provide]'s typed handle is not returned,
// because it would have nothing to open: a transport is selected by name at
// run time from configuration, never held by the caller.
//
// It returns the registry's *[registry.DuplicateNameError] (matching
// [registry.ErrDuplicateName]) if the name is taken.
func RegisterTransport[S any](r *registry.Registry, name string,
	ctor func(ctx context.Context, ms Settings, as S) (Transport, error),
) error {
	if r == nil {
		return errNilRegistry
	}
	if ctor == nil {
		return fmt.Errorf("goga/mcp: register transport %q: nil constructor", name)
	}

	return r.Register(name, func(_ context.Context, as S) (transportFactory, error) {
		return func(ctx context.Context, ms Settings) (Transport, error) {
			return ctor(ctx, ms, as)
		}, nil
	})
}

// UnknownTransportError reports a transport name no adapter is registered for.
//
// It exists rather than passing the registry's own error through so that the
// message can name the fix. The overwhelmingly likely cause of an unknown name
// is not a bad name at all: it is a composition root that never called the
// adapter's Provide, and a message listing what IS registered plus the call
// that is missing turns that into a one-line fix.
type UnknownTransportError struct {
	// Name is the transport name that was asked for.
	Name string
	// Registered holds every name in the registry at the time of the lookup,
	// sorted. It is empty when the registry held nothing — and nil when there
	// was no registry at all.
	Registered []string
	// err is the registry error this one composes, when the lookup reached a
	// registry. It is nil when no registry was injected.
	err error
}

// Error implements error, naming what is registered and the call that is
// probably missing.
func (e *UnknownTransportError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "goga/mcp: no transport %q", e.Name)

	switch {
	case e.err == nil:
		b.WriteString(" (no transport registry is configured); did you forget " +
			"mcp.WithTransportRegistry(r) and httptransport.Provide(r) in the composition root?")
	case len(e.Registered) == 0:
		b.WriteString(" (the registry is empty); did you forget " +
			"httptransport.Provide(r) in the composition root?")
	default:
		fmt.Fprintf(&b, " (registered: %s); did you forget %s.Provide(r) in the composition root?",
			strings.Join(e.Registered, ", "), e.Name+"transport")
	}
	return b.String()
}

// Is reports whether target is [registry.ErrUnknownName], so that a caller can
// branch on the condition with errors.Is without naming this type — the
// registry's own sentinel, because this is the registry's condition reported in
// this module's words.
func (e *UnknownTransportError) Is(target error) bool { return target == registry.ErrUnknownName }

// Unwrap returns the registry error this one composes, or nil when the lookup
// never reached a registry.
func (e *UnknownTransportError) Unwrap() error { return e.err }

// resolveTransport selects the transport this server serves on.
//
// Resolution is an operation in its own right and gets a span, because "which
// transport did this process actually resolve" is an operational question with
// no other answer (design D6). The span belongs to this module and not to the
// registry: the registry is a leaf that carries no instrumentation, and giving
// it one would close a registry → telemetry → registry import cycle.
func (s *Server) resolveTransport(ctx context.Context) (_ Transport, err error) {
	ctx, end := s.instr.Start(ctx, "resolve", semconv.MCPTransport(s.s.transport))
	defer func() { end(err) }()

	if s.s.reg == nil {
		if s.s.transport != defaultTransport {
			return nil, &UnknownTransportError{Name: s.s.transport}
		}
		return stdioTransport{}, nil
	}

	factory, err := s.s.reg.Open[transportFactory](ctx, s.s.transport, s.s.raw)
	if err != nil {
		var unknown *registry.UnknownNameError
		if errors.As(err, &unknown) {
			// The default is the one name that works with no wiring at all, so
			// a registry carrying only http and sse still serves stdio.
			if s.s.transport == defaultTransport {
				return stdioTransport{}, nil
			}
			return nil, &UnknownTransportError{Name: unknown.Name, Registered: unknown.Registered, err: err}
		}
		return nil, err
	}

	return factory(ctx, s.s)
}

// stdioTransport is the in-tree default: newline-delimited JSON over stdin and
// stdout, which is what a locally launched MCP server speaks.
//
// It lives here rather than in an adapter package because it is the transport a
// server gets with no options and no registry. An adapter that needs neither
// configuration nor wiring is not an adapter; it is the default.
type stdioTransport struct{}

// Serve implements [Transport].
func (stdioTransport) Serve(ctx context.Context, srv *sdkmcp.Server) error {
	if err := srv.Run(ctx, &sdkmcp.StdioTransport{}); err != nil && !errors.Is(err, ctx.Err()) {
		return fmt.Errorf("goga/mcp: serving over stdio: %w", err)
	}
	return nil
}
