package lint

import (
	"testing"

	"github.com/golangci/plugin-module-register/register"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginIsRegistered checks the wiring golangci-lint depends on. The
// package's init registers under [Name]; nothing in goga imports the plugin for
// its own sake, so without this test a rename or a dropped init would be caught
// only by whoever next built the custom binary.
func TestPluginIsRegistered(t *testing.T) {
	t.Parallel()

	newPlugin, err := register.GetPlugin(Name)
	require.NoError(t, err, "the plugin must be registered under %q", Name)

	built, err := newPlugin(nil)
	require.NoError(t, err)
	assert.Equal(t, register.LoadModeSyntax, built.GetLoadMode(),
		"these analyzers read the package path only; needing type info would be a design change")

	analyzers, err := built.BuildAnalyzers()
	require.NoError(t, err)
	require.Len(t, analyzers, 1,
		"M0 ships exactly one worked analyzer; a second one arrives with the milestone that owns it (D18)")
	assert.Equal(t, "gogalayout", analyzers[0].Name)
	assert.NotEmpty(t, analyzers[0].Doc, "every analyzer documents the rule and its reason")
}

func TestNewSettings(t *testing.T) {
	t.Parallel()

	t.Run("nil settings fall back to the default prefix", func(t *testing.T) {
		t.Parallel()

		analyzers, err := New(nil)
		require.NoError(t, err)
		require.Len(t, analyzers, 1)
		assert.Equal(t, DefaultModulePrefix,
			analyzers[0].Flags.Lookup("module-prefix").Value.String())
	})

	t.Run("a configured prefix reaches the analyzer", func(t *testing.T) {
		t.Parallel()

		analyzers, err := New(map[string]any{"module-prefix": "example.com/adopter"})
		require.NoError(t, err)
		require.Len(t, analyzers, 1)
		assert.Equal(t, "example.com/adopter",
			analyzers[0].Flags.Lookup("module-prefix").Value.String())
	})

	// The trap this guards: golangci-lint silently ignores unknown keys in its
	// settings blocks, so a misconfigured linter reports zero issues and looks
	// healthy. register.DecodeSettings rejects unknown fields, which turns a
	// typo into a build failure instead of a rule that quietly never runs.
	t.Run("an unknown key is an error, not a silent no-op", func(t *testing.T) {
		t.Parallel()

		_, err := New(map[string]any{"modulePrefix": "example.com/typo"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "goga/lint")
	})
}
