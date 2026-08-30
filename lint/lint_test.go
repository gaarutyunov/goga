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
	// Not because any analyzer reads pass.TypesInfo, but because all of them are
	// scoped by pass.Pkg.Path() and golangci-lint leaves pass.Pkg nil under
	// LoadModeSyntax — which makes every rule here report nothing at all. See
	// plugin.GetLoadMode; dropping back to syntax mode silently disables the
	// whole plugin, so it is pinned here rather than left to be rediscovered.
	assert.Equal(t, register.LoadModeTypesInfo, built.GetLoadMode(),
		"LoadModeSyntax leaves pass.Pkg nil, which disables every analyzer in this plugin")

	analyzers, err := built.BuildAnalyzers()
	require.NoError(t, err)

	// One analyzer per milestone that has landed: gogalayout with M0,
	// gogasemconv with M1, gogaserve with M2, gogaconfig with M3, gogamcp with
	// M6. The list is asserted in full
	// rather than by length so that adding a rule ahead of the package it
	// governs — the thing D18 forbids — has to be a deliberate edit here.
	names := make([]string, 0, len(analyzers))
	for _, analyzer := range analyzers {
		names = append(names, analyzer.Name)
		assert.NotEmpty(t, analyzer.Doc, "every analyzer documents the rule and its reason")
	}
	assert.Equal(t, []string{"gogalayout", "gogasemconv", "gogaserve", "gogaconfig", "gogamcp"}, names)
}

func TestNewSettings(t *testing.T) {
	t.Parallel()

	t.Run("nil settings fall back to the default prefix", func(t *testing.T) {
		t.Parallel()

		analyzers, err := New(nil)
		require.NoError(t, err)
		require.NotEmpty(t, analyzers)
		for _, analyzer := range analyzers {
			assert.Equal(t, DefaultModulePrefix,
				analyzer.Flags.Lookup("module-prefix").Value.String(),
				"%s must fall back to the default prefix", analyzer.Name)
		}
	})

	t.Run("a configured prefix reaches the analyzer", func(t *testing.T) {
		t.Parallel()

		analyzers, err := New(map[string]any{"module-prefix": "example.com/adopter"})
		require.NoError(t, err)
		require.NotEmpty(t, analyzers)
		for _, analyzer := range analyzers {
			assert.Equal(t, "example.com/adopter",
				analyzer.Flags.Lookup("module-prefix").Value.String(),
				"the configured prefix must reach %s", analyzer.Name)
		}
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
