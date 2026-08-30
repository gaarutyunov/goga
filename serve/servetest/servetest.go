// Package servetest holds the test helpers a project adopting
// [github.com/gaarutyunov/goga/serve] runs against its own handler.
//
// # It is not a conformance suite, and that is deliberate
//
// The name follows goga's convention for a module's test package, and the
// convention says a conformance suite lives in one. This package is not that.
// The listener port has one implementation in v1, and goga's own rule is that a
// conformance suite for a single implementation is cost without the property it
// establishes: interchangeability is a claim about two things, and there is
// only one.
//
// What ships instead is the assertions a project wants about *its own* server:
//
//   - a request is traced exactly once, [Harness.AssertTracedOnce];
//   - the operational endpoints are not traced at all,
//     [Harness.AssertOpsPathsNotTraced];
//   - a configured header timeout is actually enforced on the wire,
//     [Harness.AssertHeaderTimeoutEnforced];
//   - an in-flight request survives a drain, [AssertDrainsInFlightRequest].
//
// Every one of those is a property of a *serve.Server built from the project's
// own options, not of a listener — which is the second reason they are not a
// conformance suite. When a second listener ships, the drain and the timeout
// assertions are what a conformance suite gets built from. Until then, please
// do not "fix" this package into one.
//
// # How the harness runs a server
//
// [Start] reserves a loopback port, appends its own [serve.WithAddr] after the
// caller's options so that it wins, and runs the resulting server on the real
// listener the project configured. Nothing is stubbed: every option the caller
// passed applies exactly as it would in production, which is the only way an
// assertion about a timeout or a drain means anything.
//
// It installs a [SpanRecorder] as the process-wide OpenTelemetry tracer
// provider, so a test using it must not call t.Parallel: the provider is global
// and two harnesses would record into each other.
package servetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/gaarutyunov/goga/serve"
)

// TB is the part of *testing.T this package uses.
//
// It is an interface rather than *testing.T so that servetest does not import
// the testing package: a non-test package that does registers the test flags in
// every binary that links it, and this package is imported by tests that also
// build tools. *testing.T satisfies it.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	FailNow()
	Cleanup(func())
}

// settleWindow is how long an assertion watches for a span that should not
// arrive, or for a second copy of one that should arrive once. It is a
// negative-evidence window, so it trades test duration for confidence directly;
// a fifth of a second is far longer than the microseconds an in-process
// otelhttp handler takes to end a span.
const settleWindow = 200 * time.Millisecond

// readyTimeout bounds how long [Start] waits for the listener to accept
// connections. It is generous because a loaded machine can be slow to schedule
// the serving goroutine, and a generous bound on a condition that is normally
// met in a millisecond costs nothing when it is met.
const readyTimeout = 10 * time.Second

// Harness is one *serve.Server running on a loopback port for the duration of
// one test.
type Harness struct {
	// Server is the server under test.
	Server *serve.Server

	// Recorder holds the spans the server produced. It is installed as the
	// process tracer provider by [Start].
	Recorder *SpanRecorder

	// BaseURL is the http:// origin the server is listening on, with no
	// trailing slash.
	BaseURL string

	// Client talks to the server. It has a bounded timeout so that a test
	// cannot hang on a server that stopped answering.
	Client *http.Client

	t      TB
	cancel context.CancelFunc
	done   chan error
}

// Start builds a server around h and runs it until the test ends.
//
// The caller's options are applied first and a [serve.WithAddr] naming the
// reserved loopback port is applied after them, so a WithAddr among opts is
// overridden — the harness has to know where to send its requests. Every other
// option applies unchanged.
//
// The server is stopped and drained by a t.Cleanup, so a test that does not
// care about the shutdown does not have to say anything about it.
func Start(ctx context.Context, t TB, h http.Handler, opts ...serve.Option) *Harness {
	t.Helper()

	rec := NewSpanRecorder()
	otel.SetTracerProvider(rec)

	addr := reserveAddr(t)

	all := make([]serve.Option, 0, len(opts)+1)
	all = append(all, opts...)
	all = append(all, serve.WithAddr(addr))

	srv, err := serve.New(ctx, h, all...)
	require.NoError(t, err, "servetest: building the server under test")

	runCtx, cancel := context.WithCancel(ctx)
	hs := &Harness{
		Server:   srv,
		Recorder: rec,
		BaseURL:  "http://" + addr,
		Client:   &http.Client{Timeout: readyTimeout},
		t:        t,
		cancel:   cancel,
		done:     make(chan error, 1),
	}
	go func() { hs.done <- srv.Run(runCtx) }()
	t.Cleanup(func() { _ = hs.Stop() })

	hs.waitReady(addr)
	return hs
}

// reserveAddr asks the kernel for a free loopback port and gives it straight
// back, so that the server can bind the address the test already knows.
//
// The alternative — letting the server bind :0 and asking it what it got — is
// not available: the listener port hands out no address, deliberately, because
// an address is a thing the caller configured and not a thing a listener
// invents.
func reserveAddr(t TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "servetest: reserving a loopback port")
	addr := ln.Addr().String()
	require.NoError(t, ln.Close(), "servetest: releasing the reserved port")
	return addr
}

// waitReady blocks until the server accepts a connection, or fails the test.
func (h *Harness) waitReady(addr string) {
	h.t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for {
		select {
		case err := <-h.done:
			h.done <- err
			require.FailNowf(h.t, "servetest: the server stopped before it was ready",
				"Run returned %v", err)
			return
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			require.FailNowf(h.t, "servetest: the server never accepted a connection",
				"waited %s for %s: %v", readyTimeout, addr, err)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Drain cancels the server's context without waiting for it to stop. Use it
// when the test has to do something — release a blocked handler, issue one more
// request — while the drain is in progress.
func (h *Harness) Drain() { h.cancel() }

// Wait blocks until [serve.Server.Run] returns and reports its error, failing
// the test if it does not return within the given bound.
func (h *Harness) Wait(within time.Duration) error {
	h.t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-timer.C:
		require.FailNowf(h.t, "servetest: the server did not stop",
			"Run had not returned %s after the drain began", within)
		return nil
	}
}

// Stop drains the server and waits for it to finish, reporting Run's error.
// It is called automatically when the test ends; calling it earlier is how a
// test asserts on that error.
func (h *Harness) Stop() error {
	h.t.Helper()
	h.Drain()
	return h.Wait(readyTimeout)
}

// Get issues a GET to a path on the server and returns the status code and the
// whole body, with the body already closed.
func (h *Harness) Get(path string) (int, string) {
	h.t.Helper()
	resp, err := h.Client.Get(h.BaseURL + path) //nolint:bodyclose // closed below
	require.NoErrorf(h.t, err, "servetest: GET %s", path)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoErrorf(h.t, err, "servetest: reading the body of GET %s", path)
	return resp.StatusCode, string(body)
}

// AssertTracedOnce asserts that a request to path produces exactly one server
// span.
//
// Once is the whole assertion. Zero means the instrumentation is not reaching
// the handler; two means it was applied twice — a project wrapping its handler
// in otelhttp itself before handing it to goga, which is the mistake this
// assertion exists to catch, because two nested server spans do not look broken
// in a backend, they look like a slow service with a mysterious inner hop.
func (h *Harness) AssertTracedOnce(path string) {
	h.t.Helper()
	h.Recorder.Reset()

	status, _ := h.Get(path)
	assert.Lessf(h.t, status, 500, "GET %s failed before it could be traced", path)

	require.Eventuallyf(h.t, func() bool { return len(h.Recorder.Ended()) >= 1 },
		readyTimeout, 5*time.Millisecond,
		"GET %s produced no span: the handler is not instrumented", path)
	assert.Neverf(h.t, func() bool { return len(h.Recorder.Ended()) > 1 },
		settleWindow, 10*time.Millisecond,
		"GET %s produced more than one span: the handler is instrumented twice", path)
}

// AssertOpsPathsNotTraced asserts that none of the operational endpoints
// produces a span.
//
// A liveness probe every second is not a request the service received. Losing
// this property is invisible until somebody looks at a trace backend's bill, so
// it is worth an explicit assertion in the adopting project rather than trust
// in goga's wiring.
func (h *Harness) AssertOpsPathsNotTraced() {
	h.t.Helper()
	h.Recorder.Reset()

	for _, p := range []string{serve.LivezPath, serve.ReadyzPath, serve.HealthzPath, serve.MetricsPath} {
		status, _ := h.Get(p)
		assert.Lessf(h.t, status, 500, "GET %s failed", p)
	}
	assert.Neverf(h.t, func() bool { return len(h.Recorder.Ended()) > 0 },
		settleWindow, 10*time.Millisecond,
		"an operational endpoint was traced: spans %v", h.Recorder.Names())
}

// AssertHeaderTimeoutEnforced opens a connection, sends nothing, and asserts
// that the server closes it within the given bound.
//
// Pass a bound comfortably above the [serve.WithReadHeaderTimeout] the server
// was configured with. An unbounded header timeout is the cheapest denial of
// service there is — one idle connection, one goroutine, held for the life of
// the process — and it is invisible in every functional test, which is why it
// gets an assertion of its own.
func (h *Harness) AssertHeaderTimeoutEnforced(within time.Duration) {
	h.t.Helper()

	conn, err := net.Dial("tcp", h.hostPort())
	require.NoError(h.t, err, "servetest: dialling the server")
	defer func() { _ = conn.Close() }()

	require.NoError(h.t, conn.SetReadDeadline(time.Now().Add(within)))

	_, err = conn.Read(make([]byte, 1))
	require.Errorf(h.t, err, "the server answered a connection that sent no request")
	assert.Falsef(h.t, errors.Is(err, os.ErrDeadlineExceeded),
		"the server held an idle connection for %s: the read header timeout is not enforced", within)
	assert.Truef(h.t, errors.Is(err, io.EOF) || isConnReset(err),
		"expected the server to close the idle connection, got %v", err)
}

// hostPort is BaseURL without its scheme.
func (h *Harness) hostPort() string {
	return h.BaseURL[len("http://"):]
}

// isConnReset reports whether err is a peer reset, which is how some platforms
// report the close of a connection that never sent a request.
func isConnReset(err error) bool {
	var se *os.SyscallError
	if errors.As(err, &se) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && !ne.Timeout()
}

// AssertDrainsInFlightRequest asserts that a request already being served when
// the server's context is cancelled runs to completion and gets its response.
//
// It builds a server of its own, because the property is about a handler that
// blocks and the project's handler does not. Pass the project's own options —
// its shutdown grace above all — so that what is asserted is the configuration
// the project actually ships.
func AssertDrainsInFlightRequest(ctx context.Context, t TB, opts ...serve.Option) {
	t.Helper()

	entered := make(chan struct{})
	release := make(chan struct{})
	const body = "drained"

	h := Start(ctx, t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = fmt.Fprint(w, body)
	}), opts...)

	type result struct {
		status int
		body   string
	}
	got := make(chan result, 1)
	go func() {
		status, b := h.Get("/")
		got <- result{status, b}
	}()

	<-entered
	h.Drain()
	close(release)

	select {
	case r := <-got:
		assert.Equal(t, http.StatusOK, r.status,
			"the in-flight request did not complete across the drain")
		assert.Equal(t, body, r.body,
			"the in-flight request lost its response body across the drain")
	case <-time.After(readyTimeout):
		require.FailNow(t, "servetest: the in-flight request never completed")
	}

	assert.NoError(t, h.Wait(readyTimeout), "the drain reported an error")
}
