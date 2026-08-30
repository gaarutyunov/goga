package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/propagation"
)

// tracePropagator carries trace context between an MCP client and an MCP
// server.
//
// # Why the propagator is pinned rather than read from the global
//
// MCP defines no header for trace context, so goga's house convention is a W3C
// traceparent in the request's _meta. That convention only works if both ends
// agree on the key AND on the format, and OpenTelemetry's global propagator is
// exactly the thing a program is free to change: a service configured for B3
// would write b3 keys into _meta, a peer would look for traceparent, find
// nothing, and every trace would break at the MCP hop with no error anywhere.
//
// So the W3C propagator is the convention, written down here. A program that
// wants B3 on the wire still gets it on its HTTP calls; what crosses an MCP
// _meta is traceparent, always.
var tracePropagator propagation.TextMapPropagator = propagation.TraceContext{}

// metaCarrier adapts an MCP _meta map to the [propagation.TextMapCarrier] the
// propagator writes through.
//
// _meta is a map[string]any because the protocol lets it carry anything;
// traceparent is a string. Get therefore ignores a value that is not a string
// rather than rendering it, since a non-string under the traceparent key is
// somebody else's data and not a malformed trace context.
type metaCarrier sdkmcp.Meta

// Get implements [propagation.TextMapCarrier].
func (c metaCarrier) Get(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// Set implements [propagation.TextMapCarrier].
func (c metaCarrier) Set(key, value string) { c[key] = value }

// Keys implements [propagation.TextMapCarrier].
func (c metaCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// extractTraceContext returns ctx with the caller's trace context restored from
// a request's _meta, or ctx unchanged when the caller sent none.
//
// A caller that sent nothing is the ordinary case, not an error: most MCP
// clients are not goga clients. The span the wrapper starts is then a root
// span, which is exactly what it should be.
func extractTraceContext(ctx context.Context, meta sdkmcp.Meta) context.Context {
	if len(meta) == 0 {
		return ctx
	}
	return tracePropagator.Extract(ctx, metaCarrier(meta))
}

// injectTraceContext returns the _meta a client should send with a request,
// carrying the trace context in ctx.
//
// It returns nil when ctx carries no span context worth propagating, so that a
// client outside a trace sends no _meta at all rather than an empty object.
func injectTraceContext(ctx context.Context) sdkmcp.Meta {
	meta := make(sdkmcp.Meta, 1)
	tracePropagator.Inject(ctx, metaCarrier(meta))
	if len(meta) == 0 {
		return nil
	}
	return meta
}
