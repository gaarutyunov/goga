// A different module's type, with an SDK() accessor of its own. Every
// expression below reads exactly like the escape hatch and is not it, which is
// what proves the rule is about the METHOD BEING CALLED on the result rather
// than about the shape `x.SDK()` appearing at all.
//
// The stated limit sits here too, and is deliberately not papered over: what
// separates this from the defect is that the vendor handle has no AddTool. A
// lookalike that did declare an identically-named registration method would be
// reported, because telling the two apart needs the type information this
// plugin does not load. Rarer than the shapes the rule does catch, and the
// direction that fails loud rather than silent.
package mcpclean

import "example.com/mcplike"

// Lookalike calls an unrelated method on the result of an unrelated SDK().
func Lookalike(c *mcplike.Client) error {
	return c.SDK().Ping()
}

// LookalikeField reads a field off it, which is the other thing a caller does
// with an accessor's result.
func LookalikeField(c *mcplike.Client) string {
	return c.SDK().Region
}

// LookalikeBound binds it to a local first, which is the shape the rule follows
// for goga's own wrapper.
func LookalikeBound(c *mcplike.Client) error {
	sdk := c.SDK()

	return sdk.Ping()
}

// LookalikeArgs is the near miss the zero-argument check exists for: the
// accessor is spelled SDK, the method on its result is spelled AddPrompt, and
// the two together are still not the escape hatch — a configured accessor
// returns something a caller chose, not the one server the wrapper holds. The
// arity is the only syntax that separates them.
func LookalikeArgs(f *mcplike.Factory) {
	f.SDK("eu").AddPrompt("summarise", func() {})
}
