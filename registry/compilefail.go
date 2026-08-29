//go:build compilefail_registry

// This file exists to prove a negative that an ordinary test cannot express: an
// option built for one adapter's settings type must not be accepted by another
// adapter's handle. A compile error is not a test failure, so the check is run
// by building this file behind a tag and expecting the build to FAIL:
//
//	go build -tags compilefail_registry ./registry   # MUST fail
//
// Expected diagnostic, near the marked line below:
//
//	cannot use cfWithPath("/events") (value of func type Option[cfSSESettings])
//	as Option[cfHTTPSettings] value in argument to http.Open
//
// Without the tag the file is invisible, so the default build, `go vet ./...`
// and `go test ./...` are unaffected by it. The positive half of the guarantee
// — that an adapter's *own* option is accepted with no type arguments written
// at the call site — is TestAdapterOpenAcceptsItsOwnOption in registry_test.go.
package registry

import "context"

type cfPort interface{ Ping() error }

type cfConn struct{}

func (cfConn) Ping() error { return nil }

type cfHTTPSettings struct{ Addr string }

type cfSSESettings struct{ Path string }

func cfNewHTTP(context.Context, cfHTTPSettings) (cfPort, error) { return cfConn{}, nil }

func cfWithPath(p string) Option[cfSSESettings] {
	return func(s *cfSSESettings) error {
		s.Path = p
		return nil
	}
}

func cfMustNotCompile() {
	reg := New(func(Settings, any) error { return nil })

	http, err := reg.Provide("http", cfNewHTTP)
	if err != nil {
		return
	}

	// http is an Adapter[cfPort, cfHTTPSettings], so its Open takes
	// ...Option[cfHTTPSettings]. cfWithPath belongs to a different adapter.
	_, _ = http.Open(context.Background(), nil, cfWithPath("/events")) // MUST NOT COMPILE
}
