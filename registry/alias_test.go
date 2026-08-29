package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gaarutyunov/goga/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file demonstrates the pattern that makes an adapter's settings type
// private without making its handle unusable.
//
// pgxSettings is unexported here, standing in for an adapter module that keeps
// its configuration struct to itself. PgxAdapter is an exported alias of the
// instantiated handle. Downstream code can name PgxAdapter — declare variables
// and parameters of that type, store it in a struct field, return it from a
// constructor — without ever being able to write pgxSettings, because
// [registry.Registry.Provide] infers both type parameters from the constructor
// and the alias carries them.

type pgxSettings struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

type fakePool struct {
	dsn      string
	maxConns int
}

func newPool(_ context.Context, s pgxSettings) (*fakePool, error) {
	if s.DSN == "" {
		return nil, errors.New("dsn is required")
	}
	return &fakePool{dsn: s.DSN, maxConns: s.MaxConns}, nil
}

// PgxAdapter is the nameable handle. Note that the port here is a concrete
// *fakePool rather than an interface — the registry does not require the port
// to be an interface type.
type PgxAdapter = registry.Adapter[*fakePool, pgxSettings]

// WithMaxConns is the adapter's exported option. Its type mentions pgxSettings,
// but a caller writes only the function name.
func WithMaxConns(n int) registry.Option[pgxSettings] {
	return func(s *pgxSettings) error {
		if n <= 0 {
			return errors.New("max_conns must be positive")
		}
		s.MaxConns = n
		return nil
	}
}

// openViaAlias is the downstream consumer. Its signature names PgxAdapter and
// nothing else; pgxSettings appears nowhere in it.
func openViaAlias(ctx context.Context, a PgxAdapter, raw registry.Settings) (*fakePool, error) {
	return a.Open(ctx, raw, WithMaxConns(16))
}

func TestExportedAliasIsNameableWithoutNamingSettings(t *testing.T) {
	reg := registry.New(koanfDecode)

	// Both type parameters are inferred from newPool; the caller writes none.
	pgx, err := reg.Provide("pgx", newPool)
	require.NoError(t, err)

	// The handle assigns to the alias, which is the same type.
	var handle PgxAdapter = pgx
	assert.Equal(t, "pgx", handle.Name())

	pool, err := openViaAlias(t.Context(), handle, registry.Settings{"dsn": "postgres://x"})
	require.NoError(t, err)
	assert.Equal(t, "postgres://x", pool.dsn)
	assert.Equal(t, 16, pool.maxConns, "the option applied on top of the config")
}

func TestAliasHandleReportsItsOwnOptionErrors(t *testing.T) {
	reg := registry.New(koanfDecode)
	pgx, err := reg.Provide("pgx", newPool)
	require.NoError(t, err)

	_, err = pgx.Open(t.Context(), registry.Settings{"dsn": "postgres://x"}, WithMaxConns(0))
	require.Error(t, err)
	assert.EqualError(t, err, "goga: applying option: max_conns must be positive")
}

func TestConcretePortIsCheckedLikeAnInterfacePort(t *testing.T) {
	reg := registry.New(koanfDecode)
	_, err := reg.Provide("pgx", newPool)
	require.NoError(t, err)

	_, err = reg.Open[*httpTransport](t.Context(), "pgx", registry.Settings{"dsn": "postgres://x"})
	assert.ErrorIs(t, err, registry.ErrPortMismatch)

	pool, err := reg.Open[*fakePool](t.Context(), "pgx", registry.Settings{"dsn": "postgres://x", "max_conns": 4})
	require.NoError(t, err)
	assert.Equal(t, 4, pool.maxConns)
}
