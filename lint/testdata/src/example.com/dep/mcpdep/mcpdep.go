// Package mcpdep is a dependency in another module entirely, registering
// underneath a wrapper of its own. Its registrations are not goga's to police,
// which is what -module-prefix is for.
package mcpdep

import sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

// Wrapper stands in for the dependency's own wrapper.
type Wrapper struct {
	sdk *sdkmcp.Server
}

// SDK returns the wrapped server.
func (w *Wrapper) SDK() *sdkmcp.Server { return w.sdk }

// Register registers straight through it.
func (w *Wrapper) Register(h sdkmcp.Handler) {
	w.SDK().AddTool(&sdkmcp.Tool{Name: "search"}, h)
	sdkmcp.AddTool(w.SDK(), &sdkmcp.Tool{Name: "other"}, func(int) string { return "" })
}
