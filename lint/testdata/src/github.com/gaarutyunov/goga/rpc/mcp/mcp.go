// Package mcp is the second lookalike: its last segment IS the owner's, but its
// POSITION is not. The exemption is "the package that owns the wrapped server",
// which is a place in the tree, so this one is ordinary project code.
package mcp

import (
	gogamcp "github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register is ordinary project code, and gets checked like any other.
func Register(s *gogamcp.Server, h sdkmcp.Handler) {
	s.SDK().AddPrompt(&sdkmcp.Prompt{Name: "summarise"}, h) // want `registering a prompt on the server returned by SDK\(\) routes around goga/mcp`
}
