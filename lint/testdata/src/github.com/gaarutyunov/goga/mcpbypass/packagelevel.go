// The SDK's package-level generic AddTool is the spelling its own documentation
// leads with, so a rule covering only the method form would miss the common
// case rather than an exotic one. Both instantiations appear here: inferred,
// and written out.
package mcpbypass

import (
	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Inferred lets the compiler infer the type arguments.
func Inferred(s *mcp.Server) {
	sdkmcp.AddTool(s.SDK(), &sdkmcp.Tool{Name: "search"}, func(int) string { return "" }) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}

// Explicit writes the type arguments out, which puts an IndexListExpr between
// the call and its selector. A rule that did not unwrap it would report the
// first and not the second.
func Explicit(s *mcp.Server) {
	sdkmcp.AddTool[int, string](s.SDK(), &sdkmcp.Tool{Name: "search"}, func(int) string { return "" }) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}
