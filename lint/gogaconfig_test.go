package lint

import (
	"go/types"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/packages"
)

// TestGogaConfig runs the analyzer over the fixture tree in testdata/src.
//
// Most of the packages listed carry no `want` comment, and that majority is the
// point: analysistest fails on an unexpected diagnostic as well as on a missing
// one, so a rule that had quietly become "report every mention of os" would fail
// here rather than pass. The clean packages cover each way this rule could be
// wrong — the package that owns the environment source, a command, a test file,
// every other os function, and another module entirely.
//
// The two lookalikes are the pair that decides how the owner exemption is
// written. configutil must be reported because its name merely starts with the
// owner's; goga/api/config must be reported because its last segment is
// "config" but its position is not. A segment match would silence both.
func TestGogaConfig(t *testing.T) {
	t.Parallel()

	results := analysistest.Run(t, analysistest.TestData(), NewConfigAnalyzer(""),
		// Violating: goga's own library code reading the environment itself,
		// through the plain import and through an alias. Its _test.go file is
		// in the same package and is deliberately clean.
		"github.com/gaarutyunov/goga/envread",
		// Violating: the two packages the owner exemption must not reach.
		"github.com/gaarutyunov/goga/configutil",
		"github.com/gaarutyunov/goga/api/config",
		// Clean: the package that owns the environment source.
		"github.com/gaarutyunov/goga/config",
		// Clean: a command, which has to read the environment to bootstrap.
		"github.com/gaarutyunov/goga/envmain",
		// Clean: correct usage and every near miss.
		"github.com/gaarutyunov/goga/envclean",
		// Clean: a dependency, whose environment reads are not goga's to
		// police.
		"example.com/dep/envdep",
	)
	// Nine actions, not seven patterns: analysistest loads with tests enabled,
	// so envread — the one fixture with an in-package _test.go — yields three
	// (the package without its test file, the package with it, and the
	// generated test main, which is a package main and therefore exempt).
	require.Len(t, results, 9, "every fixture package should have been loaded and analyzed")
}

// TestConfigFixtureAnalyzesTestFiles is the guard on a guard.
//
// The _test.go exemption is proven by envread/envread_test.go staying silent —
// but "silent" is also exactly what a fixture the harness never loaded looks
// like, and a fixture nobody loads proves nothing at all. So this asserts the
// file really is among the ones the analyzer was handed: analysistest loads
// each pattern with tests enabled, which is why envread yields three actions
// (the package without its test file, the package with it, and the generated
// test main), and the middle one is where the exemption does its work.
func TestConfigFixtureAnalyzesTestFiles(t *testing.T) {
	t.Parallel()

	results := analysistest.Run(t, analysistest.TestData(), NewConfigAnalyzer(""),
		"github.com/gaarutyunov/goga/envread")

	var analyzed bool
	for _, result := range results {
		if result.Pass == nil {
			continue
		}
		for _, file := range result.Pass.Files {
			if filepath.Base(result.Pass.Fset.Position(file.Pos()).Filename) == "envread_test.go" {
				analyzed = true
			}
		}
	}

	assert.True(t, analyzed,
		"the harness must hand the analyzer the fixture's test file, or the exemption that file proves is proven by nothing")
}

// TestConfigTriggersExistInOs is the guard that keeps the rule's trigger set
// honest.
//
// Every name in the set is a string matched against a selector, so a typo — or
// an upstream rename — produces a rule that still compiles, still passes every
// fixture it was given, and simply stops covering the case it names. That is the
// same failure mode as golangci-lint's unknown-key trap: a check that looks
// healthy and guards less than it claims. So each name is resolved against the
// real os here, and a name that no longer refers to a function fails.
func TestConfigTriggersExistInOs(t *testing.T) {
	t.Parallel()

	scope := loadOsScope(t)

	for name := range configEnvReaders {
		object := scope.Lookup(name)
		require.NotNil(t, object, "os no longer declares %s; the rule silently stopped covering it", name)
		_, ok := object.(*types.Func)
		assert.True(t, ok, "os.%s is no longer a function", name)
	}

	// The deliberate absences, asserted against the set rather than against os:
	// all three exist upstream, and what this pins is that they were considered
	// and declined. Without it, adding one by reflex looks like tightening the
	// rule rather than reversing a decision.
	for _, name := range []string{"ExpandEnv", "Setenv", "Unsetenv"} {
		require.NotNil(t, scope.Lookup(name), "os no longer declares %s", name)
		assert.False(t, configEnvReaders[name],
			"os.%s is deliberately outside the trigger set; see configEnvReaders for the argument", name)
	}
}

// TestConfigSkipPackage pins the rule's scope, including the edges no fixture
// tree can express cheaply: a configured prefix, the module root, and the
// subtrees the go tool never loads.
func TestConfigSkipPackage(t *testing.T) {
	t.Parallel()

	const otherPrefix = "example.com/adopter"

	tests := map[string]struct {
		prefix  string
		pkgPath string
		want    bool
	}{
		"a goga package is checked":                   {pkgPath: "github.com/gaarutyunov/goga/serve"},
		"the module root is checked":                  {pkgPath: "github.com/gaarutyunov/goga"},
		"the owner package is skipped":                {pkgPath: "github.com/gaarutyunov/goga/config", want: true},
		"a package under the owner is skipped":        {pkgPath: "github.com/gaarutyunov/goga/config/provider", want: true},
		"a package merely named like it is checked":   {pkgPath: "github.com/gaarutyunov/goga/configutil"},
		"a config package elsewhere is checked":       {pkgPath: "github.com/gaarutyunov/goga/api/config"},
		"a fixture tree is skipped":                   {pkgPath: "github.com/gaarutyunov/goga/lint/testdata/src/x", want: true},
		"a vendored dependency is skipped":            {pkgPath: "github.com/gaarutyunov/goga/vendor/example.com/d", want: true},
		"another module is skipped":                   {pkgPath: "os", want: true},
		"a module sharing the prefix is skipped":      {pkgPath: "github.com/gaarutyunov/goga-extras/config", want: true},
		"a configured prefix moves the rule":          {prefix: otherPrefix, pkgPath: otherPrefix + "/api"},
		"a configured prefix moves the exemption":     {prefix: otherPrefix, pkgPath: otherPrefix + "/config", want: true},
		"a configured prefix stops it firing here":    {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/api", want: true},
		"goga's own config is checked under a prefix": {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/config", want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefix := test.prefix
			if prefix == "" {
				prefix = DefaultModulePrefix
			}
			checker := &configChecker{modulePrefix: prefix}

			assert.Equal(t, test.want, checker.skipPackage(test.pkgPath))
		})
	}
}

// TestNewConfigAnalyzerPrefixWiring covers the two ways the prefix is set: the
// constructor argument the golangci-lint plugin uses, and the -module-prefix
// flag that singlechecker, go vet and analysistest use. Both must reach the
// same field, or configuring the rule through one path silently does nothing
// through the other.
func TestNewConfigAnalyzerPrefixWiring(t *testing.T) {
	t.Parallel()

	analyzer, checker := newConfigAnalyzer("")
	assert.Equal(t, DefaultModulePrefix, checker.modulePrefix)

	flag := analyzer.Flags.Lookup("module-prefix")
	require.NotNil(t, flag, "the -module-prefix flag must be registered")
	assert.Equal(t, DefaultModulePrefix, flag.DefValue)

	require.NoError(t, analyzer.Flags.Set("module-prefix", "example.com/adopter"))
	assert.Equal(t, "example.com/adopter", checker.modulePrefix)

	_, explicit := newConfigAnalyzer("example.com/other")
	assert.Equal(t, "example.com/other", explicit.modulePrefix)
}

// loadOsScope type-checks os and returns its package scope.
func loadOsScope(t *testing.T) *types.Scope {
	t.Helper()

	loaded, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
	}, osImportPath)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Empty(t, loaded[0].Errors, "os must type-check for this guard to mean anything")

	return loaded[0].Types.Scope()
}
