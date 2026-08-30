package lint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

// TestGogaLayout runs the analyzer over the fixture tree in testdata/src.
//
// The clean packages carry no `// want` comment, which is the half of the test
// that matters most: analysistest fails on an *unexpected* diagnostic as well
// as a missing one, so a rule that started firing on everything would fail here
// rather than pass quietly. A rule with only violating fixtures has never been
// shown to be a rule at all.
func TestGogaLayout(t *testing.T) {
	t.Parallel()

	results := analysistest.Run(t, analysistest.TestData(), NewLayoutAnalyzer(""),
		// Violating: goga's own code under internal/ and under pkg/.
		"github.com/gaarutyunov/goga/internal/foo",
		"github.com/gaarutyunov/goga/pkg/bar",
		// Clean: a top-level module, two directories whose names merely start
		// with a banned word, and a dependency's genuine internal/ package.
		"github.com/gaarutyunov/goga/config",
		"github.com/gaarutyunov/goga/internalstuff",
		"github.com/gaarutyunov/goga/registry/pkgfmt",
		"example.com/dep/internal/baz",
	)
	require.Len(t, results, 6, "every fixture package should have been loaded and analyzed")
}

// TestOffendingSegment pins the predicate itself. analysistest proves the
// analyzer is wired up; this proves the rule's edges, including the ones no
// fixture tree can express cheaply — a different module prefix, and the module
// root.
func TestOffendingSegment(t *testing.T) {
	t.Parallel()

	const otherPrefix = "example.com/adopter"

	tests := map[string]struct {
		prefix  string
		pkgPath string
		want    string
	}{
		"internal under the module": {
			pkgPath: "github.com/gaarutyunov/goga/internal/foo",
			want:    "internal",
		},
		"pkg under the module": {
			pkgPath: "github.com/gaarutyunov/goga/pkg/bar",
			want:    "pkg",
		},
		"nested internal under the module": {
			pkgPath: "github.com/gaarutyunov/goga/serve/internal/mux",
			want:    "internal",
		},
		"first banned segment wins": {
			pkgPath: "github.com/gaarutyunov/goga/pkg/x/internal/y",
			want:    "pkg",
		},
		"top-level module is clean": {
			pkgPath: "github.com/gaarutyunov/goga/config",
		},
		"module root is clean": {
			pkgPath: "github.com/gaarutyunov/goga",
		},
		"banned word as a prefix is not a segment": {
			pkgPath: "github.com/gaarutyunov/goga/internalstuff",
		},
		"banned word as a suffix is not a segment": {
			pkgPath: "github.com/gaarutyunov/goga/mypkg",
		},
		"a dependency's internal is not ours to judge": {
			pkgPath: "golang.org/x/tools/internal/gcimporter",
		},
		"a module whose path merely starts with the prefix": {
			// The trailing slash in the prefix match is what keeps this clean:
			// goga-extras is a different module, not a package inside goga.
			pkgPath: "github.com/gaarutyunov/goga-extras/internal/foo",
		},
		"testdata shields a fixture tree": {
			pkgPath: "github.com/gaarutyunov/goga/lint/testdata/src/x/internal/y",
		},
		"vendor shields a vendored dependency": {
			pkgPath: "github.com/gaarutyunov/goga/vendor/example.com/d/internal/y",
		},
		"a configured prefix moves the rule to another module": {
			prefix:  otherPrefix,
			pkgPath: otherPrefix + "/internal/foo",
			want:    "internal",
		},
		"a configured prefix stops the rule firing on goga": {
			prefix:  otherPrefix,
			pkgPath: "github.com/gaarutyunov/goga/internal/foo",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefix := test.prefix
			if prefix == "" {
				prefix = DefaultModulePrefix
			}
			checker := &layoutChecker{modulePrefix: prefix}

			assert.Equal(t, test.want, checker.offendingSegment(test.pkgPath))
		})
	}
}

// TestNewLayoutAnalyzerPrefixWiring covers the two ways the prefix is set: the
// constructor argument the golangci-lint plugin uses, and the -module-prefix
// flag that singlechecker, go vet and analysistest use. Both must reach the
// same field.
func TestNewLayoutAnalyzerPrefixWiring(t *testing.T) {
	t.Parallel()

	analyzer, checker := newLayoutAnalyzer("")
	assert.Equal(t, DefaultModulePrefix, checker.modulePrefix,
		"an empty constructor argument must fall back to the default prefix")

	flag := analyzer.Flags.Lookup("module-prefix")
	require.NotNil(t, flag, "the -module-prefix flag must be registered")
	assert.Equal(t, DefaultModulePrefix, flag.DefValue)

	require.NoError(t, analyzer.Flags.Set("module-prefix", "example.com/adopter"))
	assert.Equal(t, "example.com/adopter", checker.modulePrefix,
		"setting the flag must reach the field the constructor argument writes")

	_, explicit := newLayoutAnalyzer("example.com/other")
	assert.Equal(t, "example.com/other", explicit.modulePrefix)
}
