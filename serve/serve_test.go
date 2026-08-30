package serve_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga/serve"
	"github.com/gaarutyunov/goga/serve/servetest"
)

// fakeDriver is a driver.Server that binds nothing. It exists to test the parts
// of Run that are about the port and not about TCP: what address and handler
// the listener is given, what Run makes of the error it returns, and whether a
// cancelled context reaches Shutdown.
type fakeDriver struct {
	serveErr error

	started  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once

	mu        sync.Mutex
	addr      string
	handler   http.Handler
	shutdowns int
}

func newFakeDriver(serveErr error) *fakeDriver {
	return &fakeDriver{
		serveErr: serveErr,
		started:  make(chan struct{}),
		stop:     make(chan struct{}),
	}
}

func (d *fakeDriver) ListenAndServe(addr string, h http.Handler) error {
	d.mu.Lock()
	d.addr, d.handler = addr, h
	d.mu.Unlock()
	close(d.started)

	if d.serveErr != nil {
		return d.serveErr
	}
	<-d.stop
	return http.ErrServerClosed
}

func (d *fakeDriver) Shutdown(context.Context) error {
	d.mu.Lock()
	d.shutdowns++
	d.mu.Unlock()
	d.stopOnce.Do(func() { close(d.stop) })
	return nil
}

func (d *fakeDriver) served() (string, http.Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.addr, d.handler
}

func (d *fakeDriver) shutdownCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shutdowns
}

// tlsFakeDriver additionally implements driver.TLSServer, which is how the
// optional-interface rule is exercised: the capability arrives as a second
// interface, and a listener without it never grows a stub.
type tlsFakeDriver struct {
	*fakeDriver

	mu       sync.Mutex
	certFile string
	keyFile  string
}

func (d *tlsFakeDriver) ListenAndServeTLS(addr, certFile, keyFile string, h http.Handler) error {
	d.mu.Lock()
	d.certFile, d.keyFile = certFile, keyFile
	d.mu.Unlock()
	return d.ListenAndServe(addr, h)
}

func (d *tlsFakeDriver) files() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.certFile, d.keyFile
}

// stubRouter stands in for a third-party router. It is a plain type with its
// own ServeHTTP, which is all http.Handler ever asks of one.
//
// Using it rather than adding gin or chi as a dependency is deliberate: the
// claim under test is that goga takes an http.Handler it has never heard of and
// changes nothing about it, and a type goga could not possibly know about tests
// that claim better than a type it might.
type stubRouter struct {
	hits atomic.Int64
}

func (r *stubRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.hits.Add(1)
	w.Header().Set("X-Stub-Router", "yes")
	_, _ = io.WriteString(w, "routed "+req.URL.Path)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
}

// freeAddr reserves a loopback port and gives it straight back.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// ---------------------------------------------------------------------------
// The port: any http.Handler, unmodified.
// ---------------------------------------------------------------------------

// TestHandlersPassThroughUnmodified is the milestone's central claim. Both
// shapes go through New untouched: the one the standard library ships, and one
// goga has never seen.
func TestHandlersPassThroughUnmodified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "order "+r.PathValue("id"))
	})

	router := &stubRouter{}

	t.Run("http.ServeMux", func(t *testing.T) {
		h := servetest.Start(t.Context(), t, mux)

		status, body := h.Get("/orders/42")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "order 42", body,
			"the mux's own routing, including its path wildcards, is untouched")
	})

	t.Run("a handler goga has never heard of", func(t *testing.T) {
		h := servetest.Start(t.Context(), t, router)

		status, body := h.Get("/anything/at/all")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "routed /anything/at/all", body)
		assert.Equal(t, int64(1), router.hits.Load(),
			"the very instance passed to New is the one that served")
	})
}

// ---------------------------------------------------------------------------
// Instrumentation: exactly once, and never on an operational endpoint.
// ---------------------------------------------------------------------------

// TestApplicationHandlerIsTracedExactlyOnce is checklist item 2.9, asserted on
// real recorded spans rather than on the wiring that produces them.
func TestApplicationHandlerIsTracedExactlyOnce(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler())
	h.AssertTracedOnce("/orders")
}

// TestMiddlewareDoesNotAddASecondSpan: middleware is applied inside the
// OpenTelemetry wrapper, so however much of it a project stacks up, a request
// still produces one server span.
func TestMiddlewareDoesNotAddASecondSpan(t *testing.T) {
	var seen atomic.Int64
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Add(1)
			next.ServeHTTP(w, r)
		})
	}

	h := servetest.Start(t.Context(), t, okHandler(),
		serve.WithMiddleware(count, count))
	h.AssertTracedOnce("/orders")

	assert.Equal(t, int64(2), seen.Load(), "both middlewares ran")
}

// TestOperationalEndpointsAreNotTraced is checklist item 2.3.
func TestOperationalEndpointsAreNotTraced(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler())
	h.AssertOpsPathsNotTraced()
}

// TestEndpointsAddedToOpsAreNotTracedEither: the exemption is a property of the
// mux, not of the four paths goga happens to register on it, so a project that
// adds pprof beside them gets the same treatment.
func TestEndpointsAddedToOpsAreNotTracedEither(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler())
	h.Server.Ops().HandleFunc("/debug/build", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "build info")
	})

	h.Recorder.Reset()
	status, body := h.Get("/debug/build")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "build info", body)
	assert.Never(t, func() bool { return len(h.Recorder.Ended()) > 0 },
		200*time.Millisecond, 10*time.Millisecond,
		"an endpoint registered on Ops was traced")
}

// ---------------------------------------------------------------------------
// Probes.
// ---------------------------------------------------------------------------

// TestFailingReadinessCheckFlipsReadyzOnly: readiness takes an instance out of
// rotation; liveness restarts the process. Conflating them turns a database
// blip into a restart loop.
func TestFailingReadinessCheckFlipsReadyzOnly(t *testing.T) {
	down := errors.New("connection refused")
	h := servetest.Start(t.Context(), t, okHandler(),
		serve.WithReadinessCheck("db", func(context.Context) error { return down }),
		serve.WithHealthCheck("goroutines", func(context.Context) error { return nil }),
	)

	status, body := h.Get(serve.ReadyzPath)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, body, "db: connection refused")

	for _, p := range []string{serve.LivezPath, serve.HealthzPath} {
		status, body := h.Get(p)
		assert.Equalf(t, http.StatusOK, status, "%s must be unaffected", p)
		assert.Contains(t, body, "goroutines: ok")
	}
}

// TestFailingHealthCheckFlipsLivezAndHealthz, and leaves readiness alone: the
// two sets are independent.
func TestFailingHealthCheckFlipsLivezAndHealthz(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler(),
		serve.WithHealthCheck("deadlock", func(context.Context) error {
			return errors.New("the run loop is wedged")
		}),
	)

	for _, p := range []string{serve.LivezPath, serve.HealthzPath} {
		status, body := h.Get(p)
		assert.Equalf(t, http.StatusServiceUnavailable, status, "%s", p)
		assert.Contains(t, body, "deadlock: the run loop is wedged")
	}

	status, _ := h.Get(serve.ReadyzPath)
	assert.Equal(t, http.StatusOK, status)
}

func TestMetricsEndpointIsServed(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler())

	status, body := h.Get(serve.MetricsPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "go_goroutines")
}

// TestOpsAddrMovesTheEndpointsToTheirOwnListener: with a separate address the
// application port stops answering probes and the ops port starts.
func TestOpsAddrMovesTheEndpointsToTheirOwnListener(t *testing.T) {
	opsAddr := freeAddr(t)
	h := servetest.Start(t.Context(), t, okHandler(), serve.WithOpsAddr(opsAddr))

	status, body := h.Get(serve.LivezPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body,
		"on the application port /livez is now just another application path")

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + opsAddr + serve.LivezPath) //nolint:noctx // a bare probe is the point
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 10*time.Millisecond, "the operational listener never answered")
}

// ---------------------------------------------------------------------------
// The listener seam.
// ---------------------------------------------------------------------------

// TestWithDriverSubstitutesTheListener is checklist item 2.7: selection is a
// value, not a name in a table.
func TestWithDriverSubstitutesTheListener(t *testing.T) {
	d := newFakeDriver(nil)
	ctx, cancel := context.WithCancel(t.Context())

	srv, err := serve.New(ctx, okHandler(), serve.WithAddr("127.0.0.1:9999"), serve.WithDriver(d))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	<-d.started
	addr, handler := d.served()
	assert.Equal(t, "127.0.0.1:9999", addr, "the configured address reached the listener")
	require.NotNil(t, handler, "the listener was handed a handler to dispatch to")

	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, 1, d.shutdownCount(), "a cancelled context reaches Shutdown exactly once")
}

// TestRunReportsAnOrderlyStopAsSuccess: http.ErrServerClosed is what a listener
// returns when it was asked to stop, and Run must not dress it up as a failure.
func TestRunReportsAnOrderlyStopAsSuccess(t *testing.T) {
	srv, err := serve.New(t.Context(), okHandler(),
		serve.WithDriver(newFakeDriver(http.ErrServerClosed)))
	require.NoError(t, err)

	assert.NoError(t, srv.Run(t.Context()))
}

// TestRunWrapsAListenerFailure: anything else is an error, and it carries the
// module path and unwraps to what the listener actually reported.
func TestRunWrapsAListenerFailure(t *testing.T) {
	boom := errors.New("bind: address already in use")
	srv, err := serve.New(t.Context(), okHandler(), serve.WithDriver(newFakeDriver(boom)))
	require.NoError(t, err)

	err = srv.Run(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "goga/serve")
}

// TestTLSIsAnOptionalCapability: a listener that has it is used through the
// optional interface, and one that has not is refused at construction rather
// than at the first request.
func TestTLSIsAnOptionalCapability(t *testing.T) {
	t.Run("a listener that serves TLS is given the certificate", func(t *testing.T) {
		d := &tlsFakeDriver{fakeDriver: newFakeDriver(nil)}
		ctx, cancel := context.WithCancel(t.Context())

		srv, err := serve.New(ctx, okHandler(),
			serve.WithDriver(d), serve.WithTLS("cert.pem", "key.pem"))
		require.NoError(t, err)

		done := make(chan error, 1)
		go func() { done <- srv.Run(ctx) }()

		<-d.started
		cert, key := d.files()
		assert.Equal(t, "cert.pem", cert)
		assert.Equal(t, "key.pem", key)

		cancel()
		assert.NoError(t, <-done)
	})

	t.Run("a listener that does not is refused by New", func(t *testing.T) {
		_, err := serve.New(t.Context(), okHandler(),
			serve.WithDriver(newFakeDriver(nil)), serve.WithTLS("cert.pem", "key.pem"))

		require.Error(t, err)
		assert.ErrorIs(t, err, &serve.UnsupportedTLSError{})
	})
}

func TestNewRejectsAnAbsentHandler(t *testing.T) {
	_, err := serve.New(t.Context(), nil)
	assert.Error(t, err)
}

// TestNewRejectsOpsAddrWithACustomDriver: goga cannot build a second instance
// of a listener it was handed, so the combination fails loudly instead of
// quietly dropping one of the two options.
func TestNewRejectsOpsAddrWithACustomDriver(t *testing.T) {
	_, err := serve.New(t.Context(), okHandler(),
		serve.WithDriver(newFakeDriver(nil)), serve.WithOpsAddr("127.0.0.1:9998"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithOpsAddr")
}

// ---------------------------------------------------------------------------
// As.
// ---------------------------------------------------------------------------

// TestAsReachesTheStandardLibraryServer is checklist item 2.10, including the
// half that matters most: a target the listener does not have is false, not an
// error.
func TestAsReachesTheStandardLibraryServer(t *testing.T) {
	srv, err := serve.New(t.Context(), okHandler(), serve.WithReadHeaderTimeout(2*time.Second))
	require.NoError(t, err)

	var std *http.Server
	require.True(t, srv.As(&std), "the in-tree listener exposes its *http.Server")
	assert.Equal(t, 2*time.Second, std.ReadHeaderTimeout)

	var unrelated *net.Dialer
	assert.False(t, srv.As(&unrelated), "an unrelated target is false, not an error")
	assert.False(t, srv.As(nil))
}

// TestAsIsFalseForAListenerThatDoesNotSupportIt: the fake exposes nothing, and
// a caller of As still gets an answer rather than a panic.
func TestAsIsFalseForAListenerThatDoesNotSupportIt(t *testing.T) {
	srv, err := serve.New(t.Context(), okHandler(), serve.WithDriver(newFakeDriver(nil)))
	require.NoError(t, err)

	var std *http.Server
	assert.False(t, srv.As(&std))
}

// ---------------------------------------------------------------------------
// The drain.
// ---------------------------------------------------------------------------

// TestDrainCompletesInFlightRequests is checklist item 2.1's whole point, and
// it is asserted through servetest so that the helper an adopting project runs
// is the one goga runs on itself.
func TestDrainCompletesInFlightRequests(t *testing.T) {
	servetest.AssertDrainsInFlightRequest(t.Context(), t)
}

// TestDrainIsBounded: a request that outlasts the grace does not hold Run open
// for ever. The assertion is deliberately one-sided — the grace is 200ms and
// Run is given ten seconds to return — so that a loaded machine cannot fail it
// while an unbounded drain still cannot pass it.
func TestDrainIsBounded(t *testing.T) {
	entered := make(chan struct{})
	blocked := make(chan struct{})

	h := servetest.Start(t.Context(), t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-blocked
	}), serve.WithShutdownGrace(200*time.Millisecond))
	t.Cleanup(func() { close(blocked) })

	go func() {
		resp, err := h.Client.Get(h.BaseURL + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered

	h.Drain()
	err := h.Wait(10 * time.Second)

	require.Error(t, err, "a drain that could not finish must say so")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "goga/serve")
}

// TestHeaderTimeoutIsEnforcedOnTheWire: the static assertion that the timeout
// reached the *http.Server lives in the internal tests; this one is the socket.
func TestHeaderTimeoutIsEnforcedOnTheWire(t *testing.T) {
	h := servetest.Start(t.Context(), t, okHandler(),
		serve.WithReadHeaderTimeout(150*time.Millisecond))

	h.AssertHeaderTimeoutEnforced(10 * time.Second)
}
