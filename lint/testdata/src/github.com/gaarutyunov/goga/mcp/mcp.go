// Package mcp is a fixture stub of goga's own MCP wrapper, and at the same time
// the fixture for the owner exemption: every registration below reaches the
// wrapped server directly, which is exactly what this subtree is for, so not one
// of them may be reported.
//
// It is not a model of the real goga/mcp — it declares only what the fixtures
// name.
package mcp

import (
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool, Resource, ResourceTemplate, Prompt and Handler are aliased here so a
// fixture can write the bypass in a file that does NOT import the SDK. That is
// the point they exist to make, and it is about the analyzer rather than about
// goga/mcp: the method half of the rule must not gate on the import block,
// because whether the SDK's types are spelled through their own package, an
// alias, a local type or a helper's return value is a property of how the file
// is written, not of what it does. Gating on the import would make the rule
// depend on the spelling.
//
// The real goga/mcp does not alias these today — its registration API is
// name/desc/func-shaped, so the correct path needs no SDK types at all. This is
// a fixture stub, not a model of it.
type (
	// Tool describes a tool to the client.
	Tool = sdkmcp.Tool
	// Resource describes a resource to the client.
	Resource = sdkmcp.Resource
	// ResourceTemplate describes a family of resources to the client.
	ResourceTemplate = sdkmcp.ResourceTemplate
	// Prompt describes a prompt to the client.
	Prompt = sdkmcp.Prompt
	// Handler runs a registration.
	Handler = sdkmcp.Handler
)

// Server is goga's wrapper. The SDK server is an unexported field, which is the
// structural half of "AddTool is the only path to the wrapped server": there is
// no way to hold one except through New, and no way to reach it except SDK().
type Server struct {
	sdk *sdkmcp.Server
}

// New builds the wrapper. It is the only constructor.
func New() *Server { return &Server{sdk: &sdkmcp.Server{}} }

// SDK returns the wrapped server: the escape hatch, for the SDK surface this
// wrapper does not cover.
func (s *Server) SDK() *sdkmcp.Server { return s.sdk }

// Handler mounts the server on goga/serve.
func (s *Server) Handler() http.Handler { return http.NotFoundHandler() }

// AddTool is the wrapper's registration: the path that attaches the span, the
// timeout and the panic recovery.
func AddTool(s *Server, tool *sdkmcp.Tool, handler sdkmcp.Handler) {
	// The wrapper registering on the server it wraps. This is the call the rule
	// would fire on anywhere else, and the owner exemption is what keeps it
	// quiet here.
	s.SDK().AddTool(tool, handler)
}

// AddResource is the wrapper's resource registration.
func AddResource(s *Server, resource *sdkmcp.Resource, handler sdkmcp.Handler) {
	sdk := s.SDK()
	sdk.AddResource(resource, handler)
	sdk.AddResourceTemplate(&sdkmcp.ResourceTemplate{}, handler)
}

// AddPrompt is the wrapper's prompt registration.
func AddPrompt(s *Server, prompt *sdkmcp.Prompt, handler sdkmcp.Handler) {
	sdkmcp.AddTool(s.SDK(), &sdkmcp.Tool{}, func(struct{}) struct{} { return struct{}{} })
	s.SDK().AddPrompt(prompt, handler)
}

// Client is the other half of the shipped API, and it has an SDK() of its own —
// which is why a fixture needs one. It returns a session rather than a server,
// so nothing on it is in the rule's trigger set.
type Client struct{}

// SDK returns the wrapped client session.
func (c *Client) SDK() *sdkmcp.ClientSession { return &sdkmcp.ClientSession{} }
