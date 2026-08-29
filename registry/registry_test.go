package registry_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transport is a stand-in port. lookalike has an identical method set and is a
// different type, which is the case the port check exists for.
type (
	transport interface{ Send(string) error }
	lookalike interface{ Send(string) error }
)

// httpSettings is the settings type of one adapter. Note the koanf tag on Addr:
// the config key is "address", which does not match the field name, so a
// decoder that ignored tags would silently leave the field zero.
type httpSettings struct {
	Addr    string `koanf:"address"`
	Retries int    `koanf:"retries"`
}

type httpTransport struct {
	addr    string
	retries int
}

func (h *httpTransport) Send(string) error { return nil }

func newHTTP(_ context.Context, s httpSettings) (transport, error) {
	if s.Addr == "" {
		return nil, errors.New("addr is required")
	}
	return &httpTransport{addr: s.Addr, retries: s.Retries}, nil
}

type sseSettings struct {
	Path string `koanf:"path"`
}

type sseTransport struct{ path string }

func (s *sseTransport) Send(string) error { return nil }

func newSSE(_ context.Context, s sseSettings) (transport, error) {
	return &sseTransport{path: s.Path}, nil
}

type stdioSettings struct{}

type stdioTransport struct{}

func (stdioTransport) Send(string) error { return nil }

func newStdio(context.Context, stdioSettings) (transport, error) {
	return stdioTransport{}, nil
}

func withRetries(n int) registry.Option[httpSettings] {
	return func(s *httpSettings) error {
		if n < 0 {
			return fmt.Errorf("retries must not be negative, got %d", n)
		}
		s.Retries = n
		return nil
	}
}

func withAddr(a string) registry.Option[httpSettings] {
	return func(s *httpSettings) error {
		s.Addr = a
		return nil
	}
}

// newRegistry returns a registry wired to the tag-honouring test decoder, with
// the three transports registered under the names a config file would use.
func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("http", newHTTP))
	require.NoError(t, reg.Register("sse", newSSE))
	require.NoError(t, reg.Register("stdio", newStdio))
	return reg
}

// --- 1. duplicate name -------------------------------------------------------

func TestRegisterDuplicateNameReturnsTypedError(t *testing.T) {
	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("http", newHTTP))

	err := reg.Register("http", newHTTP)

	require.Error(t, err, "a duplicate must be reported, not silently overwritten")
	assert.ErrorIs(t, err, registry.ErrDuplicateName)

	var dup *registry.DuplicateNameError
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, "http", dup.Name)
	require.NotNil(t, dup.Port)
	assert.Equal(t, "registry_test.transport", dup.Port.String())
	assert.EqualError(t, err,
		`goga/registry: register "http": name already registered for port registry_test.transport`)
}

func TestRegisterDuplicateAcrossPortsStillCollides(t *testing.T) {
	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("http", newHTTP))

	// A different port under the same name is still a duplicate: the registry
	// is keyed by name alone, which is what a config file supplies.
	err := reg.Register("http", func(context.Context, sseSettings) (lookalike, error) {
		return &sseTransport{}, nil
	})
	assert.ErrorIs(t, err, registry.ErrDuplicateName)
}

func TestProvideReturnsTheDuplicateError(t *testing.T) {
	reg := registry.New(koanfDecode)
	_, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	_, err = reg.Provide("http", newHTTP)
	assert.ErrorIs(t, err, registry.ErrDuplicateName)
}

func TestRegisterNilConstructor(t *testing.T) {
	reg := registry.New(koanfDecode)
	err := reg.Register[transport, httpSettings]("http", nil)
	require.Error(t, err)
	assert.Empty(t, reg.Names(), "a rejected registration must not occupy the name")
}

// --- 2. New(nil) panics ------------------------------------------------------

func TestNewPanicsOnNilDecode(t *testing.T) {
	assert.PanicsWithValue(t, "goga/registry: New called with a nil Decode", func() {
		registry.New(nil)
	})
}

func TestNewWithDecodeDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { _ = registry.New(koanfDecode) })
}

// --- 3. unknown name ---------------------------------------------------------

func TestOpenUnknownNameNamesWhatIsRegistered(t *testing.T) {
	reg := newRegistry(t)

	_, err := reg.Open[transport](t.Context(), "htp", nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, registry.ErrUnknownName)
	assert.EqualError(t, err,
		`goga/registry: no adapter "htp" registered for port registry_test.transport (registered: http, sse, stdio)`)

	var unknown *registry.UnknownNameError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, "htp", unknown.Name)
	assert.Equal(t, []string{"http", "sse", "stdio"}, unknown.Registered)
}

func TestOpenUnknownNameOnAnEmptyRegistry(t *testing.T) {
	reg := registry.New(koanfDecode)

	_, err := reg.Open[transport](t.Context(), "http", nil)

	assert.ErrorIs(t, err, registry.ErrUnknownName)
	assert.EqualError(t, err,
		`goga/registry: no adapter "http" registered for port registry_test.transport (registry is empty)`)
}

// --- 4. successful typed open ------------------------------------------------

func TestProvideAndAdapterOpen(t *testing.T) {
	reg := registry.New(koanfDecode)

	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)
	assert.Equal(t, "http", http.Name())

	tr, err := http.Open(t.Context(), registry.Settings{"address": "127.0.0.1:8080"})
	require.NoError(t, err)

	got, ok := tr.(*httpTransport)
	require.True(t, ok, "the handle must yield the concrete adapter behind the port")
	assert.Equal(t, "127.0.0.1:8080", got.addr)
}

func TestOpenConfigDrivenPath(t *testing.T) {
	reg := newRegistry(t)

	tr, err := reg.Open[transport](t.Context(), "sse", registry.Settings{"path": "/events"})
	require.NoError(t, err)

	got, ok := tr.(*sseTransport)
	require.True(t, ok)
	assert.Equal(t, "/events", got.path)
}

func TestAdapterOpenAppliesOptionsOnTopOfRawConfig(t *testing.T) {
	reg := registry.New(koanfDecode)
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	tests := []struct {
		name        string
		raw         registry.Settings
		opts        []registry.Option[httpSettings]
		wantAddr    string
		wantRetries int
	}{
		{
			name:        "raw config alone",
			raw:         registry.Settings{"address": "a:1", "retries": 2},
			wantAddr:    "a:1",
			wantRetries: 2,
		},
		{
			name:        "options alone, no config present",
			raw:         nil,
			opts:        []registry.Option[httpSettings]{withAddr("b:2"), withRetries(9)},
			wantAddr:    "b:2",
			wantRetries: 9,
		},
		{
			name:        "an option overrides the config it sits on top of",
			raw:         registry.Settings{"address": "a:1", "retries": 2},
			opts:        []registry.Option[httpSettings]{withRetries(7)},
			wantAddr:    "a:1",
			wantRetries: 7,
		},
		{
			name:        "later options win over earlier ones",
			raw:         registry.Settings{"address": "a:1"},
			opts:        []registry.Option[httpSettings]{withRetries(1), withRetries(3)},
			wantAddr:    "a:1",
			wantRetries: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := http.Open(t.Context(), tt.raw, tt.opts...)
			require.NoError(t, err)

			got := tr.(*httpTransport)
			assert.Equal(t, tt.wantAddr, got.addr)
			assert.Equal(t, tt.wantRetries, got.retries)
		})
	}
}

func TestAdapterOpenReportsAnOptionErrorInTheGogaShape(t *testing.T) {
	reg := registry.New(koanfDecode)
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	_, err = http.Open(t.Context(), registry.Settings{"address": "a:1"}, withRetries(-1))

	require.Error(t, err)
	assert.EqualError(t, err, "goga: applying option: retries must not be negative, got -1")
}

// TestAdapterOpenAcceptsItsOwnOption is the positive half of the compile-time
// guarantee whose negative half lives in compilefail.go: an option built for
// this adapter's settings type is accepted with no type arguments written at
// the call site. Passing an option for a *different* adapter's settings does
// not compile — see compilefail.go for how to check that.
func TestAdapterOpenAcceptsItsOwnOption(t *testing.T) {
	reg := registry.New(koanfDecode)
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	tr, err := http.Open(t.Context(), nil, withAddr("only-via-option:1"))
	require.NoError(t, err)
	assert.Equal(t, "only-via-option:1", tr.(*httpTransport).addr)
}

func TestAdapterOptionIsInterchangeableWithGogaOption(t *testing.T) {
	// registry.Option is an alias, not a second declaration, so an option
	// written as a goga.Option is the same type and folds with goga.Apply.
	var opt goga.Option[httpSettings] = withRetries(4)
	var same registry.Option[httpSettings] = opt

	s, err := goga.Apply(httpSettings{Addr: "a:1"}, same)
	require.NoError(t, err)
	assert.Equal(t, httpSettings{Addr: "a:1", Retries: 4}, s)
}

func TestAdapterOpenPropagatesConstructorError(t *testing.T) {
	reg := registry.New(koanfDecode)
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	_, err = http.Open(t.Context(), nil) // newHTTP rejects an empty addr
	require.Error(t, err)
	assert.EqualError(t, err, `goga/registry: open "http": addr is required`)
}

func TestZeroAdapterIsUnusable(t *testing.T) {
	var a registry.Adapter[transport, httpSettings]

	_, err := a.Open(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero value")
}

// --- 5. decoding into an unexported settings struct --------------------------

func TestSettingsDecodeHonoursKoanfTags(t *testing.T) {
	reg := newRegistry(t)

	// The key is "address"; the field is Addr. Only the koanf tag connects
	// them, so a zero Addr here would mean the tag was ignored.
	tr, err := reg.Open[transport](t.Context(), "http", registry.Settings{
		"address": "tagged:1234",
		"retries": 5,
	})
	require.NoError(t, err)

	got := tr.(*httpTransport)
	assert.Equal(t, "tagged:1234", got.addr, "the koanf tag, not the field name, selects the key")
	assert.Equal(t, 5, got.retries)
}

func TestSettingsDecodeRejectsAFieldNameThatIsNotTheKey(t *testing.T) {
	reg := newRegistry(t)

	// "addr" is the field name, not the configured key, so a decoder that
	// rejects unknown keys must fail rather than leave Addr zero.
	_, err := reg.Open[transport](t.Context(), "http", registry.Settings{"addr": "wrong:1"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `goga/registry: open "http": decoding settings into registry_test.httpSettings`)
	assert.Contains(t, err.Error(), "unknown keys: addr")
}

func TestSettingsDecodeRejectsUnknownKeys(t *testing.T) {
	reg := newRegistry(t)

	_, err := reg.Open[transport](t.Context(), "http", registry.Settings{
		"address": "a:1",
		"typo":    true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown keys: typo")
}

func TestEmptySettingsSkipTheDecoder(t *testing.T) {
	var called int
	reg := registry.New(func(raw registry.Settings, dst any) error {
		called++
		return koanfDecode(raw, dst)
	})
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	_, err = http.Open(t.Context(), nil, withAddr("a:1"))
	require.NoError(t, err)
	_, err = http.Open(t.Context(), registry.Settings{}, withAddr("a:1"))
	require.NoError(t, err)
	assert.Zero(t, called, "an options-only Open must not need a config subtree")

	_, err = http.Open(t.Context(), registry.Settings{"address": "a:1"})
	require.NoError(t, err)
	assert.Equal(t, 1, called)
}

// --- 6. wrong port -----------------------------------------------------------

func TestOpenWithTheWrongPortFails(t *testing.T) {
	reg := newRegistry(t)

	// lookalike has http's exact method set. The check is type identity, not
	// structural, so this must still fail — and fail before constructing.
	_, err := reg.Open[lookalike](t.Context(), "http", registry.Settings{"address": "a:1"})

	require.Error(t, err)
	assert.ErrorIs(t, err, registry.ErrPortMismatch)
	assert.EqualError(t, err,
		"goga/registry: adapter \"http\" is registered for port registry_test.transport, not registry_test.lookalike")

	var mismatch *registry.PortMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, "http", mismatch.Name)
	assert.Equal(t, "registry_test.transport", mismatch.Registered.String())
	assert.Equal(t, "registry_test.lookalike", mismatch.Requested.String())
}

func TestOpenWithTheWrongPortDoesNotRunTheConstructor(t *testing.T) {
	var ran bool
	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("boom", func(context.Context, httpSettings) (transport, error) {
		ran = true
		return nil, nil
	}))

	_, err := reg.Open[lookalike](t.Context(), "boom", nil)
	require.ErrorIs(t, err, registry.ErrPortMismatch)
	assert.False(t, ran, "the port check must precede construction")
}

func TestOpenReturnsTheConcreteZeroValueOnFailure(t *testing.T) {
	reg := newRegistry(t)

	tr, err := reg.Open[transport](t.Context(), "nope", nil)
	require.Error(t, err)
	assert.Nil(t, tr)
}

func TestOpenPreservesAConstructorsNilPortValue(t *testing.T) {
	// A constructor is allowed to return a nil interface with a nil error. The
	// value must survive the trip through the registry rather than being lost
	// to a failed type assertion on a boxed any.
	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("nil", func(context.Context, stdioSettings) (transport, error) {
		return nil, nil
	}))

	tr, err := reg.Open[transport](t.Context(), "nil", nil)
	require.NoError(t, err)
	assert.Nil(t, tr)
}

// --- 7. concurrency ----------------------------------------------------------

func TestConcurrentRegisterOpenAndNames(t *testing.T) {
	const n = 32

	reg := registry.New(koanfDecode)
	require.NoError(t, reg.Register("http", newHTTP))

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(3)

		go func() {
			defer wg.Done()
			// Every goroutine registers a distinct name; a handful collide with
			// "http" on purpose so the duplicate path runs under the race
			// detector too.
			name := fmt.Sprintf("adapter-%02d", i)
			if i%8 == 0 {
				name = "http"
			}
			_ = reg.Register(name, newSSE)
		}()

		go func() {
			defer wg.Done()
			assert.True(t, sortedAscending(reg.Names()))
		}()

		go func() {
			defer wg.Done()
			_, _ = reg.Open[transport](t.Context(), "http", registry.Settings{"address": "a:1"})
		}()
	}
	wg.Wait()

	names := reg.Names()
	assert.Contains(t, names, "http")
	assert.True(t, sortedAscending(names))
}

func TestConcurrentAdapterOpen(t *testing.T) {
	reg := registry.New(koanfDecode)
	http, err := reg.Provide("http", newHTTP)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := http.Open(t.Context(), registry.Settings{"address": "a:1"}, withRetries(i))
			assert.NoError(t, err)
			assert.Equal(t, i, tr.(*httpTransport).retries, "settings must not be shared between opens")
		}()
	}
	wg.Wait()
}

func sortedAscending(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// --- Names -------------------------------------------------------------------

func TestNamesIsSortedAndEmptyForAFreshRegistry(t *testing.T) {
	assert.Empty(t, registry.New(koanfDecode).Names())

	reg := registry.New(koanfDecode)
	for _, name := range []string{"stdio", "http", "sse"} {
		require.NoError(t, reg.Register(name, newStdio))
	}
	assert.Equal(t, []string{"http", "sse", "stdio"}, reg.Names())
}
