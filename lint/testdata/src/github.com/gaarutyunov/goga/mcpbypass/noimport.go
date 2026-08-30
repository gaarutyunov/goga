// The case that decides whether the method half of the rule may gate on the
// import block: this file does NOT import the SDK. Nothing forces a bypass to —
// the types can arrive through an alias, a local type or a helper's return
// value, and under the depguard rule confining that import path to goga/mcp
// they had better. A rule that skipped files without the SDK import would be
// keyed on how the file spells its types rather than on what it does, and would
// report nothing here.
package mcpbypass

import "github.com/gaarutyunov/goga/mcp"

// Aliased registers with the wrapper's re-exported types and no SDK import.
func Aliased(s *mcp.Server, h mcp.Handler) {
	s.SDK().AddTool(&mcp.Tool{Name: "search"}, h) // want `registering a tool on the server returned by SDK\(\) routes around goga/mcp`
}

// AliasedBound does the same through a local binding.
func AliasedBound(s *mcp.Server, h mcp.Handler) {
	sdk := s.SDK()
	sdk.AddResource(&mcp.Resource{URI: "file:///x"}, h) // want `registering a resource on the server returned by SDK\(\) routes around goga/mcp`
}
