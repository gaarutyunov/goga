// A test file is NOT exempt, by decision. A tool registered on the raw server
// in a test is a tool whose timeout and panic recovery the test never
// exercises: it passes while proving nothing about the path that ships.
package mcpbypass

import (
	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterForTest is the fixture's stand-in for a test helper.
func RegisterForTest(s *mcp.Server, h sdkmcp.Handler) {
	s.SDK().AddTool(&sdkmcp.Tool{Name: "fixture"}, h) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}
