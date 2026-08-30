// Package mcputil is the first of the two lookalikes that decide how the owner
// exemption is written: its name merely STARTS WITH the owner's. A segment
// match, or a prefix match on the name rather than on the path, would silence
// it — and exempting a package because of what it is called is how a rule
// quietly stops covering the code it was written for.
package mcputil

import (
	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register is ordinary project code, and gets checked like any other.
func Register(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddTool(&sdkmcp.Tool{Name: "search"}, h) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}
