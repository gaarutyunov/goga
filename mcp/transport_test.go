package mcp_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga/mcp"
	"github.com/gaarutyunov/goga/mcp/httptransport"
	"github.com/gaarutyunov/goga/mcp/ssetransport"
	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/serve"
)

// TestUnknownTransportNamesWhatIsRegistered is checklist item 6.7's diagnostic
// half. The overwhelmingly likely cause of an unknown transport name is not a
// typo at all — it is a composition root that never called the adapter's
// Provide — so the message has to name both what IS registered and the call
// that is missing, or the reader is left to guess which of the two it was.
func TestUnknownTransportNamesWhatIsRegistered(t *testing.T) {
	t.Parallel()

	reg := registry.New(testDecode)
	require.NoError(t, httptransport.Provide(reg))
	require.NoError(t, ssetransport.Provide(reg))

	server, err := mcp.New(
		mcp.WithTransportRegistry(reg),
		mcp.WithTransport("htp"),
		mcp.WithEndpoint("127.0.0.1:0"))
	require.NoError(t, err)

	err = server.Run(t.Context())
	require.Error(t, err)

	assert.ErrorIs(t, err, registry.ErrUnknownName,
		"the module's error still answers the registry's condition")

	var unknown *mcp.UnknownTransportError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, "htp", unknown.Name)
	assert.Equal(t, []string{"http", "sse"}, unknown.Registered)

	assert.Contains(t, err.Error(), `no transport "htp"`)
	assert.Contains(t, err.Error(), "registered: http, sse")
	assert.Contains(t, err.Error(), "Provide(r)", "the message names the call that is probably missing")
}

// TestATransportNameWithNoRegistryPointsAtTheOption covers the other way the
// wiring goes wrong: the adapters exist, and nothing injected the registry they
// were attached to.
func TestATransportNameWithNoRegistryPointsAtTheOption(t *testing.T) {
	t.Parallel()

	server, err := mcp.New(mcp.WithTransport("http"), mcp.WithEndpoint("127.0.0.1:0"))
	require.NoError(t, err)

	err = server.Run(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, registry.ErrUnknownName)
	assert.Contains(t, err.Error(), "no transport registry is configured")
	assert.Contains(t, err.Error(), "mcp.WithTransportRegistry(r)")
}

// TestATransportRegisteredTwiceIsReported leans on the registry's own
// duplicate-name check rather than re-implementing one, which is what "the
// registry is the mechanism under each module's surface" means in practice.
func TestATransportRegisteredTwiceIsReported(t *testing.T) {
	t.Parallel()

	reg := registry.New(testDecode)
	require.NoError(t, httptransport.Provide(reg))

	err := httptransport.Provide(reg)
	require.Error(t, err)
	assert.ErrorIs(t, err, registry.ErrDuplicateName)
}

// TestRegisterTransportRejectsBadArguments covers the two wiring mistakes that
// would otherwise surface as a nil dereference much later.
func TestRegisterTransportRejectsBadArguments(t *testing.T) {
	t.Parallel()

	ctor := func(context.Context, mcp.Settings, memorySettings) (mcp.Transport, error) { return nil, nil }

	assert.Error(t, mcp.RegisterTransport(nil, "memory", ctor))
	assert.Error(t, mcp.RegisterTransport[memorySettings](registry.New(testDecode), "memory", nil))
}

// TestTheHTTPTransportNeedsAnEndpoint proves the module's Settings accessor is
// what a transport actually reads: with neither its own configuration nor
// mcp.WithEndpoint there is no address to bind, and that has to be a startup
// error naming the transport rather than a listener failing later.
func TestTheHTTPTransportNeedsAnEndpoint(t *testing.T) {
	t.Parallel()

	reg := registry.New(testDecode)
	require.NoError(t, httptransport.Provide(reg))

	server, err := mcp.New(mcp.WithTransportRegistry(reg), mcp.WithTransport(httptransport.Name))
	require.NoError(t, err)

	err = server.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/mcp/httptransport")
	assert.Contains(t, err.Error(), "no endpoint")
}

// TestTheSSETransportNeedsAnEndpoint is the same property for the second
// adapter, because "the endpoint comes from the module's settings" is a claim
// about the port and not about one implementation of it.
func TestTheSSETransportNeedsAnEndpoint(t *testing.T) {
	t.Parallel()

	reg := registry.New(testDecode)
	require.NoError(t, ssetransport.Provide(reg))

	server, err := mcp.New(mcp.WithTransportRegistry(reg), mcp.WithTransport(ssetransport.Name))
	require.NoError(t, err)

	err = server.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goga/mcp/ssetransport")
	assert.Contains(t, err.Error(), "no endpoint")
}

// TestHandlerMountsOnAServeServer is checklist item 6.8, and it is run rather
// than merely compiled: the claim is that a process can expose HTTP and MCP on
// ONE port, and the only way to know is to call a tool over the port the
// application's own routes are on.
func TestHandlerMountsOnAServeServer(t *testing.T) {
	rec := newSpanRecorder(t)

	server, err := mcp.New(mcp.WithName("mounted"))
	require.NoError(t, err)
	mcp.AddTool(server, "greet", "greets",
		func(_ context.Context, in callArgs) (callResult, error) {
			return callResult{Greeting: "hello " + in.Name}, nil
		})

	mux := http.NewServeMux()
	mux.Handle("/mcp", server.Handler())
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})

	addr := freeAddr(t)
	httpServer, err := serve.New(t.Context(), mux, serve.WithAddr(addr))
	require.NoError(t, err)

	// Not t.Context(): that is cancelled just BEFORE the cleanups run, which
	// would tear the listener down under the client's own Close.
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- httpServer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})

	base := "http://" + addr
	requireServing(t, base+"/livez")

	client, err := mcp.Connect(t.Context(),
		mcp.WithClientTransport(&sdkmcp.StreamableClientTransport{Endpoint: base + "/mcp"}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })

	res, err := client.CallTool(t.Context(), "greet", callArgs{Name: "world"})
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", resultText(res))
	assert.Contains(t, resultText(res), "hello world")

	assert.Contains(t, rec.names(), "goga.mcp.tool",
		"a mounted server is instrumented exactly as a standalone one is")
}

// freeAddr reserves a loopback port and hands back the address, the way
// serve/servetest does: binding and closing is the only way to learn a port the
// operating system will still give back a moment later.
func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// requireServing waits for a URL to answer, so that a test never races the
// listener it just started.
func requireServing(t *testing.T, url string) {
	t.Helper()

	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "the server never began serving %s", url)
}
