package mcp_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gaarutyunov/goga/mcp"
	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/semconv"
)

// memoryTransportName is the adapter name the harness registers its in-memory
// transport under. It is not one of the three shipped names on purpose: a test
// that reached for "stdio" would be exercising the built-in fallback rather
// than the registry.
const memoryTransportName = "memory"

// memorySettings is the in-memory transport's own settings type. It is empty,
// which is a legitimate shape — the registry skips the decoder entirely when
// there is nothing to decode.
type memorySettings struct{}

// memoryTransport serves one MCP server over an in-memory pipe, so that a test
// exercises the real client, the real server and the real wire encoding without
// a socket.
type memoryTransport struct {
	transport sdkmcp.Transport
}

func (m memoryTransport) Serve(ctx context.Context, srv *sdkmcp.Server) error {
	return srv.Run(ctx, m.transport)
}

// testDecode is the decoder the harness gives its registry. goga/config
// supplies the real one; nothing in these tests has settings to decode, and the
// registry rejects a nil decoder because a registry that cannot decode is a
// wiring mistake rather than a runtime condition.
func testDecode(raw registry.Settings, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return fmt.Errorf("testDecode: nothing in these tests has settings, got %d keys for %T", len(raw), dst)
}

// harness is a goga MCP server wired to an in-memory transport, with a span
// recorder installed as the process tracer provider.
type harness struct {
	rec             *spanRecorder
	server          *mcp.Server
	clientTransport sdkmcp.Transport
}

// newHarness builds the server. Tools, resources and prompts are added to
// h.server before [harness.connect] runs it.
//
// It installs a global tracer provider, so a test using it must not call
// t.Parallel.
func newHarness(t *testing.T, opts ...mcp.Option) *harness {
	t.Helper()

	rec := newSpanRecorder(t)
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()

	reg := registry.New(testDecode)
	require.NoError(t, mcp.RegisterTransport(reg, memoryTransportName,
		func(context.Context, mcp.Settings, memorySettings) (mcp.Transport, error) {
			return memoryTransport{transport: serverTransport}, nil
		}))

	server, err := mcp.New(append([]mcp.Option{
		mcp.WithName("goga-mcp-test"),
		mcp.WithVersion("0.0.1-test"),
		mcp.WithTransportRegistry(reg),
		mcp.WithTransport(memoryTransportName),
	}, opts...)...)
	require.NoError(t, err)

	return &harness{rec: rec, server: server, clientTransport: clientTransport}
}

// connect runs the server and returns a connected client. Both are torn down
// when the test ends.
func (h *harness) connect(t *testing.T) *mcp.Client {
	t.Helper()

	// Not t.Context(): that is cancelled just BEFORE the cleanups run, which
	// would stop the server under the client's own Close.
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- h.server.Run(ctx) }()

	client, err := mcp.Connect(t.Context(), mcp.WithClientTransport(h.clientTransport))
	require.NoError(t, err)

	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		cancel()
		<-served
	})
	return client
}

// callArgs and callResult are the ordinary structs a tool is written in terms
// of. The SDK derives the tool's JSON schema from them.
type callArgs struct {
	Name string `json:"name"`
}

type callResult struct {
	Greeting string `json:"greeting"`
}

// resultText renders a tool result's content, which is where an in-band failure
// puts its message.
func resultText(res *sdkmcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// TestPanickingToolIsInBandAndItsSpanStillEnds is checklist item 6.5's headline
// test, and it is worth writing precisely because the behaviour is invisible
// when it works: a server whose panic guard was deleted passes every other test
// in this file and dies on the first nil dereference in production.
//
// All three halves of the claim are asserted separately. The call returns a
// result rather than propagating the panic; the result is an in-band failure
// rather than a protocol error; and the span the wrapper opened was ENDED, not
// merely opened — which is the half a recover with no deferred end would fail.
func TestPanickingToolIsInBandAndItsSpanStillEnds(t *testing.T) {
	h := newHarness(t)
	mcp.AddTool(h.server, "boom", "panics on purpose",
		func(context.Context, callArgs) (callResult, error) {
			// An out-of-range index rather than a bare panic call: the case the
			// guard exists for is a tool that did not mean to.
			var rows []callResult
			return rows[0], nil
		})

	client := h.connect(t)

	res, err := client.CallTool(t.Context(), "boom", callArgs{Name: "x"})
	require.NoError(t, err, "a panicking tool must not become a protocol error")
	require.NotNil(t, res)
	assert.True(t, res.IsError, "a panicking tool is reported in-band, with IsError set")
	assert.Contains(t, resultText(res), "panicked", "the in-band message says what happened")

	span, ok := h.rec.find("goga.mcp.tool")
	require.True(t, ok, "the wrapper's span was ended even though the tool panicked; recorded: %v", h.rec.names())
	assert.Equal(t, codes.Error, span.status, "the ended span records the failure")

	name, ok := span.attributeValue(semconv.MCPToolNameKey)
	require.True(t, ok, "the span names the tool that panicked")
	assert.Equal(t, "boom", name.AsString())
}

// TestToolErrorIsInBandNotAProtocolError is checklist item 6.4. The distinction
// is the whole point of the rule: an in-band failure is something the model on
// the other end can read and correct, and a protocol error is a server that
// looks broken.
func TestToolErrorIsInBandNotAProtocolError(t *testing.T) {
	h := newHarness(t)
	mcp.AddTool(h.server, "fails", "returns an ordinary error",
		func(context.Context, callArgs) (callResult, error) {
			return callResult{}, errors.New("the database said no")
		})

	client := h.connect(t)

	res, err := client.CallTool(t.Context(), "fails", callArgs{Name: "x"})
	require.NoError(t, err, "a tool failure is not a protocol error")
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(res), "the database said no", "the tool's own message reaches the caller")

	span, ok := h.rec.find("goga.mcp.tool")
	require.True(t, ok)
	assert.Equal(t, codes.Error, span.status)
}

// TestSuccessfulToolIsTracedAndReturnsItsOutput pins the ordinary path, so that
// the failure tests above are not the only thing holding the wrapper's shape.
func TestSuccessfulToolIsTracedAndReturnsItsOutput(t *testing.T) {
	h := newHarness(t)
	mcp.AddTool(h.server, "greet", "greets",
		func(_ context.Context, in callArgs) (callResult, error) {
			return callResult{Greeting: "hello " + in.Name}, nil
		})

	client := h.connect(t)

	res, err := client.CallTool(t.Context(), "greet", callArgs{Name: "world"})
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", resultText(res))
	assert.Contains(t, resultText(res), "hello world")

	span, ok := h.rec.find("goga.mcp.tool")
	require.True(t, ok)
	assert.Equal(t, codes.Ok, span.status)
}

// TestToolTimeoutFiresAndTheContextIsReleased is checklist item 6.5's other
// half. Two separate properties: the deadline the wrapper installed is the one
// the settings asked for and it actually fires, and the context is cancelled on
// the way out rather than leaked for the life of the process.
func TestToolTimeoutFiresAndTheContextIsReleased(t *testing.T) {
	const timeout = 80 * time.Millisecond

	h := newHarness(t, mcp.WithToolTimeout(timeout))

	var (
		toolCtx     context.Context
		deadline    time.Time
		hasDeadline bool
	)
	mcp.AddTool(h.server, "slow", "outlives its timeout",
		func(ctx context.Context, _ callArgs) (callResult, error) {
			toolCtx = ctx
			deadline, hasDeadline = ctx.Deadline()
			<-ctx.Done()
			return callResult{}, ctx.Err()
		})

	client := h.connect(t)

	start := time.Now()
	res, err := client.CallTool(t.Context(), "slow", callArgs{Name: "x"})
	elapsed := time.Since(start)

	require.NoError(t, err, "a timeout is a tool failure, reported in-band like any other")
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(res), context.DeadlineExceeded.Error())

	require.True(t, hasDeadline, "the wrapper installed a deadline")
	assert.WithinDuration(t, start.Add(timeout), deadline, 50*time.Millisecond,
		"the deadline is the one WithToolTimeout asked for")
	assert.Less(t, elapsed, 5*time.Second, "the call returned when the timeout fired, not when the client gave up")

	require.NotNil(t, toolCtx)
	assert.Error(t, toolCtx.Err(), "the timeout context is released once the call returns")
}

// TestPerToolTimeoutOverridesTheServerDefault covers the one knob a tool has
// over its own bound. There is deliberately no value that removes it.
func TestPerToolTimeoutOverridesTheServerDefault(t *testing.T) {
	const serverTimeout = 10 * time.Second
	const toolTimeout = 90 * time.Millisecond

	h := newHarness(t, mcp.WithToolTimeout(serverTimeout))

	var deadline time.Time
	mcp.AddTool(h.server, "quick", "has a tighter bound than the server",
		func(ctx context.Context, _ callArgs) (callResult, error) {
			var ok bool
			deadline, ok = ctx.Deadline()
			if !ok {
				return callResult{}, errors.New("no deadline")
			}
			return callResult{Greeting: "ok"}, nil
		},
		mcp.WithToolCallTimeout(toolTimeout))

	client := h.connect(t)

	start := time.Now()
	res, err := client.CallTool(t.Context(), "quick", callArgs{Name: "x"})
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", resultText(res))

	assert.WithinDuration(t, start.Add(toolTimeout), deadline, 50*time.Millisecond,
		"the per-tool timeout won over the server's")
}

// TestTraceContextSurvivesTheRoundTrip is checklist item 6.6, proved end to end
// rather than one side at a time.
//
// MCP defines no header for trace context, so goga's convention is a traceparent
// in the request's _meta — and a convention only works if both ends agree. The
// assertion is therefore the one that would catch a disagreement: a trace id
// minted in the caller's process is the trace id the server's tool span runs
// under. Asserting that the client wrote a key and, separately, that the server
// reads one would pass with the two halves using different formats.
func TestTraceContextSurvivesTheRoundTrip(t *testing.T) {
	h := newHarness(t)

	toolTraceIDs := make(chan trace.TraceID, 1)
	mcp.AddTool(h.server, "trace", "reports the trace it ran under",
		func(ctx context.Context, _ callArgs) (callResult, error) {
			toolTraceIDs <- trace.SpanContextFromContext(ctx).TraceID()
			return callResult{Greeting: "ok"}, nil
		})

	client := h.connect(t)

	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	callerTraceID := trace.SpanContextFromContext(ctx).TraceID()
	require.True(t, callerTraceID.IsValid())

	res, err := client.CallTool(ctx, "trace", callArgs{Name: "x"})
	caller.End()
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", resultText(res))

	select {
	case got := <-toolTraceIDs:
		assert.Equal(t, callerTraceID, got,
			"the caller's trace id reached the tool through traceparent in _meta")
	case <-time.After(time.Second):
		t.Fatal("the tool never ran")
	}

	span, ok := h.rec.find("goga.mcp.tool")
	require.True(t, ok)
	assert.Equal(t, callerTraceID, span.spanContext.TraceID(),
		"the server's span belongs to the caller's trace, not to a new one")
	assert.True(t, span.parent.IsValid(), "the server span has the client's span as its parent")
}

// TestACallerOutsideATraceStartsARootSpan is the other half of 6.6, and the
// case that is far more common: most MCP clients are not goga clients and send
// no _meta at all. That must be an ordinary root span rather than an error.
func TestACallerOutsideATraceStartsARootSpan(t *testing.T) {
	h := newHarness(t)
	mcp.AddTool(h.server, "greet", "greets",
		func(context.Context, callArgs) (callResult, error) {
			return callResult{Greeting: "hi"}, nil
		})

	client := h.connect(t)

	res, err := client.CallTool(context.WithoutCancel(t.Context()), "greet", callArgs{Name: "x"})
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", resultText(res))

	span, ok := h.rec.find("goga.mcp.tool")
	require.True(t, ok)
	assert.True(t, span.spanContext.TraceID().IsValid(), "a root span is still a span")
}

// TestResourceReadIsTraced is checklist item 6.3 for the surface that is easiest
// to forget: a resource read is an operation too.
func TestResourceReadIsTraced(t *testing.T) {
	h := newHarness(t)
	mcp.AddResource(h.server, "file:///notes.txt", "notes",
		func(context.Context, string) ([]byte, string, error) {
			return []byte("the contents"), "text/plain", nil
		})

	client := h.connect(t)

	res, err := client.ReadResource(t.Context(), "file:///notes.txt")
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
	assert.Equal(t, "the contents", res.Contents[0].Text, "a text MIME type arrives as text, not as a blob")

	span, ok := h.rec.find("goga.mcp.resource")
	require.True(t, ok, "recorded: %v", h.rec.names())
	assert.Equal(t, codes.Ok, span.status)

	uri, ok := span.attributeValue(semconv.MCPResourceURIKey)
	require.True(t, ok)
	assert.Equal(t, "file:///notes.txt", uri.AsString())
}

// TestPromptRenderIsTraced is checklist item 6.3 for the third surface.
func TestPromptRenderIsTraced(t *testing.T) {
	type promptArgs struct {
		Topic string `json:"topic"`
	}

	h := newHarness(t)
	mcp.AddPrompt(h.server, "summarise", "summarises a topic",
		func(_ context.Context, in promptArgs) ([]*sdkmcp.PromptMessage, error) {
			return []*sdkmcp.PromptMessage{{
				Role:    "user",
				Content: &sdkmcp.TextContent{Text: "summarise " + in.Topic},
			}}, nil
		})

	client := h.connect(t)

	res, err := client.GetPrompt(t.Context(), "summarise", map[string]string{"topic": "otters"})
	require.NoError(t, err)
	require.Len(t, res.Messages, 1)

	text, ok := res.Messages[0].Content.(*sdkmcp.TextContent)
	require.True(t, ok, "the prompt rendered text content, got %T", res.Messages[0].Content)
	assert.Equal(t, "summarise otters", text.Text, "the arguments reached the handler decoded")

	span, ok := h.rec.find("goga.mcp.prompt")
	require.True(t, ok, "recorded: %v", h.rec.names())
	assert.Equal(t, codes.Ok, span.status)

	name, ok := span.attributeValue(semconv.MCPPromptNameKey)
	require.True(t, ok)
	assert.Equal(t, "summarise", name.AsString())
}

// TestPromptArgumentsAreDerivedFromTheHandlersInput proves the listing a client
// reads is generated from the same struct the handler decodes, so the two
// cannot drift.
func TestPromptArgumentsAreDerivedFromTheHandlersInput(t *testing.T) {
	type promptArgs struct {
		Topic  string  `json:"topic"`
		Tone   *string `json:"tone"`
		Length string  `json:"length,omitempty"`
	}

	h := newHarness(t)
	mcp.AddPrompt(h.server, "summarise", "summarises a topic",
		func(context.Context, promptArgs) ([]*sdkmcp.PromptMessage, error) { return nil, nil })

	client := h.connect(t)

	listed, err := client.SDK().ListPrompts(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Prompts, 1)

	assert.Equal(t, []*sdkmcp.PromptArgument{
		{Name: "topic", Required: true},
		{Name: "tone"},
		{Name: "length"},
	}, listed.Prompts[0].Arguments)
}

// TestClientSpansAreSymmetricWithTheServers is checklist item 6.9: the consumer
// side is instrumented too, and by the same module.
func TestClientSpansAreSymmetricWithTheServers(t *testing.T) {
	h := newHarness(t)
	mcp.AddTool(h.server, "greet", "greets",
		func(context.Context, callArgs) (callResult, error) {
			return callResult{Greeting: "hi"}, nil
		})

	client := h.connect(t)

	_, err := client.CallTool(t.Context(), "greet", callArgs{Name: "x"})
	require.NoError(t, err)

	assert.Contains(t, h.rec.names(), "goga.mcp.client.connect")
	assert.Contains(t, h.rec.names(), "goga.mcp.client.tool")

	span, ok := h.rec.find("goga.mcp.client.tool")
	require.True(t, ok)
	name, ok := span.attributeValue(semconv.MCPToolNameKey)
	require.True(t, ok, "the client span names the tool it called")
	assert.Equal(t, "greet", name.AsString())
}

// TestResolvingTheTransportIsItsOwnSpan is design D6's adapter-resolution rule:
// "which transport did this process actually resolve" is an operational
// question, and the span is where it is answered.
func TestResolvingTheTransportIsItsOwnSpan(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	span, ok := h.rec.find("goga.mcp.resolve")
	require.True(t, ok, "recorded: %v", h.rec.names())

	name, ok := span.attributeValue(semconv.MCPTransportKey)
	require.True(t, ok)
	assert.Equal(t, memoryTransportName, name.AsString())
}

// TestNewRejectsBadOptions covers the option validation that keeps a bad value
// at the call site that supplied it.
func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	tests := map[string]mcp.Option{
		"an empty name":       mcp.WithName(""),
		"an empty version":    mcp.WithVersion(""),
		"an empty endpoint":   mcp.WithEndpoint(""),
		"an empty transport":  mcp.WithTransport(""),
		"a zero tool timeout": mcp.WithToolTimeout(0),
		"a negative timeout":  mcp.WithToolTimeout(-time.Second),
		"a nil telemetry":     mcp.WithTelemetry(nil),
		"a nil authenticator": mcp.WithAuthenticator(nil),
		"a nil registry":      mcp.WithTransportRegistry(nil),
	}

	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := mcp.New(opt)
			require.Error(t, err)
		})
	}
}

// TestSDKIsTheOnlyOtherRouteToTheWrappedServer pins checklist item 6.2's claim
// in the one way a test can: [mcp.Server]'s fields are unexported, so the only
// exported route to the SDK server is the escape hatch that is named like one.
//
// The rest of 6.2 is a compile-time property and needs no test — a package
// outside this one cannot name settings, cannot construct a Server literal, and
// has no accessor but SDK.
func TestSDKIsTheOnlyOtherRouteToTheWrappedServer(t *testing.T) {
	t.Parallel()

	server, err := mcp.New()
	require.NoError(t, err)
	require.NotNil(t, server.SDK())

	structType := reflect.TypeOf(*server)
	for i := range structType.NumField() {
		assert.False(t, structType.Field(i).IsExported(),
			"Server.%s is exported, which is a second route to the wrapped server",
			structType.Field(i).Name)
	}

	var exported []string
	pointerType := reflect.TypeOf(server)
	for i := range pointerType.NumMethod() {
		method := pointerType.Method(i)
		if method.Type.NumOut() == 1 && method.Type.Out(0) == reflect.TypeFor[*sdkmcp.Server]() {
			exported = append(exported, method.Name)
		}
	}
	assert.Equal(t, []string{"SDK"}, exported,
		"exactly one exported method hands back the wrapped SDK server")
}
