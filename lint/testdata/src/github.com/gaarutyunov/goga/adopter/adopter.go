// Package adopter is the fixture that has to stay silent. Every declaration in
// it is a near miss: something the rule could plausibly fire on and must not.
//
// analysistest fails on an unexpected diagnostic as well as on a missing one,
// so this file is what proves gogaserve is a rule about constructing a
// listener rather than a blanket ban on naming net/http.
package adopter

import (
	"net/http"
	"net/http/httptest"

	"github.com/gaarutyunov/goga/serve"
)

// Correct is the shape the rule is asking for: keep the router — an
// *http.ServeMux is already an http.Handler — and hand it to serve.New.
func Correct() *serve.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {})

	return serve.New(mux)
}

// TestServers pins the exclusion that needs no code: httptest.NewServer returns
// a *httptest.Server, which is a different type from a different import path,
// so a rule keyed on the net/http import never sees it. A test server is not a
// production listener and reporting one would make the rule unusable in tests.
func TestServers(h http.Handler) (*httptest.Server, *httptest.Server) {
	return httptest.NewServer(h), httptest.NewUnstartedServer(h)
}

// ForeignLiteral is the same exclusion written as a composite literal, and it
// is the one that catches a rule matching the bare selector name "Server"
// instead of the package it belongs to.
func ForeignLiteral() *httptest.Server {
	return &httptest.Server{}
}

// Escape pins task 2.10's As contract. Naming *http.Server as a TYPE is exactly
// what As asks a caller to do, so a rule that reported type references would
// fire on the escape hatch goga documents.
func Escape(s *serve.Server) *http.Server {
	var srv *http.Server
	if !s.As(&srv) {
		return nil
	}

	return srv
}

// Tune declares an *http.Server parameter and a zero value of the type. Neither
// constructs anything, and construction is the defect.
func Tune(srv *http.Server) {
	var zero http.Server
	_ = zero
	srv.Addr = ":9090"
}

// MethodCall is a stated limit rather than a near miss. Calling ListenAndServe
// on a server obtained through As is a bypass, but the receiver is an
// expression whose type only the type checker knows, and this plugin loads
// syntax alone. The rule reports construction, which is where the *http.Server
// in a project's own code has to come from in the first place.
func MethodCall(s *serve.Server) error {
	var srv *http.Server
	if !s.As(&srv) {
		return nil
	}

	return srv.ListenAndServe()
}
