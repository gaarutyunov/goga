// Binding the escape hatch to a local first is the one-line edit that would
// otherwise turn this rule into a check on formatting, so the rule follows it
// within a function body.
package mcpbypass

import (
	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bound registers through a local bound to SDK(), twice, which is what an
// author with more than one tool would naturally write.
func Bound(s *mcp.Server, h sdkmcp.Handler) {
	sdk := s.SDK()
	sdk.AddTool(&sdkmcp.Tool{Name: "search"}, h)  // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
	sdk.AddPrompt(&sdkmcp.Prompt{Name: "sum"}, h) // want `registering a prompt on the server returned by SDK\(\) routes around goga/mcp`
}

// BoundToPackageForm passes the bound local to the package-level form.
func BoundToPackageForm(s *mcp.Server) {
	sdk := s.SDK()
	sdkmcp.AddTool(sdk, &sdkmcp.Tool{Name: "search"}, func(int) string { return "" }) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}

// BoundInClosure pins that a body's bindings reach the closures inside it,
// which is where a registration loop would be written.
func BoundInClosure(s *mcp.Server, h sdkmcp.Handler) func() {
	sdk := s.SDK()

	return func() {
		sdk.AddTool(&sdkmcp.Tool{Name: "search"}, h) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
	}
}
