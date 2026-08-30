// Package mcplike is a different module's package declaring a type with an
// SDK() accessor of its own. It exists so a fixture can write the rule's exact
// shape — `x.SDK().Something(…)` — around something that is not goga's wrapper
// at all.
package mcplike

// Client is an unrelated type that happens to expose a vendor SDK.
type Client struct{}

// Vendor is what its SDK() hands back.
type Vendor struct {
	// Region is a field, so a fixture can read one off the result.
	Region string
}

// SDK returns the vendor handle. Same name, same arity, different everything.
func (c *Client) SDK() *Vendor { return &Vendor{} }

// Ping is an unrelated method on the result.
func (v *Vendor) Ping() error { return nil }

// Factory is the second lookalike: its accessor is also called SDK, and it
// TAKES AN ARGUMENT. A configured accessor is a different thing from goga's
// zero-argument escape hatch, and the arity is the only syntax that says so.
type Factory struct{}

// SDK returns a per-region handle.
func (f *Factory) SDK(_ string) *Prompts { return &Prompts{} }

// Prompts is what the configured accessor hands back, and it declares a method
// whose name is in the rule's trigger set.
type Prompts struct{}

// AddPrompt registers a prompt with the vendor, which is not goga's wrapper.
func (p *Prompts) AddPrompt(_ string, _ func()) {}
