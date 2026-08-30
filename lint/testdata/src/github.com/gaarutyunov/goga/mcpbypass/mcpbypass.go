// Package mcpbypass is goga's own library code registering underneath the
// wrapper. It is the violating fixture: every shape in the trigger set, each
// reported once.
package mcpbypass

import (
	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool takes the obvious spelling: the registration chained straight onto the
// escape hatch.
func Tool(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddTool(&sdkmcp.Tool{Name: "search"}, h) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}

// Resource takes the resource spelling, which a rule naming only AddTool would
// miss.
func Resource(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddResource(&sdkmcp.Resource{URI: "file:///x"}, h) // want `registering a resource on the server returned by SDK\(\) routes around goga/mcp`
}

// Template takes the resource-template spelling.
func Template(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddResourceTemplate(&sdkmcp.ResourceTemplate{URITemplate: "file:///{p}"}, h) // want `registering a resource template on the server returned by SDK\(\) routes around goga/mcp`
}

// Prompt takes the prompt spelling.
func Prompt(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddPrompt(&sdkmcp.Prompt{Name: "summarise"}, h) // want `registering a prompt on the server returned by SDK\(\) routes around goga/mcp`
}
