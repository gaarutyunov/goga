package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga/registry"
)

// TestTheDefaultTransportNeedsNoWiring is the reason stdio is not an adapter
// package. A server built with no options at all has to run, or the module's
// simplest case — a locally launched MCP server, which is most of them — pays
// for a registry it has nothing to put in.
//
// It is an internal test because the property is about what resolution returns,
// and driving it through Run would mean serving on the test process's own
// stdin.
func TestTheDefaultTransportNeedsNoWiring(t *testing.T) {
	t.Parallel()

	s, err := New()
	require.NoError(t, err)

	tr, err := s.resolveTransport(t.Context())
	require.NoError(t, err)
	assert.IsType(t, stdioTransport{}, tr)
}

// TestTheDefaultSurvivesARegistryThatDoesNotHoldIt covers the arrangement a
// composition root actually produces: a registry carrying http and sse, and a
// deployment that still runs over stdio. Failing there would make injecting a
// registry a breaking change for every stdio deployment.
func TestTheDefaultSurvivesARegistryThatDoesNotHoldIt(t *testing.T) {
	t.Parallel()

	reg := registry.New(func(registry.Settings, any) error { return nil })
	s, err := New(WithTransportRegistry(reg))
	require.NoError(t, err)

	tr, err := s.resolveTransport(t.Context())
	require.NoError(t, err)
	assert.IsType(t, stdioTransport{}, tr)
}

// TestIsTextualPicksTheProtocolsTextHalf pins the rule that decides whether a
// resource's bytes arrive as text or as a base64 blob. Getting it wrong is not
// a protocol error and is unreadable everywhere a human looks.
func TestIsTextualPicksTheProtocolsTextHalf(t *testing.T) {
	t.Parallel()

	textual := []string{
		"text/plain",
		"text/plain; charset=utf-8",
		"TEXT/Markdown",
		"application/json",
		"application/vnd.api+json",
		"application/xml",
		"image/svg+xml",
		"application/yaml",
	}
	for _, mimeType := range textual {
		assert.True(t, isTextual(mimeType), "%q is text", mimeType)
	}

	binary := []string{"", "image/png", "application/octet-stream", "application/pdf", "jsonish"}
	for _, mimeType := range binary {
		assert.False(t, isTextual(mimeType), "%q is not text", mimeType)
	}
}

// TestPromptArgumentsAreDerivedFromTheStruct covers the derivation the exported
// test cannot reach for the shapes that matter — an unexported field, a `json:"-"`
// field, and a non-struct input.
func TestPromptArgumentsAreDerivedFromTheStruct(t *testing.T) {
	t.Parallel()

	type args struct {
		Topic    string  `json:"topic"`
		Tone     *string `json:"tone"`
		Length   string  `json:"length,omitempty"`
		Internal string  `json:"-"`
		Untagged string
		secret   string
	}

	// The unexported field is written once here so that it is a field with a
	// use, and the assertion below is that the derivation skips it anyway.
	_ = args{secret: "not an argument"}

	assert.Equal(t, []*sdkmcp.PromptArgument{
		{Name: "topic", Required: true},
		{Name: "tone"},
		{Name: "length"},
		{Name: "Untagged", Required: true},
	}, promptArguments[args]())

	assert.Nil(t, promptArguments[string](), "a prompt whose input is not a struct declares no arguments")
}

// TestPromptArgumentsDecodeThroughTheJSONTag proves the two halves stay in
// step: the name the derivation declares is the name the decoding looks for.
func TestPromptArgumentsDecodeThroughTheJSONTag(t *testing.T) {
	t.Parallel()

	type args struct {
		Topic string `json:"topic"`
		Count int    `json:"count"`
	}

	in, err := decodePromptArguments[args](map[string]string{"topic": "otters", "count": "3"})
	require.NoError(t, err)
	assert.Equal(t, args{Topic: "otters", Count: 3}, in,
		"a string on the wire reaches a non-string field through its JSON form")

	empty, err := decodePromptArguments[args](nil)
	require.NoError(t, err)
	assert.Equal(t, args{}, empty)

	_, err = decodePromptArguments[args](map[string]string{"count": "not a number"})
	require.Error(t, err, "an argument that cannot be decoded names itself")
}

// TestMetaCarrierIgnoresANonStringValue pins the one thing _meta can hold that
// a text-map carrier cannot: the protocol lets _meta carry any JSON value, and
// somebody else's object under a key we read is data, not a malformed trace
// context.
func TestMetaCarrierIgnoresANonStringValue(t *testing.T) {
	t.Parallel()

	c := metaCarrier(sdkmcp.Meta{"traceparent": 42, "tracestate": "a=b"})

	assert.Empty(t, c.Get("traceparent"))
	assert.Equal(t, "a=b", c.Get("tracestate"))
	assert.Empty(t, c.Get("absent"))
	assert.ElementsMatch(t, []string{"traceparent", "tracestate"}, c.Keys())
}
