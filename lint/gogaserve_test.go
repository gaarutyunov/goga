package lint

import (
	"go/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/packages"
)

// TestGogaServe runs the analyzer over the fixture tree in testdata/src.
//
// Most of the packages listed carry no `want` comment, and that majority is the
// point: analysistest fails on an unexpected diagnostic as well as on a missing
// one, so a rule that had quietly become "report every mention of net/http"
// would fail here rather than pass. The clean packages cover each way this rule
// could be wrong — the package that owns the listener, its driver sub-package,
// httptest, the *http.Server type named as the As escape hatch, and another
// module entirely.
//
// The two lookalikes are the pair that decides how the exemption is written.
// serveutil must be reported because its name merely starts with the owner's;
// goga/httpx/serve must be reported because its last segment is "serve" but its
// position is not. A segment match would silence both.
func TestGogaServe(t *testing.T) {
	t.Parallel()

	analysistest.Run(t, analysistest.TestData(), NewServeAnalyzer(""),
		// Violating: goga's own code building the listener itself, through the
		// plain import, through an alias, and in a test file.
		"github.com/gaarutyunov/goga/bypass",
		// Violating: the two packages the exemption must not reach.
		"github.com/gaarutyunov/goga/serveutil",
		"github.com/gaarutyunov/goga/httpx/serve",
		// Clean: the package that owns the listener, and its driver.
		"github.com/gaarutyunov/goga/serve",
		"github.com/gaarutyunov/goga/serve/driver",
		// Clean: correct usage and every near miss.
		"github.com/gaarutyunov/goga/adopter",
		// Clean: a dependency, whose listener is not goga's to police.
		"example.com/dep/httpsrv",
	)
}

// TestServeTriggersExistInNetHTTP is the guard that keeps the rule's trigger
// set honest.
//
// Both halves of the set are strings matched against a selector, so a typo —
// or an upstream rename — produces a rule that still compiles, still passes
// every fixture it was given, and simply stops covering the case it names. That
// is the same failure mode as golangci-lint's unknown-key trap: a check that
// looks healthy and guards less than it claims. So each name is resolved
// against the real net/http here, and a name that no longer refers to what the
// analyzer assumes fails.
func TestServeTriggersExistInNetHTTP(t *testing.T) {
	t.Parallel()

	scope := loadNetHTTPScope(t)

	for name := range serveListenFuncs {
		object := scope.Lookup(name)
		require.NotNil(t, object, "net/http no longer declares %s; the rule silently stopped covering it", name)
		function, ok := object.(*types.Func)
		require.True(t, ok, "net/http.%s is no longer a function", name)
		signature, ok := function.Type().(*types.Signature)
		require.True(t, ok)
		assert.Equal(t, 1, signature.Results().Len(),
			"net/http.%s no longer returns the error-only result the rule's reasoning rests on", name)
	}

	object := scope.Lookup(serveTypeName)
	require.NotNil(t, object, "net/http no longer declares %s", serveTypeName)
	_, ok := object.(*types.TypeName)
	assert.True(t, ok, "net/http.%s is no longer a type, so a composite literal of it means something else", serveTypeName)
}

// TestServeSkipPackage pins the rule's scope, including the edges no fixture
// tree can express cheaply: a configured prefix, the module root, and the
// subtrees the go tool never loads.
func TestServeSkipPackage(t *testing.T) {
	t.Parallel()

	const otherPrefix = "example.com/adopter"

	tests := map[string]struct {
		prefix  string
		pkgPath string
		want    bool
	}{
		"a goga package is checked":                  {pkgPath: "github.com/gaarutyunov/goga/registry"},
		"the module root is checked":                 {pkgPath: "github.com/gaarutyunov/goga"},
		"the owner package is skipped":               {pkgPath: "github.com/gaarutyunov/goga/serve", want: true},
		"the driver under the owner is skipped":      {pkgPath: "github.com/gaarutyunov/goga/serve/driver", want: true},
		"the conformance suite is skipped":           {pkgPath: "github.com/gaarutyunov/goga/serve/servetest", want: true},
		"a package merely named like it is checked":  {pkgPath: "github.com/gaarutyunov/goga/serveutil"},
		"a serve package elsewhere is checked":       {pkgPath: "github.com/gaarutyunov/goga/httpx/serve"},
		"a fixture tree is skipped":                  {pkgPath: "github.com/gaarutyunov/goga/lint/testdata/src/x", want: true},
		"a vendored dependency is skipped":           {pkgPath: "github.com/gaarutyunov/goga/vendor/example.com/d", want: true},
		"another module is skipped":                  {pkgPath: "net/http", want: true},
		"a module sharing the prefix is skipped":     {pkgPath: "github.com/gaarutyunov/goga-extras/serve", want: true},
		"a configured prefix moves the rule":         {prefix: otherPrefix, pkgPath: otherPrefix + "/api"},
		"a configured prefix moves the exemption":    {prefix: otherPrefix, pkgPath: otherPrefix + "/serve", want: true},
		"a configured prefix stops it firing here":   {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/api", want: true},
		"goga's own serve is checked under a prefix": {prefix: otherPrefix, pkgPath: "github.com/gaarutyunov/goga/serve", want: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefix := test.prefix
			if prefix == "" {
				prefix = DefaultModulePrefix
			}
			checker := &serveChecker{modulePrefix: prefix}

			assert.Equal(t, test.want, checker.skipPackage(test.pkgPath))
		})
	}
}

// TestNewServeAnalyzerPrefixWiring covers the two ways the prefix is set: the
// constructor argument the golangci-lint plugin uses, and the -module-prefix
// flag that singlechecker, go vet and analysistest use. Both must reach the
// same field, or configuring the rule through one path silently does nothing
// through the other.
func TestNewServeAnalyzerPrefixWiring(t *testing.T) {
	t.Parallel()

	analyzer, checker := newServeAnalyzer("")
	assert.Equal(t, DefaultModulePrefix, checker.modulePrefix)

	flag := analyzer.Flags.Lookup("module-prefix")
	require.NotNil(t, flag, "the -module-prefix flag must be registered")
	assert.Equal(t, DefaultModulePrefix, flag.DefValue)

	require.NoError(t, analyzer.Flags.Set("module-prefix", "example.com/adopter"))
	assert.Equal(t, "example.com/adopter", checker.modulePrefix)

	_, explicit := newServeAnalyzer("example.com/other")
	assert.Equal(t, "example.com/other", explicit.modulePrefix)
}

// loadNetHTTPScope type-checks net/http and returns its package scope.
func loadNetHTTPScope(t *testing.T) *types.Scope {
	t.Helper()

	loaded, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
	}, netHTTPImportPath)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Empty(t, loaded[0].Errors, "net/http must type-check for this guard to mean anything")

	return loaded[0].Types.Scope()
}
