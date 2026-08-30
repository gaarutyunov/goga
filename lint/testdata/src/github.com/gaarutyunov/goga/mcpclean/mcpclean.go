// Package mcpclean is the fixture that has to stay silent. Every declaration in
// it is a near miss: something the rule could plausibly fire on and must not.
//
// analysistest fails on an unexpected diagnostic as well as on a missing one,
// so this file is what proves gogamcp is a rule about REGISTERING through the
// escape hatch rather than a ban on reaching for it.
package mcpclean

import (
	"net/http"

	"github.com/gaarutyunov/goga/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Correct is the shape the rule is asking for: register through goga/mcp, which
// is what attaches the span, the timeout and the panic recovery.
func Correct(h sdkmcp.Handler) http.Handler {
	s := mcp.New()
	mcp.AddTool(s, &sdkmcp.Tool{Name: "search"}, h)

	return s.Handler()
}

// EscapeHatch is the case the rule exists to leave alone. Reaching for SDK() is
// legitimate — the wrapper does not cover the SDK's whole surface — and only
// registering through it is the defect.
func EscapeHatch(s *mcp.Server) bool {
	return s.SDK().Connected()
}

// Middleware is the near miss closest to the trigger set: it is spelled Add…,
// it is called on the escape hatch, and it ADDS behaviour around the wrapper
// rather than escaping it, which is one of the things the hatch is for.
func Middleware(s *mcp.Server, m sdkmcp.Middleware) {
	s.SDK().AddReceivingMiddleware(m)
}

// Unregister is the other Add…-adjacent near miss. Removing a registration is a
// visible failure — the client stops seeing the tool — not the silent one this
// rule is about.
func Unregister(s *mcp.Server) {
	s.SDK().RemoveTools("search")
}

// Passed hands the raw server somewhere else without registering anything on
// it. The rule follows a local binding inside one body, not a server that
// travels; this is the near miss on that side of the line.
func Passed(s *mcp.Server) *sdkmcp.Server {
	sdk := s.SDK()
	_ = sdk

	return s.SDK()
}

// WrapperRegistration is goga's own AddTool called from ordinary project code.
// Its first argument is the wrapper, not the escape hatch, so a rule matching
// the function name alone would report the very call it is asking for.
func WrapperRegistration(s *mcp.Server, h sdkmcp.Handler) {
	mcp.AddTool(s, &sdkmcp.Tool{Name: "search"}, h)
	mcp.AddResource(s, &sdkmcp.Resource{URI: "file:///x"}, h)
	mcp.AddPrompt(s, &sdkmcp.Prompt{Name: "summarise"}, h)
}

// MethodValue references the trigger method without calling it. A reference is
// not a registration, and construction — here, registration — is the defect.
func MethodValue(s *mcp.Server) func(*sdkmcp.Tool, sdkmcp.Handler) {
	return s.SDK().AddTool
}

// SecondServer builds a raw SDK server of its own and registers on it, in both
// spellings. That is a real defect, and it is DEPGUARD's rather than this
// rule's: the `mcp-owns-the-protocol-sdk` ban is what stops a project holding
// an SDK server the wrapper never saw, which is also why no shipped package
// could contain this file. This rule is about the server the wrapper DID see,
// so it stays quiet here — and the fixture is what pins that division of
// labour, since a rule that also fired on this one would be reporting the same
// defect twice while still missing neither.
func SecondServer(h sdkmcp.Handler) *sdkmcp.Server {
	raw := &sdkmcp.Server{}
	raw.AddTool(&sdkmcp.Tool{Name: "search"}, h)
	sdkmcp.AddTool(raw, &sdkmcp.Tool{Name: "other"}, func(int) string { return "" })

	return raw
}

// ClientEscapeHatch is the near miss the shipped API adds for free: goga/mcp's
// client has an SDK() of its own, so the rule's exact shape occurs on the
// client half too. It stays quiet because the trigger set is the SERVER's
// registration surface — a session publishes nothing to anybody.
func ClientEscapeHatch(c *mcp.Client) error {
	return c.SDK().CallTool("search")
}
