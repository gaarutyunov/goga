// Package mcp is a fixture stub of the Model Context Protocol SDK's server,
// declaring only the surface gogamcp's fixtures name. analysistest loads its
// fixture tree in GOPATH mode, so a third-party import has to exist here rather
// than in go.mod; it is not a model of the real package, and the real package
// is what TestMCPTriggersExistInSDK checks the rule's trigger set against.
package mcp

// Server is the SDK's own server — the one goga/mcp wraps and SDK() hands back.
type Server struct{}

// Tool describes a tool to the client.
type Tool struct {
	// Name is how the client asks for it.
	Name string
}

// Resource describes a resource to the client.
type Resource struct {
	// URI is how the client asks for it.
	URI string
}

// ResourceTemplate describes a family of resources to the client.
type ResourceTemplate struct {
	// URITemplate is the pattern the client expands.
	URITemplate string
}

// Prompt describes a prompt to the client.
type Prompt struct {
	// Name is how the client asks for it.
	Name string
}

// Handler is the stub's stand-in for the SDK's several handler signatures. The
// rule matches on the function being called, not on what it is handed, so one
// shape is enough here.
type Handler func()

// Middleware wraps the SDK's receiving or sending pipeline.
type Middleware func(Handler) Handler

// AddTool registers a tool. It is the method form of the trigger.
func (s *Server) AddTool(_ *Tool, _ Handler) {}

// AddResource registers a resource.
func (s *Server) AddResource(_ *Resource, _ Handler) {}

// AddResourceTemplate registers a resource template.
func (s *Server) AddResourceTemplate(_ *ResourceTemplate, _ Handler) {}

// AddPrompt registers a prompt.
func (s *Server) AddPrompt(_ *Prompt, _ Handler) {}

// AddReceivingMiddleware installs middleware. It is a near miss: it adds
// behaviour around the wrapper rather than escaping it, so the rule leaves it
// alone.
func (s *Server) AddReceivingMiddleware(_ ...Middleware) {}

// RemoveTools unregisters tools. Also a near miss: a tool that disappears is a
// visible failure, not the silent one the rule is about.
func (s *Server) RemoveTools(_ ...string) {}

// Connected reports whether a client is attached. It is the ordinary,
// legitimate use of the escape hatch.
func (s *Server) Connected() bool { return false }

// AddTool is the SDK's package-level generic registration function, and the
// spelling its own documentation leads with. The real one derives the tool's
// JSON schema from In and Out; the stub only has to have the shape.
func AddTool[In, Out any](_ *Server, _ *Tool, _ func(In) Out) {}

// ClientSession is what the client half's SDK() hands back. It has no
// registration surface at all, which is the whole reason a fixture names it:
// the rule's shape `x.SDK().Something(…)` occurs on the client too, and stays
// quiet there.
type ClientSession struct{}

// CallTool invokes a tool on the server.
func (s *ClientSession) CallTool(_ string) error { return nil }
