package config

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga"
)

// TestSourcesAreEmittedInHouseOrder pins the mechanism behind item 3.1 one
// level below [config.Load]: whatever order the options arrive in,
// [settings.sourcesInHouseOrder] emits defaults, then files, then environment,
// then flags.
//
// The external test asserts the observable consequence — the same value comes
// out whatever order the options went in. This one asserts the cause, so that
// a change that breaks the ordering is reported as an ordering failure rather
// than as a wrong string three tests later.
func TestSourcesAreEmittedInHouseOrder(t *testing.T) {
	t.Parallel()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	// Deliberately the reverse of the house order, plus a second file, so that
	// the within-a-kind order is asserted at the same time.
	opts := []Option{
		WithFlags(flags),
		WithEnv("A"),
		WithFile("base.yaml"),
		WithDefaults(map[string]any{"addr": ":8080"}),
		WithFile("overlay.yaml"),
	}

	set, err := goga.Apply(defaults(), opts...)
	require.NoError(t, err)

	sources := set.sourcesInHouseOrder()

	kinds := make([]sourceKind, 0, len(sources))
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		kinds = append(kinds, s.kind)
		names = append(names, s.name)
	}

	assert.Equal(t,
		[]sourceKind{kindDefaults, kindFile, kindFile, kindEnv, kindFlags},
		kinds,
		"the kinds come out in house order regardless of the order the options came in")
	assert.Equal(t,
		[]string{"defaults", "file:base.yaml", "file:overlay.yaml", "env:A__", "flags"},
		names,
		"and within one kind, in the order the options were passed")
}

// TestEnvTransform pins the convention [WithEnv] documents, at the function
// that implements it, including the cases a whole Load cannot reach cheaply.
func TestEnvTransform(t *testing.T) {
	t.Parallel()

	source := envSource{prefix: "GOGA" + envSeparator}

	tests := map[string]struct {
		in   string
		want string
	}{
		"a single segment":               {"GOGA__ADDR", "addr"},
		"two segments":                   {"GOGA__DATABASE__DSN", "database.dsn"},
		"an underscore inside a segment": {"GOGA__DATABASE__MAX_CONNS", "database.max_conns"},
		"three segments":                 {"GOGA__A__B__C", "a.b.c"},
		"already lower case":             {"GOGA__addr", "addr"},

		// Dropped: not this prefix, or nothing after it.
		"another prefix": {"OTHER__ADDR", ""},
		"a longer name sharing the prefix without the separator": {"GOGAX__ADDR", ""},
		"the bare prefix": {"GOGA__", ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			key, value := source.transform(tt.in, "v")

			assert.Equal(t, tt.want, key)
			if tt.want == "" {
				assert.Nil(t, value, "a dropped variable carries no value")
			} else {
				assert.Equal(t, "v", value)
			}
		})
	}
}

// TestFlagKeyMatchesTheEnvConvention is the other half of the key convention: a
// flag and an environment variable must name the same setting, or a project
// ends up with two spellings of every key and a table mapping one to the other.
func TestFlagKeyMatchesTheEnvConvention(t *testing.T) {
	t.Parallel()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("addr", "", "")
	flags.String("database.max-conns", "", "")
	flags.String("log-level", "", "")

	key := flagKey(flags)

	got := map[string]string{}
	flags.VisitAll(func(f *pflag.Flag) {
		k, _ := key(f)
		got[f.Name] = k
	})

	assert.Equal(t, map[string]string{
		"addr":               "addr",
		"database.max-conns": "database.max_conns",
		"log-level":          "log_level",
	}, got)
}

// TestParentPaths pins the helper the collision guard walks.
func TestParentPaths(t *testing.T) {
	t.Parallel()

	assert.Nil(t, parentPaths("addr"))
	assert.Equal(t, []string{"a"}, parentPaths("a.b"))
	assert.Equal(t, []string{"a", "a.b"}, parentPaths("a.b.c"))
}

// TestKeyGuardAcceptsWhatItShould proves the guard is not simply refusing
// everything: an override of the same key, and two sibling keys under one
// parent, are both legal merges.
func TestKeyGuardAcceptsWhatItShould(t *testing.T) {
	t.Parallel()

	guard := newKeyGuard()

	require.NoError(t, guard.add("defaults", map[string]any{
		"database": map[string]any{"dsn": "a", "max_conns": 1},
	}))
	require.NoError(t, guard.add("file:app.yaml", map[string]any{
		"database": map[string]any{"dsn": "b"},
	}))
	require.NoError(t, guard.add("env:X__", map[string]any{
		"database": map[string]any{"pool": map[string]any{"size": 3}},
	}))
	assert.NoError(t, guard.add("flags", map[string]any{"addr": ":8080"}))
}
