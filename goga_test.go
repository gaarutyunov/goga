package goga_test

import (
	"errors"
	"testing"

	"github.com/gaarutyunov/goga"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settings is unexported here on purpose: it stands in for a module's private
// settings type, which is the whole reason Option is parameterised over S.
type settings struct {
	addr  string
	retry int
	trace []string
}

func withAddr(a string) goga.Option[settings] {
	return func(s *settings) error {
		s.addr = a
		s.trace = append(s.trace, "addr="+a)
		return nil
	}
}

func withRetry(n int) goga.Option[settings] {
	return func(s *settings) error {
		s.retry = n
		return nil
	}
}

var errBadAddr = errors.New("addr must not be empty")

func failing(err error) goga.Option[settings] {
	return func(*settings) error { return err }
}

func TestApply(t *testing.T) {
	defaults := settings{addr: "localhost:0", retry: 1}

	tests := []struct {
		name string
		opts []goga.Option[settings]
		want settings
	}{
		{
			name: "no options returns the defaults unchanged",
			opts: nil,
			want: defaults,
		},
		{
			name: "empty variadic slice returns the defaults unchanged",
			opts: []goga.Option[settings]{},
			want: defaults,
		},
		{
			name: "a single option is applied",
			opts: []goga.Option[settings]{withRetry(5)},
			want: settings{addr: "localhost:0", retry: 5},
		},
		{
			name: "options are applied in order, so the last write wins",
			opts: []goga.Option[settings]{withAddr("a:1"), withAddr("b:2"), withAddr("c:3")},
			want: settings{
				addr:  "c:3",
				retry: 1,
				trace: []string{"addr=a:1", "addr=b:2", "addr=c:3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := goga.Apply(defaults, tt.opts...)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyDoesNotMutateTheCallersDefaults(t *testing.T) {
	defaults := settings{addr: "localhost:0", retry: 1}

	_, err := goga.Apply(defaults, withAddr("other:9"), withRetry(7))
	require.NoError(t, err)

	assert.Equal(t, settings{addr: "localhost:0", retry: 1}, defaults,
		"Apply takes defaults by value; the caller's copy must be untouched")
}

func TestApplyWrapsAnOptionError(t *testing.T) {
	_, err := goga.Apply(settings{}, withAddr("a:1"), failing(errBadAddr))

	require.Error(t, err)
	assert.EqualError(t, err, "goga: applying option: addr must not be empty")
	assert.ErrorIs(t, err, errBadAddr, "the wrapped error must unwrap to the original")
}

func TestApplyStopsAtTheFirstFailingOption(t *testing.T) {
	var reached bool
	after := goga.Option[settings](func(*settings) error {
		reached = true
		return nil
	})

	_, err := goga.Apply(settings{}, failing(errBadAddr), after)

	require.ErrorIs(t, err, errBadAddr)
	assert.False(t, reached, "options after the first failure must not run")
}

func TestApplyIsGenericOverUnrelatedSettingsTypes(t *testing.T) {
	type other struct{ n int }

	got, err := goga.Apply(other{n: 1}, func(o *other) error {
		o.n *= 10
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, other{n: 10}, got)
}
