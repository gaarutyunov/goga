package serve

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Operational endpoint paths. They are constants rather than options because
// checklist item 2.3 is that no option may move them: an endpoint an
// orchestrator probes is part of the contract between the process and the thing
// that restarts it, and a framework that lets each project rename them has
// given up the one property that made them worth centralising.
const (
	// LivezPath answers whether the process is viable. A failure here is a
	// request to restart it.
	LivezPath = "/livez"

	// HealthzPath is the legacy alias of [LivezPath] and answers identically.
	// It exists because a great deal of deployed configuration still probes it.
	HealthzPath = "/healthz"

	// ReadyzPath answers whether this instance should be sent traffic now. A
	// failure takes it out of rotation and leaves it running.
	ReadyzPath = "/readyz"

	// MetricsPath is scraped by Prometheus. goga/telemetry attaches a
	// Prometheus reader to prometheus.DefaultRegisterer by default, so the
	// handler here exports goga's own metrics with no further wiring.
	MetricsPath = "/metrics"
)

// newOpsMux builds the mux the operational endpoints live on.
//
// It is returned rather than merged into the application's routing because it
// is never wrapped in OpenTelemetry instrumentation: see [Server] for why that
// is a correctness property and not a preference.
func newOpsMux(health, readiness []check) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(MetricsPath, promhttp.Handler())
	mux.HandleFunc(LivezPath, probe(health))
	mux.HandleFunc(HealthzPath, probe(health))
	mux.HandleFunc(ReadyzPath, probe(readiness))
	return mux
}

// probe runs a set of checks and reports the result as text.
//
// The body names every check and its outcome, because the number a probe
// returns tells an operator that something is wrong and nothing about what. The
// checks run under the request's own context, so a client that gives up stops
// them.
//
// A probe with no checks registered succeeds. That is the honest answer: the
// process is running and nothing has claimed otherwise.
func probe(checks []check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		ok := true
		for _, c := range checks {
			if err := c.fn(r.Context()); err != nil {
				ok = false
				fmt.Fprintf(&b, "%s: %v\n", c.name, err)
				continue
			}
			fmt.Fprintf(&b, "%s: ok\n", c.name)
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
			if b.Len() == 0 {
				b.WriteString("ok\n")
			}
		}
		// The write is the last thing this handler does; there is nothing to
		// report a failed flush to but the connection that already broke.
		_, _ = w.Write([]byte(b.String()))
	}
}

// rootHandler dispatches between the operational mux and the instrumented
// application handler.
//
// The dispatch is a lookup on the ops mux rather than a fixed list of paths so
// that anything a caller adds through [Server.Ops] — /debug/pprof, a build-info
// endpoint — is served on the same terms: outside the instrumentation, outside
// the application's middleware. Encoding it this way is the point of the
// wrapper: there is no option that moves an operational endpoint inside the
// traced handler, because the traced handler is only ever reached when the ops
// mux has nothing for the request.
type rootHandler struct {
	app http.Handler
	ops *http.ServeMux
}

func (h rootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Handler reports the pattern it matched, and the empty string when it
	// matched nothing — which is how a mux is asked "is this yours?" without
	// serving a 404 from it.
	if _, pattern := h.ops.Handler(r); pattern != "" {
		h.ops.ServeHTTP(w, r)
		return
	}
	h.app.ServeHTTP(w, r)
}

// applyMiddleware wraps h outermost-first, so that the first middleware a
// caller passed to [WithMiddleware] is the first to see a request.
func applyMiddleware(h http.Handler, mw []func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// routePattern names a server span.
//
// The OpenTelemetry HTTP semantic conventions ask for "{method} {route}", and
// for "{method}" alone when the route is not known. It is not known here: goga
// instruments outside the application's router, so the pattern the router will
// match has not been decided when the span is named. Naming the span after the
// request path instead would put an unbounded number of names into the backend,
// which is the failure the convention exists to prevent.
func routePattern(operation string, r *http.Request) string {
	if r.Pattern != "" {
		return r.Method + " " + r.Pattern
	}
	if operation != "" {
		return operation
	}
	return r.Method
}
