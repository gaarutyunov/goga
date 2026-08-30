package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/serve/driver"
)

// TestDefaultsLeaveNoTimeoutUnbounded is checklist item 2.1's static half: a
// server built with no options at all still has every timeout set. The dynamic
// half — that the listener enforces them on the wire — is
// servetest.Harness.AssertHeaderTimeoutEnforced.
func TestDefaultsLeaveNoTimeoutUnbounded(t *testing.T) {
	d := defaults()
	o := d.driverOptions()

	assert.Positive(t, o.ReadHeaderTimeout, "read header timeout")
	assert.Positive(t, o.ReadTimeout, "read timeout")
	assert.Positive(t, o.WriteTimeout, "write timeout")
	assert.Positive(t, d.shutdownGrace, "shutdown grace")
}

// TestTimeoutOptionsReachDriverOptions is the port-boundary copy: what a caller
// configured has to arrive at the listener, and the two structs deliberately do
// not alias, so nothing but a test says the fields line up.
func TestTimeoutOptionsReachDriverOptions(t *testing.T) {
	set, err := goga.Apply(defaults(),
		WithReadHeaderTimeout(time.Second),
		WithReadTimeout(2*time.Second),
		WithWriteTimeout(3*time.Second),
		WithShutdownGrace(4*time.Second),
	)
	require.NoError(t, err)

	assert.Equal(t, driver.Options{
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
	}, set.driverOptions())
	assert.Equal(t, 4*time.Second, set.shutdownGrace,
		"shutdown grace is serve's own, not the listener's")
}

// TestTimeoutOptionsReachTheStandardLibraryServer closes the last gap: the
// options reach driver.Options, and driver.Options reaches the *http.Server the
// in-tree listener actually serves with.
func TestTimeoutOptionsReachTheStandardLibraryServer(t *testing.T) {
	l := newStdListener(driver.Options{
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
	})

	var got *http.Server
	require.True(t, l.As(&got))
	require.NotNil(t, got)

	assert.Equal(t, time.Second, got.ReadHeaderTimeout)
	assert.Equal(t, 2*time.Second, got.ReadTimeout)
	assert.Equal(t, 3*time.Second, got.WriteTimeout)
}

func TestDurationOptionsRejectNonPositiveValues(t *testing.T) {
	for name, opt := range map[string]func(time.Duration) Option{
		"read header timeout": WithReadHeaderTimeout,
		"read timeout":        WithReadTimeout,
		"write timeout":       WithWriteTimeout,
		"shutdown grace":      WithShutdownGrace,
	} {
		t.Run(name, func(t *testing.T) {
			for _, d := range []time.Duration{0, -time.Second} {
				_, err := goga.Apply(defaults(), opt(d))
				require.Error(t, err, "a %s of %s must be rejected", name, d)
				assert.Contains(t, err.Error(), name)
			}
		})
	}
}

func TestOptionsRejectEmptyAndNilInputs(t *testing.T) {
	for name, opt := range map[string]Option{
		"empty addr":          WithAddr(""),
		"empty ops addr":      WithOpsAddr(""),
		"nil driver":          WithDriver(nil),
		"nil middleware":      WithMiddleware(nil),
		"unnamed check":       WithHealthCheck("", func(context.Context) error { return nil }),
		"nil check":           WithReadinessCheck("db", nil),
		"half-configured TLS": WithTLS("cert.pem", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := goga.Apply(defaults(), opt)
			assert.Error(t, err)
		})
	}
}

// TestChecksRejectDuplicateNames: two checks under one name make the probe body
// ambiguous about which dependency is down, which is the only thing the body is
// for.
func TestChecksRejectDuplicateNames(t *testing.T) {
	ok := func(context.Context) error { return nil }

	_, err := goga.Apply(defaults(), WithHealthCheck("db", ok), WithHealthCheck("db", ok))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	_, err = goga.Apply(defaults(), WithHealthCheck("db", ok), WithReadinessCheck("db", ok))
	assert.NoError(t, err, "the two sets are independent: one name may appear in each")
}

// TestApplyMiddlewareRunsOutermostFirst pins the order the documentation
// promises. Getting it backwards is invisible until a middleware pair depends
// on it — an authenticator that must run before a logger, say.
func TestApplyMiddlewareRunsOutermostFirst(t *testing.T) {
	var order []string
	tag := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := applyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), []func(http.Handler) http.Handler{tag("first"), tag("second")})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, []string{"first", "second", "handler"}, order)
}

// TestRoutePatternStaysLowCardinality: goga instruments outside the
// application's router, so the route is not known when the span is named.
// Naming it after the path instead would put an unbounded number of span names
// into the backend.
func TestRoutePatternStaysLowCardinality(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/orders/12345", nil)

	assert.Equal(t, http.MethodGet, routePattern("", r))
	assert.Equal(t, "operation", routePattern("operation", r))

	r.Pattern = "GET /orders/{id}"
	assert.Equal(t, "GET GET /orders/{id}", routePattern("", r),
		"once a pattern is known it is used verbatim")
}

// TestStdListenerShutdownBeforeStart: Run's drain path calls Shutdown on every
// listener, including one whose ListenAndServe never got as far as binding.
func TestStdListenerShutdownBeforeStart(t *testing.T) {
	d := defaults()
	l := newStdListener(d.driverOptions())
	assert.NoError(t, l.Shutdown(t.Context()))
}

func TestStdListenerAsRejectsOtherTargets(t *testing.T) {
	d := defaults()
	l := newStdListener(d.driverOptions())

	var mux *http.ServeMux
	assert.False(t, l.As(&mux))
	assert.False(t, l.As(nil))
}

// TestUnsupportedTLSErrorBranches: a caller that would serve TLS if it could
// has to be able to tell this failure from a bad certificate path.
func TestUnsupportedTLSErrorBranches(t *testing.T) {
	err := fmt.Errorf("goga/serve: new: %w", &UnsupportedTLSError{Driver: "*serve.fake"})

	assert.ErrorIs(t, err, &UnsupportedTLSError{})
	var typed *UnsupportedTLSError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, "*serve.fake", typed.Driver)
	assert.NotErrorIs(t, err, errNilDriver)
}

// TestRootHandlerDispatchesEverythingOnTheOpsMuxOutsideTheApplication is
// checklist item 2.3 at the routing level: the ops mux is consulted first, so
// nothing registered on it can be reached through the instrumented handler.
func TestRootHandlerDispatchesEverythingOnTheOpsMuxOutsideTheApplication(t *testing.T) {
	ops := newOpsMux(nil, nil)
	ops.HandleFunc("/debug/added-later", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	var appHits int
	root := rootHandler{
		app: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			appHits++
			w.WriteHeader(http.StatusOK)
		}),
		ops: ops,
	}

	for _, p := range []string{LivezPath, ReadyzPath, HealthzPath, MetricsPath, "/debug/added-later"} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		assert.NotEqual(t, http.StatusNotFound, rec.Code, "%s must be served by the ops mux", p)
	}
	assert.Zero(t, appHits, "no operational path reached the application handler")

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders", nil))
	assert.Equal(t, 1, appHits)
}

// TestProbeReportsEveryCheckByName: the status code says something is wrong and
// the body says what.
func TestProbeReportsEveryCheckByName(t *testing.T) {
	boom := errors.New("connection refused")
	h := probe([]check{
		{name: "db", fn: func(context.Context) error { return boom }},
		{name: "cache", fn: func(context.Context) error { return nil }},
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, LivezPath, nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "db: connection refused")
	assert.Contains(t, rec.Body.String(), "cache: ok")
}

func TestProbeWithNoChecksSucceeds(t *testing.T) {
	rec := httptest.NewRecorder()
	probe(nil)(rec, httptest.NewRequest(http.MethodGet, LivezPath, nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
}
